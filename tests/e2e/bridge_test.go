// Package e2e runs the bridge as its own process between a throwaway Tuwunel homeserver
// and the fake EduPage in internal/fakeedupage. Nothing reaches edupage.org or another server.
//
//	TUWUNEL=/path/to/tuwunel go test ./tests/e2e -v
package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gooder24/matrix-edupage/internal/fakeedupage"
)

const server = "test"

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func secret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// until waits for something the bridge does on its own (a poll, a sync) and reports whether it happened.
func until(d time.Duration, check func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if check() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return check()
}

// ghost is the Matrix user of an EduPage person ("Ucitel11" → @edupage_ucitel11:test).
func ghost(id string) string { return "@edupage_" + strings.ToLower(id) + ":" + server }

type env struct {
	t        *testing.T
	fake     *fakeedupage.Server
	c        *mx
	prov     string
	provAuth string
	pid      int
	// roomsDown makes the homeserver answer the bridge's createRoom with 500, as if it were down.
	roomsDown *atomic.Bool
	// away makes the homeserver answer the bridge 502 to everything, as if it were down behind its proxy;
	// versions counts the bridge's checks of it (one a poll).
	away     *atomic.Bool
	versions *atomic.Int32
	// writesAway answers 502 to every write of the bridge while reads still work.
	writesAway *atomic.Bool
	// failing makes createRoom answer 500 for one chat: a lower-case EduPage id → *atomic.Int32 of tries.
	failing *sync.Map
	// checkRefused makes the homeserver itself refuse the poll's check write (403): it is there all the same.
	checkRefused *atomic.Bool
	// db is the bridge's database.
	db string
}

func (e *env) provision(method, path string, body any, out any) error {
	var reader *bytes.Reader
	data, _ := json.Marshal(body)
	reader = bytes.NewReader(data)
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, _ := http.NewRequest(method, e.prov+path+sep+"user_id="+url.QueryEscape(e.c.userID), reader)
	req.Header.Set("Authorization", "Bearer "+e.provAuth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func setup(t *testing.T, poll string) *env {
	tuwunel := os.Getenv("TUWUNEL")
	if tuwunel == "" {
		t.Skip("TUWUNEL is not set")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "matrix-edupage")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/matrix-edupage")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	fake := fakeedupage.New()
	t.Cleanup(fake.Close)

	hsPort, brPort := freePort(t), freePort(t)
	asToken, hsToken, regToken, provSecret := secret(), secret(), secret(), secret()
	apps := filepath.Join(dir, "appservices")
	_ = os.Mkdir(apps, 0o700)
	registration := fmt.Sprintf(`id: edupage
url: http://127.0.0.1:%d
as_token: %s
hs_token: %s
sender_localpart: edupagebot
rate_limited: false
receive_ephemeral: true
de.sorunome.msc2409.push_ephemeral: true
namespaces:
  users:
  - regex: '^@edupagebot:%s$'
    exclusive: true
  - regex: '^@edupage_.*:%s$'
    exclusive: true
`, brPort, asToken, hsToken, server, server)
	must(t, os.WriteFile(filepath.Join(apps, "edupage.yaml"), []byte(registration), 0o600))
	hsConf := fmt.Sprintf(`[global]
server_name = "%s"
database_path = "%s"
address = ["127.0.0.1"]
port = %d
allow_registration = true
registration_token = "%s"
allow_federation = false
new_user_displayname_suffix = ""
log = "warn"
log_colors = false
appservice_dir = "%s"
`, server, filepath.Join(dir, "hsdb"), hsPort, regToken, apps)
	must(t, os.WriteFile(filepath.Join(dir, "tuwunel.toml"), []byte(hsConf), 0o600))
	hsLog, _ := os.Create(filepath.Join(dir, "tuwunel.log"))
	hs := exec.Command(tuwunel, "-c", filepath.Join(dir, "tuwunel.toml"))
	hs.Stdout, hs.Stderr = hsLog, hsLog
	must(t, hs.Start())
	t.Cleanup(func() { _ = hs.Process.Kill(); _ = hs.Wait() })
	hsURL := fmt.Sprintf("http://127.0.0.1:%d", hsPort)
	if !until(30*time.Second, func() bool {
		resp, err := http.Get(hsURL + "/_matrix/client/versions")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil && resp.StatusCode == 200
	}) {
		t.Fatal("Tuwunel did not start")
	}

	// The bridge talks to the homeserver through a proxy that can refuse to make rooms.
	roomsDown, failing, away, versions, writesAway := &atomic.Bool{}, &sync.Map{}, &atomic.Bool{}, &atomic.Int32{}, &atomic.Bool{}
	checkRefused := &atomic.Bool{}
	target, _ := url.Parse(hsURL)
	rp := httputil.NewSingleHostReverseProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/account_data/io.github.gooder24.edupage.poll") {
			versions.Add(1)
			if checkRefused.Load() {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"refused for the test"}`))
				return
			}
		}
		if away.Load() || writesAway.Load() && r.Method != http.MethodGet {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/createRoom") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			down := roomsDown.Load()
			failing.Range(func(id, tries any) bool {
				if strings.Contains(strings.ToLower(string(body)), id.(string)) {
					tries.(*atomic.Int32).Add(1)
					down = true
				}
				return true
			})
			if down {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"down for the test"}`))
				return
			}
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	c := &mx{t: t, base: hsURL, events: map[string][]map[string]any{}, stop: make(chan struct{})}
	c.register("alex", regToken)
	go c.syncLoop()
	t.Cleanup(func() { close(c.stop) })

	config := fmt.Sprintf(`network:
    edupage_url: %s
    poll_interval: %s
    recent_days: 7
    initial_days: 0
    settle_days: 7
    arrivals: false
    meals: false
    max_file_size: 26214400
bridge:
    personal_filtering_spaces: false
    portal_create_filter:
        mode: deny
        list: ["p:ucitel15"]
    permissions:
        "%s": admin
matrix:
    message_status_events: true
database:
    type: sqlite3-fk-wal
    uri: file:%s?_txlock=immediate
homeserver:
    address: %s
    domain: %s
appservice:
    address: http://127.0.0.1:%d
    hostname: 127.0.0.1
    port: %d
    id: edupage
    bot:
        username: edupagebot
    as_token: %s
    hs_token: %s
    username_template: edupage_{{.}}
provisioning:
    shared_secret: %s
logging:
    min_level: debug
    writers:
    - type: stdout
      format: pretty
`, fake.Template(), poll, c.userID, filepath.Join(dir, "bridge.db"), proxy.URL, server, brPort, brPort, asToken, hsToken, provSecret)
	must(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600))
	brLogPath := filepath.Join(dir, "bridge.log")
	brLog, _ := os.Create(brLogPath)
	br := exec.Command(bin, "-c", filepath.Join(dir, "config.yaml"), "-n")
	br.Stdout, br.Stderr = brLog, brLog
	must(t, br.Start())
	t.Cleanup(func() {
		_ = br.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = br.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = br.Process.Kill()
		}
		data, _ := os.ReadFile(brLogPath)
		if keep := os.Getenv("E2E_LOG"); keep != "" {
			_ = os.WriteFile(keep, data, 0o600)
		}
		if t.Failed() {
			lines := strings.Split(string(data), "\n")
			t.Logf("bridge log (last 60 lines):\n%s", strings.Join(lines[max(0, len(lines)-60):], "\n"))
		}
	})
	e := &env{t: t, fake: fake, c: c, roomsDown: roomsDown, failing: failing, away: away, versions: versions, writesAway: writesAway, checkRefused: checkRefused, db: filepath.Join(dir, "bridge.db"), pid: br.Process.Pid, prov: fmt.Sprintf("http://127.0.0.1:%d/_matrix/provision", brPort), provAuth: provSecret}
	if !until(30*time.Second, func() bool { return e.provision("GET", "/v3/whoami", nil, nil) == nil }) {
		t.Fatal("bridge did not start")
	}
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type loginStep struct {
	LoginID      string `json:"login_id"`
	Type         string `json:"type"`
	StepID       string `json:"step_id"`
	Instructions string `json:"instructions"`
}

