package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

// A new message to classes, groups and people (EduPage's "New message" with several recipients) has
// a room of its own: its portal ID is "to:" and the recipients' ids in lower case, sorted and joined
// by ";". Every plain message there starts a new EduPage message to all of them; a reply answers in
// its thread. Rooms the poll made for messages to a class ("g:") never start one.
const komuPrefix = "to:"

// OptionsKey is the content key of a teacher's choices on a new message, as edupage.Options:
// {"important": true, "replies": "author"}. Others' messages ignore it.
const OptionsKey = "io.github.gooder24.edupage.options"

const groupType = "message"

func komuPortal(ids []string) networkid.PortalID {
	return networkid.PortalID(komuPrefix + strings.Join(ids, ";"))
}

func portalKomu(id networkid.PortalID) ([]string, bool) {
	rest, ok := strings.CutPrefix(string(id), komuPrefix)
	if !ok || rest == "" {
		return nil, false
	}
	return strings.Split(rest, ";"), true
}

// sendID is whom a person's chat writes to: a pupil alone, never also their parents.
func sendID(person networkid.UserID) string {
	id := edupageID(person)
	if rest, ok := strings.CutPrefix(id, "Student"); ok {
		return "StudentOnly" + rest
	}
	return id
}

// setSchool keeps the recipients the last poll found, for the contact list and search.
func (ec *EduPageClient) setSchool(list []edupage.Recipient) { ec.school.Store(&list) }

// recipients is what the account may write to: the last poll's list, else read from the session.
func (ec *EduPageClient) recipients(ctx context.Context) ([]edupage.Recipient, error) {
	if list := ec.school.Load(); list != nil {
		return *list, nil
	}
	c, err := ec.session(ctx)
	if err != nil {
		return nil, err
	}
	list := c.Recipients()
	ec.setSchool(list)
	return list, nil
}

func refused(err error) error {
	return bridgev2.WrapErrorInStatus(err).WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true).
		WithStatus(event.MessageStatusFail).WithErrorReason(event.MessageStatusNoPermission)
}

type notOffered []string

func (n notOffered) Error() string {
	return "EduPage does not offer " + strings.Join(n, ", ") + " to this account now; start the message again"
}

// pick checks ids (any case, an EduPage id or its ghost's) against what the account may write to and
// gives them in lower case, sorted and without repeats, with EduPage's spelling of each.
func pick(list []edupage.Recipient, ids []string) (lower, edu []string, err error) {
	offered := edupage.Offered(list)
	var missing notOffered
	for _, id := range ids {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, ok := offered[key]; !ok {
			missing = append(missing, id)
		} else if !slices.Contains(lower, key) {
			lower = append(lower, key)
		}
	}
	if len(missing) > 0 {
		return nil, nil, missing
	}
	if len(lower) == 0 {
		return nil, nil, errors.New("no recipient")
	}
	slices.Sort(lower)
	for _, key := range lower {
		edu = append(edu, offered[key])
	}
	return lower, edu, nil
}

// komuName names a message's room by its recipients, "7.A, Alex Pupil (with parents)".
func komuName(list []edupage.Recipient, lower []string) string {
	var names []string
	for _, key := range lower {
		for _, r := range list {
			if strings.ToLower(r.ID) == key {
				names = append(names, r.Name)
			} else if r.Parents != "" && strings.ToLower(r.Parents) == key {
				names = append(names, r.Name+" (with parents)")
			} else {
				continue
			}
			break
		}
	}
	return strings.Join(names, ", ")
}

// komuChat is the room of a new message to ids. One person whose chat writes to just that id gets
// their own chat instead.
func (ec *EduPageClient) komuChat(ctx context.Context, ids []string, name string) (*bridgev2.CreateChatResponse, error) {
	list, err := ec.recipients(ctx)
	if err != nil {
		return nil, err
	}
	lower, edu, err := pick(list, ids)
	if err != nil {
		var missing notOffered
		if errors.As(err, &missing) {
			return nil, bridgev2.WrapRespErrManual(err, mautrix.MNotFound.ErrCode, 404)
		}
		return nil, bridgev2.RespError(mautrix.MInvalidParam.WithMessage("%s", err.Error()))
	}
	if len(lower) == 1 && sendID(networkid.UserID(lower[0])) == edu[0] {
		ghost, err := ec.Main.Bridge.GetGhostByID(ctx, networkid.UserID(lower[0]))
		if err != nil {
			return nil, err
		}
		return ec.CreateChatWithGhost(ctx, ghost)
	}
	id := komuPortal(lower)
	if name == "" {
		name = komuName(list, lower)
	}
	ec.groups.Store(id, name)
	key := ec.portalKey(id)
	portal, err := ec.Main.Bridge.GetPortalByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	info, err := ec.GetChatInfo(ctx, portal)
	if err != nil {
		return nil, err
	}
	return &bridgev2.CreateChatResponse{PortalKey: key, Portal: portal, PortalInfo: info}, nil
}

// CreateGroup is "New message" to classes, groups and people: participants are recipient ids from the
// contact list (or EduPage's own, like StudTrieda7 or Student12 with the parents).
func (ec *EduPageClient) CreateGroup(ctx context.Context, params *bridgev2.GroupCreateParams) (*bridgev2.CreateChatResponse, error) {
	if params.Type != groupType {
		return nil, bridgev2.RespError(mautrix.MUnrecognized.WithMessage("Unrecognized group type %s", params.Type))
	}
	ids := make([]string, len(params.Participants))
	for i, p := range params.Participants {
		ids[i] = string(p)
	}
	return ec.komuChat(ctx, ids, ptr.Val(params.Name).Name)
}

// komuInfo is a message room's info; its name comes from the recipients while the room has none.
func (ec *EduPageClient) komuInfo(portal *bridgev2.Portal, info *bridgev2.ChatInfo, lower []string) {
	info.Type = ptr.Ptr(database.RoomTypeGroupDM)
	if n, ok := ec.groups.Load(portal.ID); ok {
		info.Name = ptr.Ptr(n.(string))
	} else if list := ec.school.Load(); list != nil && portal.Name == "" {
		if name := komuName(*list, lower); name != "" {
			info.Name = ptr.Ptr(name)
		}
	}
}

// komuTo is whom a message room's new message goes to, checked with the session that sends it: a
// recipient the account no longer may write to stops the message.
func komuTo(c *edupage.Client, lower []string) (string, error) {
	_, edu, err := pick(c.Recipients(), lower)
	if err != nil {
		return "", refused(err)
	}
	return strings.Join(edu, ";"), nil
}

// messageOptions reads a teacher's choices from a message's content.
func messageOptions(evt *event.Event) (*edupage.Options, error) {
	if evt == nil || evt.Content.Raw[OptionsKey] == nil {
		return nil, nil
	}
	data, err := json.Marshal(evt.Content.Raw[OptionsKey])
	if err != nil {
		return nil, err
	}
	var opts edupage.Options
	if err = json.Unmarshal(data, &opts); err != nil {
		return nil, refused(fmt.Errorf("%s is not valid: %w", OptionsKey, err))
	}
	if opts.Replies != "" && opts.Replies != "all" && opts.Replies != "author" && opts.Replies != "none" {
		return nil, refused(fmt.Errorf("%s: replies is all, author or none", OptionsKey))
	}
	return &opts, nil
}
