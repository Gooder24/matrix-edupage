package connector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

const (
	// Each homework's material is read once; a handful per poll keeps a fresh login's backfill gentle.
	materialsPerPoll = 5
	// Material is read only for recent homework: older notices come without it.
	materialDays = 14
	// Optional notices are about today; turning them on does not replay the whole school year.
	optionalMaxAge = 24 * time.Hour
)

func (ec *EduPageClient) pollLoop(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	healthy := false
	for {
		err := ec.poll(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.Warn().Err(err).Msg("EduPage poll failed")
			ec.reportError(err)
			healthy = false
			if errors.Is(err, ErrNeedsLogin) {
				return
			}
		case !healthy:
			healthy = true
			ec.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ec.Main.Config.PollInterval):
		}
	}
}

func schoolYearStart(today time.Time) time.Time {
	year := today.Year()
	if today.Month() < time.September {
		year--
	}
	return time.Date(year, time.September, 1, 0, 0, 0, 0, edupage.Location)
}

func (ec *EduPageClient) poll(ctx context.Context) error {
	cfg := ec.Main.Config
	backfilled, pending := ec.meta().progress()
	today := time.Now().In(edupage.Location)
	since := today.AddDate(0, 0, -cfg.RecentDays)
	if !backfilled {
		since = schoolYearStart(today)
		if cfg.InitialDays > 0 {
			since = today.AddDate(0, 0, -cfg.InitialDays)
		}
	} else if !pending.IsZero() && pending.Before(since) {
		since = pending // something is not in Matrix yet, or a notice waits
	}
	var items []edupage.Item
	var edu *edupage.Client
	err := ec.withSession(ctx, func(c *edupage.Client) (err error) {
		edu = c
		items, err = c.Everything(ctx, since)
		return err
	})
	if err != nil {
		return err
	}
	ec.setTeachers(edu.Teachers())
	sort.SliceStable(items, func(i, j int) bool {
		ti, tj := items[i].Time(), items[j].Time()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return items[i].ID.Int() < items[j].ID.Int()
	})
	ec.readMaterials(ctx, items)
	ec.roomsDown, ec.failedNow, ec.slowNow = false, map[networkid.PortalKey]bool{}, 0
	if ec.tries == nil {
		ec.tries, ec.roomFails = map[int64]int{}, map[networkid.PortalKey]int{}
	}
	var back horizon
	seen := ec.queueMessages(ctx, items, &back)
	err = ec.queueNotices(ctx, items, seen, &back)
	if err != nil {
		// A poll that stopped half way keeps everything it did not get to: it reads from where it started
		// again, and also from what it found that is older still.
		back.hold(since)
		if !pending.IsZero() {
			back.hold(pending)
		}
	}
	if !backfilled || !back.t.Equal(pending) {
		m := ec.meta()
		m.mu.Lock()
		m.Backfilled, m.Pending = backfilled || err == nil, 0
		if !back.t.IsZero() {
			m.Pending = back.t.Unix()
		}
		m.mu.Unlock()
		if saveErr := ec.save(ctx); err == nil {
			err = saveErr
		}
	}
	return err
}

// horizon is how far back the next poll reads: to the oldest item that is not in Matrix yet (events are
// queued, not delivered, and a restart before they land must not skip them) or that still waits.
type horizon struct{ t time.Time }

func (h *horizon) hold(t time.Time) {
	if h.t.IsZero() || t.Before(h.t) {
		h.t = t
	}
}

// maxTries: an item that is still not in Matrix after this many polls (bridgev2 or the homeserver
// refuses it) no longer holds the reads back. It is still queued whenever a read finds it.
const maxTries = 12

// undelivered counts another poll that found the item not in Matrix and holds the reads back to it.
func (ec *EduPageClient) undelivered(back *horizon, it *edupage.Item) {
	id := it.ID.Int()
	ec.tries[id]++
	if ec.tries[id] <= maxTries {
		back.hold(it.Time())
	}
}

