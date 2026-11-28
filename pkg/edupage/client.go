// Package edupage speaks the parts of edupage.org's web API that its own pages use for signing in,
// the timeline (messages and notices) and writing messages. It follows the edupage-api Python library
// (github.com/EdupageAPI/edupage-api), which reads the same scripts.
package edupage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var (
	ErrBadCredentials = errors.New("edupage did not accept the username or password")
	ErrCaptcha        = errors.New("edupage wants a captcha solved in a browser first")
	ErrSessionExpired = errors.New("the edupage session expired")
	ErrSecondFactor   = errors.New("edupage did not accept the second factor")
	ErrNotConfirmed   = errors.New("the sign-in was not confirmed yet")
)

// RetryLaterError is EduPage's countdown before it sends another code.
type RetryLaterError struct{ After time.Duration }

func (e RetryLaterError) Error() string { return fmt.Sprintf("edupage asks to retry in %s", e.After) }

// Client is one signed-in EduPage session.
type Client struct {
	// Template is the school's address with {school} in it, https://{school}.edupage.org outside tests.
	Template string
	School   string
	Username string
	// Data is the userhome object edupage.org's pages start with: the user, the school's people and more.
	Data     map[string]any
	GsecHash string
	HTTP     *http.Client
	jar      *cookiejar.Jar
	re       *regexp.Regexp
}

const DefaultTemplate = "https://{school}.edupage.org"

func New(template string, timeout time.Duration) *Client {
	if template == "" {
		template = DefaultTemplate
	}
	jar, _ := cookiejar.New(nil)
	pattern := "^" + strings.ReplaceAll(regexp.QuoteMeta(template), regexp.QuoteMeta("{school}"), "([a-z0-9-]+)") + `(?:[/?#]|$)`
	return &Client{
		Template: template,
		HTTP:     &http.Client{Timeout: timeout, Jar: jar},
		jar:      jar,
		re:       regexp.MustCompile(pattern),
	}
}

var reSchool = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// ValidSchool reports whether s can be the first part of a school's edupage.org address.
func ValidSchool(s string) bool { return reSchool.MatchString(s) }

func (c *Client) Base() string { return strings.ReplaceAll(c.Template, "{school}", c.School) }

// schoolOf reads the school back out of an address on one of its pages.
func (c *Client) schoolOf(u *url.URL) string {
	if m := c.re.FindStringSubmatch(u.String()); m != nil {
		return m[1]
	}
	return ""
}

// SameSite reports whether addr is on the signed-in school's pages, where the session cookie may go.
func (c *Client) SameSite(addr string) bool {
	u, err := url.Parse(addr)
	return err == nil && c.School != "" && c.schoolOf(u) == c.School
}

func (c *Client) do(ctx context.Context, method, addr, contentType string, body io.Reader) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, addr, body)
	if err != nil {
		return nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 500 {
		return resp, data, fmt.Errorf("edupage answered %d", resp.StatusCode)
	}
	return resp, data, nil
}

func (c *Client) get(ctx context.Context, addr string) (*http.Response, []byte, error) {
	return c.do(ctx, http.MethodGet, addr, "", nil)
}

func (c *Client) postForm(ctx context.Context, addr, form string) (*http.Response, []byte, error) {
	return c.do(ctx, http.MethodPost, addr, "application/x-www-form-urlencoded", strings.NewReader(form))
}

// rpc posts an EduPage RPC call (a compressed rpcparams form) and decodes the JSON answer.
func (c *Client) rpc(ctx context.Context, addr string, params any, out any) error {
	_, data, err := c.postForm(ctx, addr, EncodeRequestBody([]Pair{{"rpcparams", pyJSON(params)}}))
	if err != nil {
		return err
	}
	decoded, err := DecodeResponse(string(data))
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, out)
}

// Session is the PHPSESSID cookie of the school's pages; with Resume it signs in again without a password.
func (c *Client) Session() string {
	u, err := url.Parse(c.Base() + "/")
	if err != nil {
		return ""
	}
	for _, ck := range c.jar.Cookies(u) {
		if ck.Name == "PHPSESSID" {
			return ck.Value
		}
	}
	return ""
}

