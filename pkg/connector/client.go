package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

// ErrNeedsLogin means the stored session and password no longer work: the user signs in again.
var ErrNeedsLogin = errors.New("edupage needs a new sign-in")

type EduPageClient struct {
	Main      *EduPageConnector
	UserLogin *bridgev2.UserLogin

	mu  sync.Mutex // guards edu and serializes signing in
	edu *edupage.Client

	stop     context.CancelFunc
	wg       sync.WaitGroup
	names    sync.Map // ghost ID → name seen on EduPage
	groups   sync.Map // group portal ID → name
	teachers sync.Map // ghost ID → edupage.Contact, the people the user can start a message to
	// tries: how many polls found an item still not in Matrix, by EduPage ID; only the poll goroutine
	// touches it.
	tries map[int64]int
	// roomsDown: the homeserver did not make a room in this poll, so the poll makes no more.
	roomsDown bool
	// roomFails: polls in a row in which a portal's room did not come because the homeserver did not answer.
	roomFails map[networkid.PortalKey]int
	// failedNow: portals whose room failed in this poll, and whether the homeserver answered; their other
	// items do not try again.
	failedNow map[networkid.PortalKey]bool
	// ensure replaces ensureRoom in tests.
	ensure func(context.Context, networkid.PortalKey) error
	// holdUnsaved: the place an outage began could not be saved yet, so the next failed check tries again.
	holdUnsaved bool
	// slowNow: chats past the cap whose room got no answer in this poll; after two, the others past the cap wait.
	slowNow int
	saveMu  sync.Mutex
}

var (
	_ bridgev2.NetworkAPI                    = (*EduPageClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*EduPageClient)(nil)
	_ bridgev2.GhostDMCreatingNetworkAPI     = (*EduPageClient)(nil)
	_ bridgev2.UserSearchingNetworkAPI       = (*EduPageClient)(nil)
	_ bridgev2.ContactListingNetworkAPI      = (*EduPageClient)(nil)
)

func (ec *EduPageClient) meta() *UserLoginMetadata { return ec.UserLogin.Metadata.(*UserLoginMetadata) }

func (ec *EduPageClient) save(ctx context.Context) error {
	ec.saveMu.Lock()
	defer ec.saveMu.Unlock()
	return ec.UserLogin.Save(ctx)
}

func (ec *EduPageClient) Connect(_ context.Context) {
	if ec.stop != nil {
		return
	}
	ctx, stop := context.WithCancel(ec.Main.Bridge.BackgroundCtx)
	ec.stop = stop
	ctx = ec.UserLogin.Log.WithContext(ctx)
	ec.wg.Add(1)
	go func() {
		defer ec.wg.Done()
		ec.pollLoop(ctx)
	}()
}

func (ec *EduPageClient) Disconnect() {
	if ec.stop != nil {
		ec.stop()
		ec.wg.Wait()
		ec.stop = nil
	}
}

func (ec *EduPageClient) IsLoggedIn() bool {
	m := ec.meta()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Password != "" || m.Session != ""
}

func (ec *EduPageClient) LogoutRemote(ctx context.Context) {
	ec.mu.Lock()
	edu := ec.edu
	ec.edu = nil
	ec.mu.Unlock()
	if edu != nil {
		edu.Logout(ctx)
	}
	m := ec.meta()
	m.mu.Lock()
	m.Password, m.Session = "", ""
	m.mu.Unlock()
}

func (ec *EduPageClient) ownID() networkid.UserID {
	m := ec.meta()
	m.mu.Lock()
	defer m.mu.Unlock()
	return makeUserID(m.UserID, "")
}

func (ec *EduPageClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return userID == ec.ownID()
}

