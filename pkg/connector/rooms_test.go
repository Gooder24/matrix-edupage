package connector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

// homeserver answers room creation per chat: nil makes the room, an error fails it.
type homeserver struct {
	answer func(chat string) error
	tries  map[string]int
	made   map[string]bool
	ec     *EduPageClient
}

func (hs *homeserver) ensure(_ context.Context, key networkid.PortalKey) error {
	if hs.made[string(key.ID)] {
		return nil
	} else if hs.ec.roomsDown {
		return errTransient // like ensureRoom: no request once the poll found the homeserver down
	}
	hs.tries[string(key.ID)]++
	if err := hs.answer(string(key.ID)); err != nil {
		return err
	}
	hs.made[string(key.ID)] = true
	return nil
}

// poll runs room() for one item of each chat, like a poll that finds them, and returns how far back the
// next poll reads.
func poll(ec *EduPageClient, items map[string]*edupage.Item, order []string) horizon {
	ec.roomsDown, ec.failedNow, ec.slowNow = false, map[networkid.PortalKey]bool{}, 0
	var back horizon
	for _, chat := range order {
		ec.room(context.Background(), networkid.PortalKey{ID: networkid.PortalID(chat)}, &back, items[chat])
	}
	return back
}

func setupRooms(chats []string, answer func(string) error) (*EduPageClient, *homeserver, map[string]*edupage.Item) {
	hs := &homeserver{answer: answer, tries: map[string]int{}, made: map[string]bool{}}
	ec := &EduPageClient{ensure: hs.ensure, tries: map[int64]int{}, roomFails: map[networkid.PortalKey]int{}}
	hs.ec = ec
	items := map[string]*edupage.Item{}
	start := time.Date(2026, 11, 2, 8, 0, 0, 0, edupage.Location)
	for i, chat := range chats {
		items[chat] = &edupage.Item{ID: edupage.Str(fmt.Sprint(100 + i)), Added: edupage.Str(start.Add(time.Duration(i) * time.Hour).Format(time.DateTime))}
	}
	return ec, hs, items
}

var (
	errAnswered = fmt.Errorf("M_UNKNOWN: internal error") // the homeserver's own answer about this room
	errSilent   = fmt.Errorf("%w: context deadline exceeded", errTransient)
)

// A homeserver that is down for longer than the cap keeps every waiting item: the reads stay back at the
// oldest, and a poll costs at most three tries.
func TestLongOutageKeepsWaiting(t *testing.T) {
	chats := []string{"a", "b", "c", "d"}
	down := true
	ec, hs, items := setupRooms(chats, func(string) error {
		if down {
			return errSilent
		}
		return nil
	})
	oldest := items["a"].Time()
	for i := range 3 * maxTries {
		before := sum(hs.tries)
		back := poll(ec, items, chats)
		if !back.t.Equal(oldest) {
			t.Fatalf("poll %d reads back to %v, not to the oldest waiting item %v", i, back.t, oldest)
		}
		if n := sum(hs.tries) - before; n > 3 {
			t.Fatalf("poll %d tried %d rooms of a homeserver that is down", i, n)
		}
	}
	down = false
	poll(ec, items, chats)
	for _, chat := range chats {
		if !hs.made[chat] {
			t.Errorf("chat %s got no room once the homeserver was back", chat)
		}
	}
}

// Chats whose rooms the homeserver keeps failing do not hold back a new chat, whether or not it answers.
func TestFailingChatsDoNotHoldBackNewOnes(t *testing.T) {
	for name, fail := range map[string]error{"answered": errAnswered, "no answer": errSilent} {
		t.Run(name, func(t *testing.T) {
			chats := []string{"x", "y", "new"}
			ec, hs, items := setupRooms(chats, func(chat string) error {
				if chat == "new" {
					return nil
				}
				return fail
			})
			polls := 0
			for ; polls < 3*maxTries && !hs.made["new"]; polls++ {
				poll(ec, items, chats)
			}
			if !hs.made["new"] {
				t.Fatal("a new chat never got its room")
			}
			if fail == errAnswered && polls > 1 {
				t.Errorf("a new chat waited %d polls behind chats the homeserver answered about", polls)
			}
		})
	}
}

func sum(m map[string]int) (n int) {
	for _, v := range m {
		n += v
	}
	return n
}