// room makes sure the portal's room exists before an item is queued into it. ok is false when the item
// is not queued now: the filter does not want the chat (nothing waits), the homeserver refused the room
// (the item waits like one not delivered), or the homeserver did not answer. After that, the rest of the
// poll makes no new rooms; items in rooms that exist go on, and the waiting ones hold the reads back
// until a room can be made again.
func (ec *EduPageClient) room(ctx context.Context, key networkid.PortalKey, back *horizon, it *edupage.Item) bool {
	if ec.failedNow[key] || (ec.slowNow > 1 && ec.roomFails[key] >= maxTries) {
		// Its room already failed in this poll, or two chats that keep failing did: one try a poll is enough.
		ec.undelivered(back, it)
		return false
	}
	err := ec.ensureRoom(ctx, key)
	switch {
	case errors.Is(err, errFiltered):
		return false
	case errors.Is(err, errTransient) && ec.roomsDown:
		back.hold(it.Time())
		return false
	case errors.Is(err, errTransient) && ec.roomFails[key] < maxTries:
		zerolog.Ctx(ctx).Warn().Err(err).Str("portal", string(key.ID)).Msg("No new rooms in this poll")
		ec.roomFails[key]++
		ec.roomsDown = true
		back.hold(it.Time())
		return false
	case errors.Is(err, errTransient):
		// A chat whose room keeps failing stops holding back the others after a few polls. Once two such
		// chats failed in a poll, the others like them wait for the next poll; a new chat still tries once,
		// and its failure (a homeserver that is down) stops the rooms of this poll.
		zerolog.Ctx(ctx).Warn().Err(err).Str("portal", string(key.ID)).Msg("The homeserver keeps not making this room")
		ec.failedNow[key] = true
		ec.slowNow++
	case err == nil:
		delete(ec.roomFails, key)
	default:
		zerolog.Ctx(ctx).Warn().Err(err).Str("portal", string(key.ID)).Msg("The homeserver refused a room; the next poll tries again")
		ec.failedNow[key] = true
	}
	ec.undelivered(back, it)
	return err == nil
}

// setTeachers replaces the people the user can start a message to; one who left is gone at once.
func (ec *EduPageClient) setTeachers(list []edupage.Contact) {
	now := map[networkid.UserID]bool{}
	for _, t := range list {
		id := makeUserID(t.ID, t.Name)
		now[id] = true
		ec.teachers.Store(id, t)
	}
	ec.teachers.Range(func(k, _ any) bool {
		if !now[k.(networkid.UserID)] {
			ec.teachers.Delete(k)
		}
		return true
	})
}

// threadInfo is where a bridged message of this poll went, for its replies queued in the same poll.
type threadInfo struct {
	root   int64
	portal networkid.PortalKey
}

var reNumeric = regexp.MustCompile(`^\d+$`)

// counterpart is the portal of a thread's first message: the person who wrote it, or for one the user
// wrote, the person or group it went to.
func (ec *EduPageClient) counterpart(it *edupage.Item, userID string) networkid.PortalID {
	if !it.IsMine(userID) {
		return personPortal(makeUserID(string(it.Owner), string(it.OwnerName)))
	}
	to := makeUserID(string(it.User), string(it.UserName))
	if edupageID(to) != "" {
		if it.UserName != "" {
			ec.names.Store(to, string(it.UserName))
		}
		return personPortal(to)
	}
	id := groupPortal(string(to))
	if it.UserName != "" {
		ec.groups.Store(id, string(it.UserName))
	}
	return id
}

func (ec *EduPageClient) known(ctx context.Context, id int64) (*threadInfo, bool) {
	msg, err := ec.Main.Bridge.DB.Message.GetFirstPartByID(ctx, ec.UserLogin.ID, makeMessageID(id))
	if err != nil || msg == nil {
		return nil, false
	}
	root := msg.Metadata.(*MessageMetadata).Root
	if root == 0 {
		root = id
	}
	return &threadInfo{root: root, portal: msg.Room}, true
}

