package connector

import (
	"context"
	"errors"
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
	backfill := !ec.meta().isBackfilled()
	today := time.Now().In(edupage.Location)
	since := today.AddDate(0, 0, -cfg.RecentDays)
	if backfill {
		since = schoolYearStart(today)
		if cfg.InitialDays > 0 {
			since = today.AddDate(0, 0, -cfg.InitialDays)
		}
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
	for _, t := range edu.Teachers() {
		ec.teachers.Store(makeUserID(t.ID, t.Name), t)
	}
	sort.SliceStable(items, func(i, j int) bool {
		ti, tj := items[i].Time(), items[j].Time()
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return items[i].ID.Int() < items[j].ID.Int()
	})
	ec.readMaterials(ctx, items)
	seen := ec.queueMessages(ctx, items, backfill)
	if err = ec.queueNotices(ctx, items, seen); err != nil {
		return err
	}
	if backfill {
		m := ec.meta()
		m.mu.Lock()
		m.Backfilled = true
		m.mu.Unlock()
		return ec.save(ctx)
	}
	return nil
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

// queueMessages bridges new messages oldest first; replies follow their thread's first message.
func (ec *EduPageClient) queueMessages(ctx context.Context, items []edupage.Item, backfill bool) map[int64]*threadInfo {
	m := ec.meta()
	m.mu.Lock()
	userID, school := m.UserID, m.School
	m.mu.Unlock()
	batch := map[int64]bool{}
	for i := range items {
		if items[i].Type == "sprava" && !items[i].IsRemoved() {
			batch[items[i].ID.Int()] = true
		}
	}
	seen := map[int64]*threadInfo{}
	settle := map[networkid.PortalKey]networkid.MessageID{}
	settleBefore := time.Now().AddDate(0, 0, -ec.Main.Config.SettleDays)
	for i := range items {
		it := &items[i]
		id := it.ID.Int()
		if it.Type != "sprava" || it.IsRemoved() || !reNumeric.MatchString(string(it.ID)) {
			continue
		}
		if info, ok := ec.known(ctx, id); ok {
			seen[id] = info
			continue
		}
		parent := it.ReplyTo.Int()
		_, parentKnown := ec.known(ctx, parent)
		// Helper copies exist only to notify people; the real reply is the item they point to.
		if it.Helper != "" && it.Helper != "0" && parent != 0 && (batch[parent] || parentKnown) {
			continue
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
		seen[id] = info
		ec.queueMessage(it, info, school)
		if backfill && it.Time().Before(settleBefore) {
			settle[info.portal] = makeMessageID(id)
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

func (ec *EduPageClient) queueMessage(it *edupage.Item, info *threadInfo, school string) {
	id := it.ID.Int()
	item := *it
	ec.UserLogin.QueueRemoteEvent(&simplevent.Message[*edupage.Item]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    info.portal,
			CreatePortal: true,
			Sender:       ec.sender(string(it.Owner), string(it.OwnerName)),
			Timestamp:    it.Time(),
			StreamOrder:  id,
		},
		ID:   makeMessageID(id),
		Data: &item,
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, it *edupage.Item) (*bridgev2.ConvertedMessage, error) {
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

func (ec *EduPageClient) queueNotices(ctx context.Context, items []edupage.Item, seen map[int64]*threadInfo) error {
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
				continue // its material comes on a later poll
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
					continue // wait until the message it confirms is bridged
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
			if noticeSig(existing) == n.sig {
				continue
			}
			base.Type, base.Timestamp = bridgev2.RemoteEventEdit, time.Now()
			ec.UserLogin.QueueRemoteEvent(&simplevent.Message[*notice]{
				EventMeta: base, ID: makeMessageID(id), TargetMessage: makeMessageID(id), Data: n,
				ConvertEditFunc: ec.convertNoticeEdit,
			})
			continue
		}
		base.Type, base.CreatePortal = bridgev2.RemoteEventMessage, true
		ec.UserLogin.QueueRemoteEvent(&simplevent.Message[*notice]{
			EventMeta: base, ID: makeMessageID(id), Data: n,
			ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, n *notice) (*bridgev2.ConvertedMessage, error) {
				return ec.convertNotice(ctx, intent, n, replyTo)
			},
		})
	}
	return nil
}
