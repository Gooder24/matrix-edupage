package edupage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestBodyRoundTrip(t *testing.T) {
	body := EncodeRequestBody([]Pair{{"text", "Ahoj / čau & <b>"}, {"to", "Ucitel1"}})
	form, err := DecodeRequestBody(body)
	if err != nil || form.Get("text") != "Ahoj / čau & <b>" || form.Get("to") != "Ucitel1" {
		t.Fatalf("%v %v", form, err)
	}
	if _, err = DecodeRequestBody(body[:len(body)-8] + "eqaz=1"); err == nil {
		t.Error("a broken checksum was accepted")
	}
	if got := quote("a b/c~d+e"); got != "a%20b/c~d%2Be" {
		t.Errorf("quote %q", got)
	}
}

func TestDecodeResponse(t *testing.T) {
	for _, in := range []string{EncodeResponse([]byte(`{"a":1}`)), `{"a":1}`, "eqwd:eyJhIjoxfQ"} {
		if out, err := DecodeResponse(in); err != nil || string(out) != `{"a":1}` {
			t.Errorf("%q → %q %v", in, out, err)
		}
	}
}

func TestParseUserHome(t *testing.T) {
	c := New("", time.Second)
	page := "<script>\nuserhome({\"userid\":\"Student7\",\"dbi\":{\"teachers\":{\"1\":{\"firstname\":\"Ana\",\"lastname\":\"Novak\"}," +
		"\"2\":{\"firstname\":\"Old\",\"lastname\":\"One\",\"dateto\":\"2001-01-01\"}}," +
		"\"students\":{\"7\":{\"firstname\":\"Me\",\"lastname\":\"Myself\"}}}});\nASC.gsechash=\"abc\";\nfoo();\n</script>"
	if err := c.parseUserHome(page); err != nil {
		t.Fatal(err)
	}
	if c.UserID() != "Student7" || c.GsecHash != "abc" || c.MyName() != "Me Myself" {
		t.Errorf("%q %q %q", c.UserID(), c.GsecHash, c.MyName())
	}
	if name, ok := c.Person("UcitelOnly1"); !ok || name != "Ana Novak" {
		t.Errorf("person %q %v", name, ok)
	}
	if ts := c.Teachers(); len(ts) != 1 || ts[0].ID != "Ucitel1" {
		t.Errorf("teachers %+v", ts)
	}
	if c.parseUserHome("<html>login</html>") != ErrBadCredentials {
		t.Error("the login page counted as signed in")
	}
}

func TestSchoolAddresses(t *testing.T) {
	c := New("", time.Second)
	c.School = "myschool"
	for addr, want := range map[string]bool{
		"https://myschool.edupage.org/files/a.pdf":          true,
		"https://other.edupage.org/files/a.pdf":             false,
		"https://myschool.edupage.org.evil.test/files/a":    false,
		"https://myschool.edupage.org":                      true,
		"https://evil.test/?u=https://myschool.edupage.org": false,
	} {
		if got := c.SameSite(addr); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
	if Fold("Šťastný Ján") != "Stastny Jan" {
		t.Errorf("fold %q", Fold("Šťastný Ján"))
	}
}

func TestItem(t *testing.T) {
	it := Item{Type: "sprava", Text: "hi", Added: "2026-03-29 02:30:00", RawData: []byte(`{"attachements":{"/f/a.pdf":"a.pdf"},"messageContent":"full text"}`)}
	if it.Message() != "full text" || it.Attachments()["/f/a.pdf"] != "a.pdf" {
		t.Errorf("%q %v", it.Message(), it.Attachments())
	}
	if it.Time().IsZero() {
		t.Error("no time")
	}
}

func TestDownloadStaysOnTheSchool(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from elsewhere"))
	}))
	defer other.Close()
	school := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/myschool/files/away":
			http.Redirect(w, r, other.URL+"/myschool/files/x", http.StatusFound)
		case "/myschool/files/moved":
			http.Redirect(w, r, "/myschool/files/here", http.StatusFound)
		case "/myschool/files/here":
			_, _ = w.Write([]byte("the file"))
		}
	}))
	defer school.Close()
	c := New(school.URL+"/{school}", time.Second)
	c.School = "myschool"
	if _, _, err := c.Download(context.Background(), school.URL+"/myschool/files/away", 100); err == nil {
		t.Error("followed a redirect to another host")
	}
	if data, _, err := c.Download(context.Background(), school.URL+"/myschool/files/moved", 100); err != nil || string(data) != "the file" {
		t.Errorf("a redirect within the school: %q %v", data, err)
	}
}

func TestAnswerCardsZero(t *testing.T) {
	for _, c := range []struct {
		v        any
		zero, ok bool
	}{{float64(0), true, true}, {float64(2), false, true}, {"", false, false}, {"0", false, false}, {nil, false, false}} {
		zero, ok := (&Homework{AnswerCards: c.v}).AnswerCardsZero()
		if zero != c.zero || ok != c.ok {
			t.Errorf("%#v: zero=%v ok=%v", c.v, zero, ok)
		}
	}
}
