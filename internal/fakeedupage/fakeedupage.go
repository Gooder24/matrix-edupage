// Package fakeedupage is a stand-in for edupage.org with the calls the bridge makes: sign-in (RPC and the
// old form, captcha, second factor), the timeline, writing messages, uploads and homework material.
// Every school is a path: http://127.0.0.1:port/<school>/... (edupage_url http://127.0.0.1:port/{school}).
// The people are made up.
package fakeedupage

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

const (
	School   = "myschool"
	Username = "alex.pupil"
	Password = "secret"
	Me       = "Student501"
	Bea      = "Ucitel11"
	Cyril    = "Ucitel12"
	Dora     = "Ucitel13" // left the school
	Eva      = "Ucitel14" // no messages yet
	Class    = "Trieda7"
	Code     = "246810" // the mailed second-factor code
)

// PNG is a 1×1 picture, served as a homework file.
var PNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0\xf0\x1f\x00\x05\x00\x01\xff\x89\x99=\x1d\x00\x00\x00\x00IEND\xaeB`\x82")

// Entry is one request the bridge made that writes something.
type Entry struct {
	Path string
	Form url.Values
}

type Server struct {
	*httptest.Server
	// Root is Bea's first message (a thread with Alex's reply), Grade the grade notice.
	Root, Grade int
	mu          sync.Mutex
	next        int
	items       []map[string]any
	homeworks   []map[string]any
	materials   map[string]any
	files       map[string][]byte
	sessions    map[string]bool
	log         []Entry
	twoFactor   bool
	captcha     bool
	confirmed   bool
	password    string
	pending     bool
}

func stamp(t time.Time) string { return t.In(edupage.Location).Format(time.DateTime) }

// New starts the fake with a message from Bea, Alex's reply in her thread with its helper copy, a class
// message from Cyril with an attachment, a grade and a homework with a card.
func New() *Server {
	s := &Server{materials: map[string]any{}, files: map[string][]byte{}, sessions: map[string]bool{}, next: 1000, password: Password}
	now := time.Now()
	root := s.add(map[string]any{"typ": "sprava", "vlastnik": Bea, "vlastnik_meno": "Bea Teacher", "user": Me, "user_meno": "Alex Pupil",
		"text": "Do you have the homework?", "cas_pridania": stamp(now.Add(-2 * time.Hour))})
	reply := s.add(map[string]any{"typ": "sprava", "vlastnik": Me, "vlastnik_meno": "Alex Pupil", "user": Bea, "reakcia_na": root,
		"text": "Sending it tonight", "cas_pridania": stamp(now.Add(-90 * time.Minute))})
	s.add(map[string]any{"typ": "sprava", "vlastnik": Me, "vlastnik_meno": "Alex Pupil", "user": Bea, "reakcia_na": reply,
		"pomocny_zaznam": "1", "text": "Sending it tonight", "cas_pridania": stamp(now.Add(-90 * time.Minute))})
	s.add(map[string]any{"typ": "sprava", "vlastnik": Cyril, "vlastnik_meno": "Cyril Teacher", "user": Class, "user_meno": "7.A",
		"text": "Trip on Friday", "cas_pridania": stamp(now.Add(-time.Hour)),
		"data": map[string]any{"attachements": map[string]any{"/" + School + "/files/trip.pdf": "trip.pdf"}}})
	s.Root = root
	s.Grade = s.add(map[string]any{"typ": "znamka", "vlastnik": Bea, "vlastnik_meno": "Bea Teacher", "text": "Mathematics: 1",
		"cas_pridania": stamp(now.Add(-50 * time.Minute))})
	s.files["/files/card.png"] = PNG
	s.materials["77"] = map[string]any{"cardids": []any{"1"}, "cardsData": map[string]any{"1": map[string]any{
		"content": mustJSON(map[string]any{"widgetClass": "CardWidget", "widgets": []any{
			map[string]any{"widgetClass": "TextETestWidget", "props": map[string]any{"htmlText": "<p>Read page 12</p>"}},
			map[string]any{"widgetClass": "FileETestWidget", "props": map[string]any{"files": []any{map[string]any{"src": "/" + School + "/files/card.png", "name": "card.png"}}}},
		}}),
	}}}
	s.homeworks = append(s.homeworks, map[string]any{"typ": "etesthw|hw", "testid": "77", "e_superid": "55", "etestCards": 1, "etestAnswerCards": 0})
	s.add(map[string]any{"typ": "homework", "vlastnik": Cyril, "vlastnik_meno": "Cyril Teacher", "text": "",
		"cas_pridania": stamp(now.Add(-40 * time.Minute)),
		"data":         mustJSON(map[string]any{"superid": "55", "nazov": "Worksheet", "date": now.AddDate(0, 0, 2).Format(time.DateOnly)})})
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Template is the edupage_url for the bridge.
func (s *Server) Template() string { return s.URL + "/{school}" }

func (s *Server) add(item map[string]any) int {
	s.next++
	item["timelineid"] = strconv.Itoa(s.next)
	if _, ok := item["removed"]; !ok {
		item["removed"] = "0"
	}
	s.items = append(s.items, item)
	return s.next
}

// Add puts a timeline item in and returns its id; cas_pridania defaults to now.
func (s *Server) Add(item map[string]any) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item["cas_pridania"] == nil {
		item["cas_pridania"] = stamp(time.Now())
	}
	return s.add(item)
}

// Update changes fields of an item, the way EduPage changes a notice.
func (s *Server) Update(id int, fields map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it["timelineid"] == strconv.Itoa(id) {
			for k, v := range fields {
				it[k] = v
			}
		}
	}
}

// Expire ends every session, so the bridge must sign in again.
func (s *Server) Expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]bool{}
}

// SetPassword changes the account's password.
func (s *Server) SetPassword(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = p
}

// SetTwoFactor makes the next sign-ins ask for a second factor (confirmed in the app or mailed as Code).
func (s *Server) SetTwoFactor(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.twoFactor = on
}

// SetCaptcha makes sign-ins fail with a captcha, as after too many wrong passwords.
func (s *Server) SetCaptcha(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captcha = on
}

// Confirm confirms the waiting sign-in in the "app".
func (s *Server) Confirm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmed = true
}

func (s *Server) Writes(part string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.log {
		if strings.Contains(e.Path, part) {
			out = append(out, e)
		}
	}
	return out
}

func (s *Server) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) rpcAnswer(w http.ResponseWriter, v any) {
	_, _ = io.WriteString(w, edupage.EncodeResponse([]byte(mustJSON(v))))
}

func (s *Server) newSession(w http.ResponseWriter) {
	s.next++
	id := fmt.Sprintf("sess-%d", s.next)
	s.sessions[id] = true
	http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: id, Path: "/"})
}

func (s *Server) signedIn(r *http.Request) bool {
	ck, err := r.Cookie("PHPSESSID")
	return err == nil && s.sessions[ck.Value]
}

const loginPage = `<html><script>var data = {"csrftoken":"csrf-1"};</script>Prihlásenie</html>`

func (s *Server) userPage(w http.ResponseWriter) {
	home := map[string]any{"userid": Me, "dbi": map[string]any{
		"students": map[string]any{"501": map[string]any{"firstname": "Alex", "lastname": "Pupil"}},
		"teachers": map[string]any{
			"11": map[string]any{"firstname": "Bea", "lastname": "Teacher"},
			"12": map[string]any{"firstname": "Cyril", "lastname": "Teacher"},
			"13": map[string]any{"firstname": "Dora", "lastname": "Teacher", "dateto": "2020-06-30"},
			"14": map[string]any{"firstname": "Eva", "lastname": "Teacher"},
		},
	}}
	fmt.Fprintf(w, "<html><script>\nuserhome(%s);\nASC.gsechash=\"gsec\";\nstart();\n</script></html>", mustJSON(home))
}

func (s *Server) loggedIn(w http.ResponseWriter, school string) map[string]any {
	if s.twoFactor {
		s.pending, s.confirmed = true, false
		return map[string]any{"status": "OK", "redirectUrl": "/" + school + "/login/twofactor?sn=1"}
	}
	s.newSession(w)
	if school == "login1" {
		return map[string]any{"status": "OK", "redirectUrl": s.URL + "/" + School + "/user/"}
	}
	return map[string]any{"status": "OK", "redirectUrl": "/" + school + "/user/"}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	school, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	path = "/" + path
	q := r.URL.Query()
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	form, _ := edupage.DecodeRequestBody(string(raw))
	if school != School && school != "login1" {
		http.NotFound(w, r)
		return
	}
	switch {
	case path == "/login/" && q.Get("akcia") == "getToken":
		s.rpcAnswer(w, map[string]any{"token": "tok-1"})
	case path == "/login/" && q.Get("akcia") == "login":
		var p map[string]any
		_ = json.Unmarshal([]byte(form.Get("rpcparams")), &p)
		if code, ok := p["t2fasec"].(string); ok {
			if !s.pending || (code != Code && code != "app-code") || (code == "app-code" && !s.confirmed) {
				s.rpcAnswer(w, map[string]any{"status": "FAIL"})
				return
			}
			s.pending = false
			s.newSession(w)
			s.rpcAnswer(w, map[string]any{"status": "OK", "redirectUrl": "/" + School + "/user/"})
			return
		}
		if s.captcha || p["username"] != Username || p["password"] != s.password || p["userToken"] != "tok-1" {
			s.rpcAnswer(w, map[string]any{"err": map[string]any{"error_id": "login_failed"}})
			return
		}
		s.rpcAnswer(w, s.loggedIn(w, school))
	case path == "/login/" && r.Method == http.MethodGet:
		_, _ = io.WriteString(w, loginPage)
	case path == "/login/edubarLogin.php":
		_ = r.ParseForm()
		switch {
		case s.captcha:
			http.Redirect(w, r, "/"+school+"/login/?cmd=MainLogin&cap=1", http.StatusFound)
		case r.PostForm.Get("username") != Username || r.PostForm.Get("password") != s.password || r.PostForm.Get("csrfauth") != "csrf-1":
			http.Redirect(w, r, "/"+school+"/login/?cmd=MainLogin&bad=1", http.StatusFound)
		default:
			http.Redirect(w, r, s.loggedIn(w, school)["redirectUrl"].(string), http.StatusFound)
		}
	case path == "/login/twofactor" && q.Get("sn") == "1":
		_, _ = io.WriteString(w, `<html><script src="/login/pics/jsw/twofactorlogin.js"></script></html>`)
	case path == "/login/twofactor":
		switch q.Get("akcia") {
		case "getData":
			s.json(w, map[string]any{"email": "a***@example.test", "deviceNames": []any{"Phone", ""}})
		case "checkIfConfirmed":
			if s.confirmed {
				s.json(w, map[string]any{"status": "ok", "data": "app-code"})
			} else {
				s.json(w, map[string]any{"status": "fail"})
			}
		case "sendEmail":
			s.log = append(s.log, Entry{Path: "sendEmail"})
			s.json(w, map[string]any{"status": "ok", "data": map[string]any{"email": "a***@example.test"}})
		}
	case path == "/login/logout.php":
		if ck, err := r.Cookie("PHPSESSID"); err == nil {
			delete(s.sessions, ck.Value)
		}
	case !s.signedIn(r):
		_, _ = io.WriteString(w, loginPage) // what EduPage shows to an expired session
	case path == "/user" || path == "/user/":
		s.userPage(w)
	case path == "/timeline/" && q.Get("akcia") == "getData":
		s.timeline(w, r, q)
	case path == "/timeline/" && q.Get("akcia") == "uploadAtt":
		file, header, err := r.FormFile("att")
		if err != nil {
			s.json(w, map[string]any{"status": "fail"})
			return
		}
		data, _ := io.ReadAll(file)
		where := fmt.Sprintf("/files/up-%d/%s", len(s.files), header.Filename)
		s.files[where] = data
		where = "/" + school + where // EduPage's paths start at the host, and here each school is a path
		s.log = append(s.log, Entry{Path: "uploadAtt", Form: url.Values{"name": {header.Filename}, "size": {strconv.Itoa(len(data))}, "type": {header.Header.Get("Content-Type")}}})
		s.json(w, map[string]any{"status": "ok", "data": map[string]any{"file": where, "name": header.Filename}})
	case path == "/timeline/" && q.Get("akcia") == "createItem":
		s.log = append(s.log, Entry{Path: "createItem", Form: form})
		var atts map[string]any
		_ = json.Unmarshal([]byte(form.Get("attachements")), &atts)
		id := s.add(map[string]any{"typ": "sprava", "vlastnik": Me, "vlastnik_meno": "Alex Pupil", "user": form.Get("selectedUser"),
			"text": form.Get("text"), "cas_pridania": stamp(time.Now()), "data": map[string]any{"attachements": atts}})
		s.rpcAnswer(w, map[string]any{"changes": []any{map[string]any{"timelineid": strconv.Itoa(id)}}})
	case path == "/timeline/" && q.Get("akcia") == "createReply":
		s.log = append(s.log, Entry{Path: "createReply", Form: form})
		var more map[string]any
		_ = json.Unmarshal([]byte(form.Get("moredata")), &more)
		id := s.add(map[string]any{"typ": "sprava", "vlastnik": Me, "vlastnik_meno": "Alex Pupil", "reakcia_na": form.Get("groupid"),
			"text": form.Get("text"), "cas_pridania": stamp(time.Now()), "data": map[string]any{"attachements": more["attachements"]}})
		s.add(map[string]any{"typ": "sprava", "vlastnik": Me, "vlastnik_meno": "Alex Pupil", "reakcia_na": strconv.Itoa(id),
			"pomocny_zaznam": "1", "text": form.Get("text"), "cas_pridania": stamp(time.Now())})
		s.rpcAnswer(w, map[string]any{"changes": []any{map[string]any{"timelineid": strconv.Itoa(id)}}})
	case path == "/elearning/" && q.Get("akcia") == "getETestData":
		_ = r.ParseForm()
		s.log = append(s.log, Entry{Path: "getETestData", Form: r.PostForm})
		s.json(w, map[string]any{"materialData": s.materials[r.PostForm.Get("testid")]})
	case strings.HasPrefix(path, "/files/"):
		data, ok := s.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", http.DetectContentType(data))
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request, q url.Values) {
	_ = r.ParseForm()
	since := r.PostForm.Get("datefrom")
	messages := false
	for _, tab := range q["filterTab"] {
		messages = messages || tab == "messages"
	}
	var out []map[string]any
	for _, it := range s.items {
		if (it["typ"] == "sprava") != messages {
			continue // here the "all" tab misses every message, which is why the bridge reads both
		}
		if day := fmt.Sprint(it["cas_pridania"]); len(day) >= 10 && day[:10] < since {
			continue
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return fmt.Sprint(out[i]["cas_pridania"]) > fmt.Sprint(out[j]["cas_pridania"]) })
	s.json(w, map[string]any{"timelineItems": out, "homeworks": s.homeworks})
}