// start begins a login of the flow ("password" or "password_code") and sends the credentials.
func (e *env) start(flow, username, password, school string) (*loginStep, error) {
	var first loginStep
	if err := e.provision("POST", "/v3/login/start/"+flow, map[string]any{}, &first); err != nil {
		return nil, err
	}
	return e.step(&first, map[string]string{"username": username, "password": password, "school": school})
}

// step answers a user_input step, or waits on a display_and_wait one (input nil).
func (e *env) step(s *loginStep, input map[string]string) (*loginStep, error) {
	var next loginStep
	kind := "user_input"
	var body any = input
	if input == nil {
		kind, body = "display_and_wait", map[string]any{}
	}
	err := e.provision("POST", "/v3/login/step/"+s.LoginID+"/"+s.StepID+"/"+kind, body, &next)
	return &next, err
}

// sql runs a statement on the bridge's database while the bridge runs.
func (e *env) sql(stmt string) {
	if out, err := exec.Command("sqlite3", "-cmd", ".timeout 5000", e.db, stmt).CombinedOutput(); err != nil {
		e.t.Fatalf("%s: %v %s", stmt, err, out)
	}
}

// pending is how far back the login's next poll reads, as saved in the bridge's database (zero: the usual days).
func (e *env) pending() time.Time {
	out, err := exec.Command("sqlite3", "-cmd", ".timeout 5000", e.db, "SELECT metadata FROM user_login").Output()
	if err != nil {
		e.t.Fatalf("read the bridge's database: %v", err)
	}
	var meta struct {
		Pending int64 `json:"pending"`
	}
	if err = json.Unmarshal(out, &meta); err != nil {
		e.t.Fatalf("login metadata %q: %v", out, err)
	}
	if meta.Pending == 0 {
		return time.Time{}
	}
	return time.Unix(meta.Pending, 0)
}