// session is the signed-in EduPage client: the stored session if it still works, else a new sign-in
// with the stored password. A second factor at that point cannot be answered: ErrNeedsLogin.
func (ec *EduPageClient) session(ctx context.Context) (*edupage.Client, error) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	if ec.edu != nil {
		return ec.edu, nil
	}
	m := ec.meta()
	m.mu.Lock()
	school, session, username, password := m.School, m.Session, m.Username, m.Password
	m.mu.Unlock()
	c := edupage.New(ec.Main.Config.EduPageURL, 30*time.Second)
	if session != "" {
		if err := c.Resume(ctx, school, session, username); err == nil {
			ec.edu = c
			return c, nil
		} else if !errors.Is(err, edupage.ErrSessionExpired) {
			return nil, err
		}
	}
	if password == "" {
		return nil, ErrNeedsLogin
	}
	c = edupage.New(ec.Main.Config.EduPageURL, 30*time.Second)
	two, err := c.Login(ctx, username, password, school)
	switch {
	case errors.Is(err, edupage.ErrBadCredentials), errors.Is(err, edupage.ErrCaptcha):
		return nil, fmt.Errorf("%w: %v", ErrNeedsLogin, err)
	case err != nil:
		return nil, err
	case two != nil:
		return nil, fmt.Errorf("%w: edupage wants the sign-in confirmed", ErrNeedsLogin)
	}
	m.mu.Lock()
	m.Session = c.Session()
	m.mu.Unlock()
	ec.edu = c
	if err = ec.save(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// withSession runs fn with the session and, when EduPage answers with its login page, once more
// after signing in again.
func (ec *EduPageClient) withSession(ctx context.Context, fn func(*edupage.Client) error) error {
	c, err := ec.session(ctx)
	if err != nil {
		return err
	}
	if err = fn(c); !errors.Is(err, edupage.ErrSessionExpired) {
		return err
	}
	ec.mu.Lock()
	if ec.edu == c {
		ec.edu = nil
	}
	ec.mu.Unlock()
	m := ec.meta()
	m.mu.Lock()
	m.Session = ""
	m.mu.Unlock()
	if c, err = ec.session(ctx); err != nil {
		return err
	}
	return fn(c)
}

func (ec *EduPageClient) reportError(err error) {
	if errors.Is(err, ErrNeedsLogin) {
		ec.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "edupage-needs-login",
			Message:    "EduPage no longer accepts the stored sign-in, sign in again",
		})
		return
	}
	if errors.Is(err, errHomeserverAway) {
		ec.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      "homeserver-unreachable",
			Message:    "The homeserver is not answering; EduPage is read again once it does",
		})
		return
	}
	ec.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      "edupage-unreachable",
		Message:    "EduPage is not answering: " + strings.SplitN(err.Error(), "\n", 2)[0],
	})
}

func (ec *EduPageClient) portalKey(id networkid.PortalID) networkid.PortalKey {
	return networkid.PortalKey{ID: id, Receiver: ec.UserLogin.ID}
}

func (ec *EduPageClient) sender(owner, name string) bridgev2.EventSender {
	uid := makeUserID(owner, name)
	if name != "" {
		ec.names.Store(uid, name)
	}
	if uid == ec.ownID() {
		return bridgev2.EventSender{IsFromMe: true, SenderLogin: ec.UserLogin.ID, Sender: uid}
	}
	return bridgev2.EventSender{Sender: uid}
}

func (ec *EduPageClient) member(id networkid.UserID) bridgev2.ChatMember {
	if id == ec.ownID() {
		return bridgev2.ChatMember{EventSender: bridgev2.EventSender{IsFromMe: true, SenderLogin: ec.UserLogin.ID, Sender: id}, Membership: event.MembershipJoin}
	}
	return bridgev2.ChatMember{EventSender: bridgev2.EventSender{Sender: id}, Membership: event.MembershipJoin}
}

func (ec *EduPageClient) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	members := &bridgev2.ChatMemberList{MemberMap: make(bridgev2.ChatMemberMap)}
	members.MemberMap.Set(ec.member(ec.ownID()))
	info := &bridgev2.ChatInfo{Members: members}
	switch person, isPerson := portalPerson(portal.ID); {
	case portal.ID == schoolPortal:
		info.Type = ptr.Ptr(database.RoomTypeDM)
		info.Name = ptr.Ptr("EduPage")
		members.OtherUserID = schoolGhost
		members.MemberMap.Set(ec.member(schoolGhost))
	case isPerson:
		info.Type = ptr.Ptr(database.RoomTypeDM)
		members.OtherUserID = person
		members.MemberMap.Set(ec.member(person))
	default:
		info.Type = ptr.Ptr(database.RoomTypeGroupDM)
		name := strings.TrimPrefix(string(portal.ID), "g:")
		if n, ok := ec.groups.Load(portal.ID); ok {
			name = n.(string)
		}
		info.Name = ptr.Ptr(name)
	}
	return info, nil
}