// queueMessages bridges new messages oldest first; a reply waits for its thread's first message, also
// when EduPage dates the reply earlier. It returns where each message went.
func (ec *EduPageClient) queueMessages(ctx context.Context, items []edupage.Item, back *horizon) map[int64]*threadInfo {
	m := ec.meta()
	m.mu.Lock()
	userID, school := m.UserID, m.School
	m.mu.Unlock()
	batch := map[int64]*edupage.Item{}
	for i := range items {
		if items[i].Type == "sprava" && !items[i].IsRemoved() && reNumeric.MatchString(string(items[i].ID)) {
			batch[items[i].ID.Int()] = &items[i]
		}
	}
	seen := map[int64]*threadInfo{}
	settle := map[networkid.PortalKey]networkid.MessageID{}
	settleBefore := time.Now().AddDate(0, 0, -ec.Main.Config.SettleDays)
	visiting, queuedNow := map[int64]bool{}, map[int64]bool{}
	var bridge func(it *edupage.Item)
	bridge = func(it *edupage.Item) {
		id := it.ID.Int()
		if _, ok := seen[id]; ok || visiting[id] {
			return
		}
		if info, ok := ec.known(ctx, id); ok {
			seen[id] = info
			delete(ec.tries, id)
			return
		}
		visiting[id] = true
		parent := it.ReplyTo.Int()
		if p, ok := batch[parent]; ok {
			bridge(p) // the thread's first message goes first, however EduPage dated it
		}
		if _, ok := seen[id]; ok {
			return
		}
		_, parentKnown := seen[parent]
		if !parentKnown {
			_, parentKnown = ec.known(ctx, parent)
		}
		// Helper copies exist only to notify people; the real reply is the item they point to.
		if it.Helper != "" && it.Helper != "0" && parent != 0 && (batch[parent] != nil || parentKnown) {
			return
		}
		info := &threadInfo{root: id}
		if parent != 0 {
			info.root = parent
			if p, ok := seen[parent]; ok {
				info.root, info.portal = p.root, p.portal
			} else if p, ok := ec.known(ctx, parent); ok {
				info.root, info.portal = p.root, p.portal
			}
		}
		if info.portal.ID == "" {
			info.portal = ec.portalKey(ec.counterpart(it, userID))
		}
		// A reply whose thread's first message is queued in this poll waits for it to be in Matrix, or
		// it would come without its link; once that message gave up, the reply comes alone.
		waitRoot := info.root != id && queuedNow[info.root] && ec.tries[info.root] <= maxTries
		seen[id] = info
		if !ec.room(ctx, info.portal, back, it) {
			return
		}
		ec.queueMessage(it, info, school, userID, waitRoot)
		queuedNow[id] = true
		if it.Time().Before(settleBefore) {
			settle[info.portal] = makeMessageID(id)
		}
	}
	for i := range items {
		if it := batch[items[i].ID.Int()]; it == &items[i] {
			bridge(it)
		}
	}
	// History shows up like on edupage.org: there, but already read.
	for portal, last := range settle {
		ec.UserLogin.QueueRemoteEvent(&simplevent.Receipt{
			EventMeta:  simplevent.EventMeta{Type: bridgev2.RemoteEventReadReceipt, PortalKey: portal, Sender: ec.sender(userID, "")},
			LastTarget: last,
		})
	}
	return seen
}

var errRootMissing = fmt.Errorf("%w: its thread's first message is not in Matrix yet", bridgev2.ErrIgnoringRemoteEvent)

func (ec *EduPageClient) queueMessage(it *edupage.Item, info *threadInfo, school, userID string, waitRoot bool) {
	id := it.ID.Int()
	item := *it
	sender := ec.sender(string(it.Owner), string(it.OwnerName))
	if it.IsMine(userID) {
		sender = ec.sender(userID, "") // also the owner's "Only…" alias
	}
	ec.UserLogin.QueueRemoteEvent(&simplevent.Message[*edupage.Item]{
		EventMeta: simplevent.EventMeta{
			Type:        bridgev2.RemoteEventMessage,
			PortalKey:   info.portal,
			Sender:      sender,
			Timestamp:   it.Time(),
			StreamOrder: id,
		},
		ID:   makeMessageID(id),
		Data: &item,
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, it *edupage.Item) (*bridgev2.ConvertedMessage, error) {
			if waitRoot {
				if root, err := ec.Main.Bridge.DB.Message.GetFirstPartByID(ctx, ec.UserLogin.ID, makeMessageID(info.root)); err != nil || root == nil {
					return nil, errRootMissing // the next poll queues it again
				}
			}
			return ec.convertMessage(it, info.root, school), nil
		},
	})
}

var schoolTypes = map[string]bool{"znamka": true, "signin": true, "event": true, "homework": true, "confirmation": true}

// schoolNotice: every announcement type, also future ones with text; not chat messages, substitutions
// (the timetable has them) or the optional arrival and meal notices.
func schoolNotice(it *edupage.Item) bool {
	switch kind := string(it.Type); kind {
	case "", "sprava", "substitution", "pipnutie", "strava_vydaj":
		return false
	default:
		return schoolTypes[kind] || it.Message() != ""
	}
}

func (ec *EduPageClient) optional(it *edupage.Item) bool {
	return (it.Type == "pipnutie" && ec.Main.Config.Arrivals) || (it.Type == "strava_vydaj" && ec.Main.Config.Meals)
}

func (ec *EduPageClient) needsMaterial(it *edupage.Item) bool {
	return it.HomeworkSuperID() != "" && it.Homework != nil && !it.IsRemoved() &&
		time.Since(it.Time()) < materialDays*24*time.Hour
}