// Resume signs in with a stored session cookie.
func (c *Client) Resume(ctx context.Context, school, session, username string) error {
	c.School, c.Username = school, username
	u, err := url.Parse(c.Base() + "/")
	if err != nil {
		return err
	}
	c.jar.SetCookies(u, []*http.Cookie{{Name: "PHPSESSID", Value: session, Path: "/"}})
	_, page, err := c.get(ctx, c.Base()+"/user")
	if err != nil {
		return err
	}
	if err = c.parseUserHome(string(page)); err != nil {
		return ErrSessionExpired
	}
	return nil
}

// parseUserHome reads the userhome(...) call every signed-in page starts with.
func (c *Client) parseUserHome(page string) error {
	_, rest, ok := strings.Cut(page, "userhome(")
	if !ok {
		return ErrBadCredentials
	}
	// Like edupage-api: the object ends before the second-to-last ");" of the page.
	end := strings.LastIndex(rest, ");")
	if end < 0 {
		return ErrBadCredentials
	}
	if prev := strings.LastIndex(rest[:end], ");"); prev >= 0 {
		end = prev
	}
	js := strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(rest[:end])
	var data map[string]any
	if err := json.Unmarshal([]byte(js), &data); err != nil {
		return fmt.Errorf("%w: %v", ErrBadCredentials, err)
	}
	c.Data = data
	if _, after, ok := strings.Cut(page, `ASC.gsechash="`); ok {
		c.GsecHash, _, _ = strings.Cut(after, `"`)
	}
	return nil
}

// UserID is EduPage's id of the signed-in person, like Student123.
func (c *Client) UserID() string {
	s, _ := c.Data["userid"].(string)
	return s
}

var reUserID = regexp.MustCompile(`^([A-Za-z]+?)(?:Only)?(-?\d+)$`)

// Person looks someone up in the school's people (dbi): their name, and for teachers when they left.
func (c *Client) Person(id string) (name string, ok bool) {
	m := reUserID.FindStringSubmatch(id)
	if m == nil {
		return "", false
	}
	group := map[string]string{"Student": "students", "Ucitel": "teachers", "Rodic": "parents"}[m[1]]
	dbi, _ := c.Data["dbi"].(map[string]any)
	people, _ := dbi[group].(map[string]any)
	p, _ := people[m[2]].(map[string]any)
	if p == nil {
		return "", false
	}
	first, _ := p["firstname"].(string)
	last, _ := p["lastname"].(string)
	name = strings.TrimSpace(first + " " + last)
	return name, name != ""
}

// MyName is the signed-in person's name, else the username.
func (c *Client) MyName() string {
	if name, ok := c.Person(c.UserID()); ok {
		return name
	}
	return c.Username
}

// Contact is someone of the school the user can write to.
type Contact struct{ ID, Name string }

// Teachers lists the school's current teachers.
func (c *Client) Teachers() []Contact {
	dbi, _ := c.Data["dbi"].(map[string]any)
	teachers, _ := dbi["teachers"].(map[string]any)
	today := time.Now().In(Location).Format(time.DateOnly)
	var out []Contact
	for id, raw := range teachers {
		t, _ := raw.(map[string]any)
		if t == nil || !regexp.MustCompile(`^\d+$`).MatchString(id) {
			continue
		}
		if to, _ := t["dateto"].(string); to != "" && to < today {
			continue
		}
		if name, ok := c.Person("Ucitel" + id); ok {
			out = append(out, Contact{ID: "Ucitel" + id, Name: name})
		}
	}
	return out
}

// Logout ends the session on EduPage.
func (c *Client) Logout(ctx context.Context) {
	if c.School != "" {
		_, _, _ = c.postForm(ctx, c.Base()+"/login/logout.php", "")
	}
}