func (ec *EduPageClient) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	if ghost.ID == schoolGhost {
		return &bridgev2.UserInfo{Name: ptr.Ptr("EduPage"), IsBot: ptr.Ptr(true)}, nil
	}
	if n, ok := ec.names.Load(ghost.ID); ok {
		return &bridgev2.UserInfo{Name: ptr.Ptr(n.(string)), IsBot: ptr.Ptr(false)}, nil
	}
	ec.mu.Lock()
	edu := ec.edu
	ec.mu.Unlock()
	if edu != nil {
		if name, ok := edu.Person(edupageID(ghost.ID)); ok {
			return &bridgev2.UserInfo{Name: ptr.Ptr(name), IsBot: ptr.Ptr(false)}, nil
		}
	}
	return nil, nil
}

func (ec *EduPageClient) GetCapabilities(_ context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	if portal.ID == schoolPortal {
		return noticeFeatures()
	}
	return roomFeatures(ec.Main.Config.MaxFileSize)
}

// errFiltered: bridge.portal_create_filter does not want this chat.
var errFiltered = errors.New("the portal create filter refuses this chat")

// ensureRoom makes a portal's Matrix room now, one after another from the poll. Left to bridgev2, rooms
// of several portals are made at once, and Tuwunel gives two rooms the bot creates in the same
// millisecond the same ID (room version 12 names a room by its create event). A refusal by the
// homeserver comes back as is; a homeserver that does not answer is a transient error that ends the poll.
func (ec *EduPageClient) ensureRoom(ctx context.Context, key networkid.PortalKey) error {
	portal, err := ec.Main.Bridge.GetPortalByKey(ctx, key)
	if err != nil {
		return fmt.Errorf("%w: %w", errTransient, err)
	} else if portal.MXID != "" {
		return nil
	} else if !ec.Main.Bridge.Config.PortalCreateFilter.ShouldAllow(ec.UserLogin.ID, key) {
		return errFiltered
	} else if ec.roomsDown {
		return errTransient
	}
	ctx, cancel := context.WithTimeout(ctx, roomTimeout)
	defer cancel()
	info, err := ec.GetChatInfo(ctx, portal)
	if err == nil {
		err = portal.CreateMatrixRoom(ctx, ec.UserLogin, info)
	}
	var httpErr mautrix.HTTPError
	switch {
	case err == nil, errors.Is(err, bridgev2.ErrPortalIsDeleted):
		return err
	case errors.As(err, &httpErr) && httpErr.Response != nil && !noAnswer(httpErr.Response.StatusCode):
		return err // the homeserver answered: it refused this room or failed on it
	default:
		return fmt.Errorf("%w: %w", errTransient, err)
	}
}

const roomTimeout = 30 * time.Second

var errHomeserverAway = errors.New("the homeserver does not answer")

// homeserverAnswers makes one small write as the bridge bot, without retries: a homeserver that reads but
// does not write is away too.
func (ec *EduPageClient) homeserverAnswers(ctx context.Context) error {
	mc, ok := ec.Main.Bridge.Matrix.(*matrix.Connector)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(mautrix.WithMaxRetries(ctx, 0), 10*time.Second)
	defer cancel()
	err := mc.AS.BotClient().SetAccountData(ctx, "io.github.gooder24.edupage.poll", map[string]int64{"at": time.Now().Unix()})
	var answer mautrix.HTTPError
	if errors.As(err, &answer) && answer.Response != nil && !noAnswer(answer.Response.StatusCode) {
		// The homeserver itself refused this write: it is there, and the poll's own writes show what is wrong.
		ec.UserLogin.Log.Warn().Err(err).Msg("The homeserver refused the poll's check")
		return nil
	}
	return err
}

// noAnswer: statuses that a proxy gives for a homeserver that is down or too busy, not the homeserver's
// own answer about one room.
func noAnswer(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout || status == http.StatusTooManyRequests ||
		status >= 520 && status <= 524 // a CDN in front of a homeserver that is down
}

var errTransient = errors.New("the homeserver did not make a room")