// readMaterials reads the cards of recent homework not read yet; a failure is tried again next poll.
func (ec *EduPageClient) readMaterials(ctx context.Context, items []edupage.Item) {
	m := ec.meta()
	done := 0
	for i := len(items) - 1; i >= 0 && done < materialsPerPoll; i-- {
		it := &items[i]
		if !ec.needsMaterial(it) {
			continue
		}
		if _, ok := m.material(it.HomeworkSuperID()); ok {
			continue
		}
		done++
		var mat *edupage.Material
		err := ec.withSession(ctx, func(c *edupage.Client) (err error) {
			mat, err = c.Material(ctx, it.Homework)
			return err
		})
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("superid", it.HomeworkSuperID()).Msg("Failed to read homework material")
			continue
		}
		meta := &MaterialMeta{Text: mat.Text}
		for _, f := range mat.Files {
			meta.Files = append(meta.Files, FileMeta(f))
		}
		m.setMaterial(it.HomeworkSuperID(), meta)
	}
	if done > 0 {
		if err := ec.save(ctx); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to save homework material")
		}
	}
}

// queueNotices bridges new and changed notices. A notice that has to wait (for its material, the
// message it confirms or a file) holds the reads back, so later polls read back far enough.
func (ec *EduPageClient) queueNotices(ctx context.Context, items []edupage.Item, seen map[int64]*threadInfo, back *horizon) error {
	// Only for two weeks: a confirmation of a message that never comes must not stretch every poll.
	wait := func(it *edupage.Item) {
		if t := it.Time(); time.Since(t) < materialDays*24*time.Hour {
			back.hold(t)
		}
	}
	m := ec.meta()
	m.mu.Lock()
	userID, school := m.UserID, m.School
	m.mu.Unlock()
	for i := range items {
		it := &items[i]
		if !reNumeric.MatchString(string(it.ID)) || it.IsRemoved() {
			continue
		}
		optional := ec.optional(it)
		if !schoolNotice(it) && !(optional && time.Since(it.Time()) < optionalMaxAge) {
			continue
		}
		var material *MaterialMeta
		if ec.needsMaterial(it) {
			var ok bool
			if material, ok = m.material(it.HomeworkSuperID()); !ok {
				wait(it) // its material comes on a later poll
				continue
			}
		}
		n := ec.buildNotice(it, school, material)
		portal := ec.portalKey(schoolPortal)
		var replyTo int64
		if it.Type == "confirmation" {
			parent := it.ReplyTo.Int()
			if parent == 0 {
				parent = it.ParentID.Int()
			}
			thread, ok := seen[parent]
			if !ok {
				if thread, ok = ec.known(ctx, parent); !ok {
					wait(it) // until the message it confirms is bridged
					continue
				}
			}
			if it.DataString("receipt") == "" {
				continue
			}
			portal, replyTo = thread.portal, parent
		}
		sender := ec.sender(schoolGhost, "")
		if schoolNotice(it) {
			sender = ec.sender(string(it.Owner), string(it.OwnerName))
			if it.IsMine(userID) {
				sender = ec.sender(userID, "")
			}
		}
		id := it.ID.Int()
		existing, err := ec.Main.Bridge.DB.Message.GetAllPartsByID(ctx, ec.UserLogin.ID, makeMessageID(id))
		if err != nil {
			return err
		}
		base := simplevent.EventMeta{PortalKey: portal, Sender: sender, Timestamp: it.Time(), StreamOrder: id}
		if len(existing) > 0 {
			delete(ec.tries, id)
			if noticeUpToDate(existing, n.sig) {
				continue
			}
			if len(n.files) > 0 {
				wait(it) // its files are tried again on the next poll
			}
			base.Type, base.Timestamp = bridgev2.RemoteEventEdit, time.Now()
			ec.UserLogin.QueueRemoteEvent(&simplevent.Message[*notice]{
				EventMeta: base, ID: makeMessageID(id), TargetMessage: makeMessageID(id), Data: n,
				ConvertEditFunc: ec.convertNoticeEdit,
			})
			continue
		}
		if !ec.room(ctx, portal, back, it) {
			continue
		}
		if len(n.files) > 0 {
			wait(it) // a file that fails is tried again on the next poll, also for an old notice
		}
		base.Type = bridgev2.RemoteEventMessage
		ec.UserLogin.QueueRemoteEvent(&simplevent.Message[*notice]{
			EventMeta: base, ID: makeMessageID(id), Data: n,
			ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, n *notice) (*bridgev2.ConvertedMessage, error) {
				return ec.convertNotice(ctx, intent, n, replyTo)
			},
		})
	}
	return nil
}
