package e2e

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Gooder24/matrix-edupage/internal/fakeedupage"
	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

const wait = 30 * time.Second

type whoami struct {
	Logins []struct {
		ID    string `json:"id"`
		State struct {
			StateEvent string `json:"state_event"`
		} `json:"state"`
	} `json:"logins"`
}

func (e *env) state() (id, state string) {
	var who whoami
	if e.provision("GET", "/v3/whoami", nil, &who) != nil || len(who.Logins) != 1 {
		return "", ""
	}
	return who.Logins[0].ID, who.Logins[0].State.StateEvent
}

func isNotice(text string) func(map[string]any) bool {
	return func(e map[string]any) bool { return isBody(text)(e) && str(e, "content", "msgtype") == "m.notice" }
}

func count(evts []map[string]any, match func(map[string]any) bool) int {
	n := 0
	for _, e := range evts {
		if match(e) {
			n++
		}
	}
	return n
}

func TestBridge(t *testing.T) {
	e := setup(t, "1s")
	c, fake := e.c, e.fake

	t.Run("login refusals", func(t *testing.T) {
		if _, err := e.start("password", fakeedupage.Username, "wrong", ""); err == nil || !strings.Contains(err.Error(), "BAD_CREDENTIALS") {
			t.Errorf("a wrong password: %v", err)
		}
		fake.SetCaptcha(true)
		if _, err := e.start("password", fakeedupage.Username, fakeedupage.Password, fakeedupage.School); err == nil || !strings.Contains(err.Error(), "CAPTCHA") {
			t.Errorf("a captcha: %v", err)
		}
		fake.SetCaptcha(false)
		if _, err := e.start("password", fakeedupage.Username, fakeedupage.Password, "not a school!"); err == nil {
			t.Error("a broken school name was taken")
		}
	})

	t.Run("login confirmed in the app", func(t *testing.T) {
		fake.SetTwoFactor(true)
		defer fake.SetTwoFactor(false)
		// No school: login1 finds it, as on edupage.org.
		step, err := e.start("password", fakeedupage.Username, fakeedupage.Password, "")
		if err != nil || step.Type != "display_and_wait" {
			t.Fatalf("no confirmation step: %v %+v", err, step)
		}
		if !strings.Contains(step.Instructions, "Phone") {
			t.Errorf("the step does not name the device: %q", step.Instructions)
		}
		go func() { time.Sleep(time.Second); fake.Confirm() }()
		done, err := e.step(step, nil)
		if err != nil || done.Type != "complete" {
			t.Fatalf("login did not complete: %v %+v", err, done)
		}
		if id, _ := e.state(); id != fakeedupage.School+":"+fakeedupage.Me {
			t.Errorf("login id %q", id)
		}
	})

	dm := ""
	if !until(wait, func() bool { dm = c.roomWhere(isBody("Do you have the homework?")); return dm != "" }) {
		t.Fatal("the DM with Bea never came")
	}
	cyril := ""
	if !until(wait, func() bool { cyril = c.roomWhere(isBody("Trip on Friday")); return cyril != "" }) {
		t.Fatal("Cyril's class message never came")
	}
	school := ""
	if !until(wait, func() bool { school = c.roomWhere(isNotice("Mathematics: 1")); return school != "" }) {
		t.Fatal("the grade never came")
	}
	root := c.find(dm, isBody("Do you have the homework?"))

	t.Run("incoming messages", func(t *testing.T) {
		if root["sender"] != ghost(fakeedupage.Bea) {
			t.Errorf("sender %v", root["sender"])
		}
		var reply map[string]any
		until(wait, func() bool { reply = c.find(dm, isBody("Sending it tonight")); return reply != nil })
		if reply == nil || reply["sender"] != ghost(fakeedupage.Me) {
			t.Fatalf("own reply missing or from the wrong sender: %v", reply)
		}
		if str(reply, "content", "m.relates_to", "m.in_reply_to", "event_id") != root["event_id"] {
			t.Errorf("the reply does not answer its thread: %v", reply["content"])
		}
		if n := count(c.roomEvents(dm), isBody("Sending it tonight")); n != 1 {
			t.Errorf("the reply's helper copy was bridged too (%d)", n)
		}
		trip := c.find(cyril, isBody("Trip on Friday"))
		body := str(trip, "content", "body")
		if !strings.Contains(body, "Prílohy:") || !strings.Contains(body, fake.URL+"/"+fakeedupage.School+"/files/trip.pdf") {
			t.Errorf("attachment link missing: %q", body)
		}
		if str(trip, "content", "sk.karchi.edupage.to") != "7.A" || trip["sender"] != ghost(fakeedupage.Cyril) {
			t.Errorf("class message: %v from %v", trip["content"], trip["sender"])
		}
		if school == dm || school == cyril {
			t.Error("notices share a room with a person")
		}

		// The reply is dated before its thread's first message, which goes to Cyril's room first.
		var form, signed map[string]any
		room := ""
		until(wait, func() bool {
			if room = c.roomWhere(isBody("Is the form signed?")); room != "" {
				form, signed = c.find(room, isBody("Is the form signed?")), c.find(room, isBody("I signed it"))
			}
			return form != nil && signed != nil
		})
		if form == nil || signed == nil || room == dm {
			t.Fatalf("an early reply is not in its thread's room: %v %v", form, signed)
		}
		if str(signed, "content", "m.relates_to", "m.in_reply_to", "event_id") != form["event_id"] {
			t.Errorf("the early reply does not answer its thread: %v", signed["content"])
		}
		var chainRoom, lastRoom string
		until(wait, func() bool {
			chainRoom, lastRoom = c.roomWhere(isBody("Chain start")), c.roomWhere(isBody("Chain link 5"))
			return chainRoom != "" && lastRoom != ""
		})
		if chainRoom == "" || lastRoom != chainRoom {
			t.Errorf("a long chain of early replies left its thread's room: %q %q", chainRoom, lastRoom)
		}
		var phone map[string]any
		until(wait, func() bool { phone = c.find(dm, isBody("Sent from the phone")); return phone != nil })
		if phone == nil || phone["sender"] != ghost(fakeedupage.Me) {
			t.Errorf("a message from the user's Only alias is not the user's: %v", phone)
		}
	})

	t.Run("ghost names", func(t *testing.T) {
		var p struct {
			Name string `json:"displayname"`
		}
		if !until(wait, func() bool {
			return c.call("GET", "/_matrix/client/v3/profile/"+ghost(fakeedupage.Bea), nil, &p) == nil && p.Name == "Bea Teacher"
		}) {
			t.Errorf("Bea's ghost: %+v", p)
		}
		var room struct {
			Name string `json:"name"`
		}
		if err := c.call("GET", "/_matrix/client/v3/rooms/"+school+"/state/m.room.name/", nil, &room); err != nil || room.Name != "EduPage" {
			t.Errorf("notices room name %q (%v)", room.Name, err)
		}
	})

	t.Run("notices", func(t *testing.T) {
		grade := c.find(school, isNotice("Mathematics: 1"))
		if grade["sender"] != ghost(fakeedupage.Bea) || str(grade, "content", "sk.karchi.edupage.kind") != "znamka" ||
			!strings.Contains(str(grade, "content", "body"), "/"+fakeedupage.School+"/znamky/") {
			t.Errorf("grade: %v from %v", grade["content"], grade["sender"])
		}
		var hw map[string]any
		until(wait, func() bool { hw = c.find(school, isNotice("Worksheet")); return hw != nil })
		body := str(hw, "content", "body")
		if !strings.Contains(body, "Read page 12") || !strings.Contains(body, "Termín:") {
			t.Errorf("homework without its material or deadline: %q", body)
		}
		var card map[string]any
		until(wait, func() bool {
			card = c.find(school, func(e map[string]any) bool { return str(e, "content", "msgtype") == "m.image" })
			return card != nil
		})
		if card == nil || str(card, "content", "body") != "card.png" || !strings.HasPrefix(str(card, "content", "url"), "mxc://") {
			t.Errorf("homework file part: %v", card)
		}
		if n := len(fake.Writes("getETestData")); n != 1 {
			t.Errorf("material read %d times", n)
		}

		fake.Add(map[string]any{"typ": "signin", "vlastnik": fakeedupage.Cyril, "vlastnik_meno": "Cyril Teacher", "text": "Ski course",
			"data": map[string]any{"name": "Ski course", "typeName": "Course", "time_from": "2026-12-01 08:00:00", "time_to": "2026-12-05 16:00:00",
				"targetNames": []any{"Week 1"}, "confirmTime": "2026-11-20 10:00:00"}})
		fake.Add(map[string]any{"typ": "confirmation", "vlastnik": fakeedupage.Bea, "vlastnik_meno": "Bea Teacher", "reakcia_na": fake.Root,
			"data": map[string]any{"receipt": "Bea Teacher"}})
		fake.Add(map[string]any{"typ": "pipnutie", "vlastnik": fakeedupage.Me, "text": "Arrived at 7:45"})
		fake.Update(fake.Grade, map[string]any{"text": "Mathematics: 2"})

		var signin, confirm, edit map[string]any
		until(wait, func() bool {
			signin = c.find(school, isNotice("Prihlasovanie: Ski course"))
			confirm = c.find(dm, isNotice("Potvrdené prečítanie: Bea Teacher"))
			edit = c.find(school, func(e map[string]any) bool {
				return str(e, "content", "m.relates_to", "rel_type") == "m.replace" && str(e, "content", "m.relates_to", "event_id") == grade["event_id"]
			})
			return signin != nil && confirm != nil && edit != nil
		})
		if b := str(signin, "content", "body"); !strings.Contains(b, "Zvolené: Week 1") || !strings.Contains(b, "Otvorené 1. 12. 08:00") {
			t.Errorf("sign-up: %q", b)
		}
		if str(confirm, "content", "m.relates_to", "m.in_reply_to", "event_id") != root["event_id"] {
			t.Errorf("confirmation is not a reply to its message: %v", confirm)
		}
		if !strings.Contains(str(edit, "content", "m.new_content", "body"), "Mathematics: 2") {
			t.Errorf("changed grade not an edit: %v", edit)
		}
		if c.roomWhere(isBody("Arrived at 7:45")) != "" {
			t.Error("an arrival came though arrivals are off")
		}

		// A homework file whose download fails comes as a link, then as the file on a later poll.
		fake.FailFile("/files/essay.png", 1)
		fake.AddHomework("Essay", "Write one page", "essay.png", fakeedupage.PNG)
		var link, fixed map[string]any
		until(wait, func() bool {
			link = c.find(school, func(e map[string]any) bool { return strings.HasPrefix(str(e, "content", "body"), "essay.png: ") })
			return link != nil
		})
		if link == nil {
			t.Fatal("the failed homework file left no link")
		}
		until(wait, func() bool {
			fixed = c.find(school, func(e map[string]any) bool {
				return str(e, "content", "m.relates_to", "event_id") == link["event_id"] && str(e, "content", "m.new_content", "msgtype") == "m.image"
			})
			return fixed != nil
		})
		if fixed == nil {
			t.Error("the homework file never came after its download failed once")
		}
	})

	// The first read is the school year; then the reads come back to the newest week as soon as
	// everything is in Matrix, also while new messages keep coming. A refused chat holds nothing back.
	t.Run("reads back only as far as needed", func(t *testing.T) {
		for i := 0; i < 4; i++ {
			text := fmt.Sprintf("Keep coming %d", i)
			fake.Add(map[string]any{"typ": "sprava", "vlastnik": fakeedupage.Bea, "vlastnik_meno": "Bea Teacher", "user": fakeedupage.Me, "text": text})
			if !until(wait, func() bool { return c.find(dm, isBody(text)) != nil }) {
				t.Fatalf("%q never came", text)
			}
		}
		dates := fake.Dates()
		week := time.Now().In(edupage.Location).AddDate(0, 0, -7).Format(time.DateOnly) // recent_days
		if last := dates[len(dates)-1]; last < week {
			t.Errorf("polls still read back to %s (all reads: %v)", last, dates)
		}
		if room := c.roomWhere(isBody("From a filtered chat")); room != "" {
			t.Error("the portal create filter was bypassed")
		}
	})

	t.Run("room features", func(t *testing.T) {
		var f map[string]any
		if err := c.call("GET", "/_matrix/client/v3/rooms/"+dm+"/state/com.beeper.room_features/"+url.PathEscape(server+"/edupage"), nil, &f); err != nil {
			t.Fatal(err)
		}
		if v, _ := f["reply"].(float64); v <= 0 {
			t.Errorf("reply not announced: %v", f["reply"])
		}
		for _, k := range []string{"edit", "delete", "reaction"} {
			if v, _ := f[k].(float64); v > 0 {
				t.Errorf("%s announced, EduPage has none: %v", k, f[k])
			}
		}
		if f["file"] == nil || f["read_receipts"] == true || f["typing_notifications"] == true {
			t.Errorf("files, receipts or typing announced wrongly: %v", f)
		}
		// The notices room refuses everything, so it announces nothing.
		var n map[string]any
		if err := c.call("GET", "/_matrix/client/v3/rooms/"+school+"/state/com.beeper.room_features/"+url.PathEscape(server+"/edupage"), nil, &n); err != nil {
			t.Fatal(err)
		}
		if v, _ := n["reply"].(float64); v > 0 || n["file"] != nil {
			t.Errorf("the notices room announces replies or files: %v", n)
		}
	})

	t.Run("outgoing", func(t *testing.T) {
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "answer in the thread",
			"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": root["event_id"]}}})
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.text", "body": "a new message"})
		img := c.upload(fakeedupage.PNG, "dot.png", "image/png")
		c.send(dm, "m.room.message", map[string]any{"msgtype": "m.image", "body": "dot.png", "url": img, "info": map[string]any{"mimetype": "image/png"}})
		if !until(wait, func() bool { return len(fake.Writes("createReply")) == 1 && len(fake.Writes("createItem")) == 2 }) {
			t.Fatalf("sent %d replies, %d messages", len(fake.Writes("createReply")), len(fake.Writes("createItem")))
		}
		reply := fake.Writes("createReply")[0].Form
		if reply.Get("groupid") != strconv.Itoa(fake.Root) || reply.Get("text") != "answer in the thread" {
			t.Errorf("reply %v", reply)
		}
		items := fake.Writes("createItem")
		if items[0].Form.Get("selectedUser") != fakeedupage.Bea || items[0].Form.Get("text") != "a new message" {
			t.Errorf("new message %v", items[0].Form)
		}
		up := fake.Writes("uploadAtt")
		if len(up) != 1 || up[0].Form.Get("name") != "dot.png" || up[0].Form.Get("size") != strconv.Itoa(len(fakeedupage.PNG)) ||
			!strings.Contains(items[1].Form.Get("attachements"), "dot.png") {
			t.Errorf("file: %v %v", up, items[1].Form)
		}
		// Let several polls read the sent messages back; none may come in again.
		time.Sleep(4 * time.Second)
		for _, text := range []string{"answer in the thread", "a new message"} {
			if n := count(c.roomEvents(dm), isBody(text)); n != 1 {
				t.Errorf("%q is in the room %d times", text, n)
			}
		}

		c.send(school, "m.room.message", map[string]any{"msgtype": "m.text", "body": "to nobody"})
		if !until(wait, func() bool {
			return c.find(school, func(e map[string]any) bool {
				return e["type"] == "com.beeper.message_send_status" && strings.HasPrefix(str(e, "content", "status"), "FAIL") &&
					c.find(school, isNotice("not bridged")) != nil
			}) != nil
		}) {
			t.Error("a message to the notices room was not refused")
		}
		if n := len(fake.Writes("createItem")) + len(fake.Writes("createReply")); n != 3 {
			t.Errorf("the refused message went out (%d writes)", n)
		}
	})

	t.Run("teachers and a new DM", func(t *testing.T) {
		var found struct {
			Results []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"results"`
		}
		if err := e.provision("POST", "/v3/search_users", map[string]string{"query": "éva"}, &found); err != nil || len(found.Results) != 1 || found.Results[0].Name != "Eva Teacher" {
			t.Fatalf("search: %v %+v", err, found)
		}
		if err := e.provision("POST", "/v3/search_users", map[string]string{"query": "dora"}, &found); err != nil || len(found.Results) != 0 {
			t.Errorf("a teacher who left is found: %v %+v", err, found)
		}
		var created struct {
			Room string `json:"dm_room_mxid"`
		}
		if err := e.provision("POST", "/v3/create_dm/"+found0(t, e, "eva"), map[string]any{}, &created); err != nil || created.Room == "" {
			t.Fatalf("create DM: %v %+v", err, created)
		}
		if !until(wait, func() bool { return c.joined(created.Room) }) {
			t.Fatal("never joined the new DM")
		}
		c.send(created.Room, "m.room.message", map[string]any{"msgtype": "m.text", "body": "hello Eva"})
		if !until(wait, func() bool {
			items := fake.Writes("createItem")
			return len(items) == 3 && items[2].Form.Get("selectedUser") == fakeedupage.Eva
		}) {
			t.Errorf("the new DM's message did not go to Eva: %v", fake.Writes("createItem"))
		}
	})

	t.Run("expired session and changed password", func(t *testing.T) {
		fake.Leave(fakeedupage.Eva)
		fake.Expire()
		fake.Add(map[string]any{"typ": "sprava", "vlastnik": fakeedupage.Bea, "vlastnik_meno": "Bea Teacher", "user": fakeedupage.Me, "text": "after the expiry"})
		if !until(wait, func() bool { return c.roomWhere(isBody("after the expiry")) != "" }) {
			t.Error("no new sign-in after the session expired")
		}
		var found struct {
			Results []struct{} `json:"results"`
		}
		if !until(wait, func() bool {
			return e.provision("POST", "/v3/search_users", map[string]string{"query": "eva"}, &found) == nil && len(found.Results) == 0
		}) {
			t.Errorf("a teacher who left during the login is still found: %d", len(found.Results))
		}
		fake.SetPassword("changed")
		fake.Expire()
		if !until(wait, func() bool { _, s := e.state(); return s == "BAD_CREDENTIALS" }) {
			id, s := e.state()
			t.Errorf("state after the password changed: %s %s", id, s)
		}
	})

	t.Run("login with a mailed code", func(t *testing.T) {
		fake.SetPassword(fakeedupage.Password)
		fake.SetTwoFactor(true)
		defer fake.SetTwoFactor(false)
		step, err := e.start("password_code", fakeedupage.Username, fakeedupage.Password, fakeedupage.School)
		if err != nil || step.Type != "user_input" || len(fake.Writes("sendEmail")) != 1 {
			t.Fatalf("no code step: %v %+v", err, step)
		}
		if _, err = e.step(step, map[string]string{"code": "111111"}); err == nil {
			t.Error("a wrong code was taken")
		}
		step, err = e.start("password_code", fakeedupage.Username, fakeedupage.Password, fakeedupage.School)
		if err != nil {
			t.Fatal(err)
		}
		done, err := e.step(step, map[string]string{"code": fakeedupage.Code})
		if err != nil || done.Type != "complete" {
			t.Fatalf("login did not complete: %v %+v", err, done)
		}
		if !until(wait, func() bool { _, s := e.state(); return s == "CONNECTED" }) {
			id, s := e.state()
			t.Errorf("state after signing in again: %s %s", id, s)
		}
		fake.Add(map[string]any{"typ": "sprava", "vlastnik": fakeedupage.Bea, "vlastnik_meno": "Bea Teacher", "user": fakeedupage.Me, "text": "welcome back"})
		if !until(wait, func() bool { return c.find(dm, isBody("welcome back")) != nil }) {
			t.Error("the same DM did not continue after signing in again")
		}
	})
}

func found0(t *testing.T, e *env, query string) string {
	var found struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := e.provision("POST", "/v3/search_users", map[string]string{"query": query}, &found); err != nil || len(found.Results) == 0 {
		t.Fatalf("search %q: %v", query, err)
	}
	return found.Results[0].ID
}

func (c *mx) joined(room string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.events[room]
	return ok
}