// Upload stores a file the way edupage.org's message form does and returns its path and name on EduPage.
func (c *Client) Upload(ctx context.Context, name, mimeType string, data []byte) (path, stored string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="att"; filename=%q`, name))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	h.Set("Content-Type", mimeType)
	part, _ := w.CreatePart(h)
	_, _ = part.Write(data)
	_ = w.Close()
	_, body, err := c.do(ctx, http.MethodPost, c.Base()+"/timeline/?akcia=uploadAtt", w.FormDataContentType(), &buf)
	if err != nil {
		return "", "", err
	}
	var answer struct {
		Status string `json:"status"`
		Data   struct {
			File string `json:"file"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return "", "", ErrSessionExpired
	}
	if answer.Status != "ok" || answer.Data.File == "" {
		return "", "", fmt.Errorf("edupage did not take the file %s", name)
	}
	if answer.Data.Name != "" {
		name = answer.Data.Name
	}
	return answer.Data.File, name, nil
}

type changes struct {
	Changes []struct {
		TimelineID json.Number `json:"timelineid"`
	} `json:"changes"`
}

func (c *Client) created(data []byte) (int64, error) {
	decoded, err := DecodeResponse(string(data))
	if err != nil {
		return 0, err
	}
	var ch changes
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.UseNumber()
	if dec.Decode(&ch) != nil {
		return 0, ErrSessionExpired
	}
	for _, change := range ch.Changes {
		if id, err := change.TimelineID.Int64(); err == nil {
			return id, nil
		}
	}
	return 0, errors.New("edupage did not accept the message")
}

// SendMessage starts a new message, like edupage.org's "New message", and returns its id. to is one
// recipient id or several joined by ";" (see Recipients), attachments maps each uploaded file's EduPage
// path to its name, and opts are a teacher's choices (nil for everyone else).
func (c *Client) SendMessage(ctx context.Context, to, text string, attachments map[string]string, opts *Options) (int64, error) {
	if attachments == nil {
		attachments = map[string]string{}
	}
	fields := append([]Pair{{"selectedUser", to}, {"text", text}, {"attachements", pyJSON(attachments)}}, opts.fields()...)
	_, data, err := c.postForm(ctx, c.Base()+"/timeline/?=&akcia=createItem&eqav=1&maxEqav=7", EncodeRequestBody(append(fields, Pair{"typ", "sprava"})))
	if err != nil {
		return 0, err
	}
	return c.created(data)
}

// SendReply answers in the thread whose first message is root and returns the new message's id.
func (c *Client) SendReply(ctx context.Context, root int64, text string, attachments map[string]string) (int64, error) {
	if attachments == nil {
		attachments = map[string]string{}
	}
	_, data, err := c.postForm(ctx, c.Base()+"/timeline/?akcia=createReply&eqav=1&maxEqav=7", EncodeRequestBody([]Pair{
		{"groupid", fmt.Sprint(root)}, {"recipient", ""}, {"text", text},
		{"moredata", pyJSON(map[string]any{"attachements": attachments})},
	}))
	if err != nil {
		return 0, err
	}
	return c.created(data)
}

// Download reads a file from the school's pages with the session. Other addresses are refused.
func (c *Client) Download(ctx context.Context, addr string, limit int64) ([]byte, string, error) {
	if !c.SameSite(addr) {
		return nil, "", fmt.Errorf("not an address of the school: %s", addr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, "", err
	}
	// A redirect may not lead the session's request, or the bytes, off the school's own pages.
	client := *c.HTTP
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if r.URL.Host != req.URL.Host || !c.SameSite(r.URL.String()) {
			return fmt.Errorf("redirected off the school's pages: %s", r.URL.Host)
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("edupage answered %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("the file is larger than %d bytes", limit)
	}
	mimeType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "text/html" {
		return nil, "", ErrSessionExpired // the login page instead of the file
	}
	if fixed, ok := map[string]string{"image/x-png": "image/png", "image/pjpeg": "image/jpeg"}[mimeType]; ok {
		mimeType = fixed
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return data, mimeType, nil
}

type bodyBytes struct{ *strings.Reader }

func (bodyBytes) Close() error { return nil }

func nopBody(data []byte) io.ReadCloser { return bodyBytes{strings.NewReader(string(data))} }

func readBody(resp *http.Response) string {
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

// Fold removes diacritics, so "Novák" is found by "novak".
func Fold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
