package edupage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Login signs in like edupage.org's login page (mainlogin.js). An empty school signs in through
// login1, which sends the user on to their own school. A *TwoFactor result means EduPage wants the
// second factor before the session works.
func (c *Client) Login(ctx context.Context, username, password, school string) (*TwoFactor, error) {
	if school == "" {
		school = "login1"
	}
	c.School, c.Username = school, username
	resp, err := c.loginRPC(ctx, username, password)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		if resp, err = c.loginLegacy(ctx, username, password); err != nil {
			return nil, err
		}
	}
	return c.finishLogin(ctx, resp)
}

type rpcLogin struct {
	Token       string `json:"token"`
	RedirectURL string `json:"redirectUrl"`
	Status      string `json:"status"`
	Err         *struct {
		ErrorID string `json:"error_id"`
	} `json:"err"`
}

// loginRPC is the current login (getToken, then login). A nil response means "use the old form".
func (c *Client) loginRPC(ctx context.Context, username, password string) (*http.Response, error) {
	base := c.Base()
	resp, _, err := c.get(ctx, base+"/login/?cmd=MainLogin")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var tok rpcLogin
	if c.rpc(ctx, base+"/login/?cmd=MainLogin&akcia=getToken", map[string]any{"username": username, "edupage": ""}, &tok) != nil || tok.Token == "" {
		return nil, nil
	}
	var answer rpcLogin
	err = c.rpc(ctx, base+"/login/?cmd=MainLogin&akcia=login", map[string]any{
		"username": username, "password": password, "userToken": tok.Token,
		"edupage": "", "ctxt": "", "tu": nil, "gu": nil, "au": nil,
	}, &answer)
	if err != nil || answer.RedirectURL == "" || (answer.Err != nil && answer.Err.ErrorID == "invalid_token") {
		return nil, nil
	}
	next, err := url.Parse(base + "/")
	if err != nil {
		return nil, err
	}
	target, err := next.Parse(answer.RedirectURL)
	if err != nil {
		return nil, err
	}
	resp, page, err := c.get(ctx, target.String())
	if err != nil {
		return nil, err
	}
	resp.Body = nopBody(page)
	return resp, nil
}

// loginLegacy is edubarLogin.php, which EduPage still answers when the RPC login refuses.
func (c *Client) loginLegacy(ctx context.Context, username, password string) (*http.Response, error) {
	base := c.Base()
	_, page, err := c.get(ctx, base+"/login/?cmd=MainLogin")
	if err != nil {
		return nil, err
	}
	_, after, ok := strings.Cut(string(page), `"csrftoken":"`)
	if !ok {
		return nil, ErrBadCredentials
	}
	csrf, _, _ := strings.Cut(after, `"`)
	form := url.Values{"csrfauth": {csrf}, "username": {username}, "password": {password}}
	resp, body, err := c.postForm(ctx, base+"/login/edubarLogin.php", form.Encode())
	if err != nil {
		return nil, err
	}
	final := resp.Request.URL.String()
	switch {
	case strings.Contains(final, "cap=1") || strings.Contains(final, "lerr=b43b43"):
		return nil, ErrCaptcha
	case strings.Contains(final, "bad=1"):
		return nil, ErrBadCredentials
	}
	resp.Body = nopBody(body)
	return resp, nil
}

func (c *Client) finishLogin(ctx context.Context, resp *http.Response) (*TwoFactor, error) {
	if c.School == "login1" {
		if school := c.schoolOf(resp.Request.URL); school != "" {
			c.School = school
		}
	}
	page := readBody(resp)
	if !strings.Contains(resp.Request.URL.String(), "twofactor") {
		return nil, c.parseUserHome(page)
	}
	_, twoFactorPage, err := c.get(ctx, c.Base()+"/login/twofactor?sn=1")
	if err != nil {
		return nil, err
	}
	return c.twoFactorFrom(ctx, string(twoFactorPage))
}

// TwoFactor is a sign-in waiting for its second factor: a confirmation in the EduPage app or a code.
type TwoFactor struct {
	c       *Client
	modern  bool
	gu, au  string
	csrf    string
	code    string
	Email   string
	Devices []string
}

func field(page, name string) string {
	_, after, ok := strings.Cut(page, name+`" value="`)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(after, `"`)
	return v
}

func (c *Client) twoFactorFrom(ctx context.Context, page string) (*TwoFactor, error) {
	if csrf, au, gu := field(page, "csrfauth"), field(page, "au"), field(page, "gu"); csrf != "" && au != "" && gu != "" {
		return &TwoFactor{c: c, csrf: csrf, au: au, gu: gu}, nil
	}
	if !strings.Contains(page, "/login/pics/jsw/twofactorlogin.js") {
		return nil, ErrBadCredentials
	}
	tf := &TwoFactor{c: c, modern: true}
	_, data, err := c.postForm(ctx, c.Base()+"/login/twofactor?akcia=getData", "")
	if err != nil {
		return nil, err
	}
	var info struct {
		Email       string   `json:"email"`
		DeviceNames []string `json:"deviceNames"`
	}
	if json.Unmarshal(data, &info) == nil {
		tf.Email = info.Email
		for _, d := range info.DeviceNames {
			if d != "" {
				tf.Devices = append(tf.Devices, d)
			}
		}
	}
	return tf, nil
}

type twoFactorAnswer struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

func (a twoFactorAnswer) retryLater() error {
	var d struct {
		RetryInSeconds int `json:"retryInSeconds"`
	}
	if json.Unmarshal(a.Data, &d) == nil && d.RetryInSeconds > 0 {
		return RetryLaterError{After: time.Duration(d.RetryInSeconds) * time.Second}
	}
	return ErrSecondFactor
}

func (t *TwoFactor) call(ctx context.Context, action string) (twoFactorAnswer, error) {
	var answer twoFactorAnswer
	_, data, err := t.c.postForm(ctx, t.c.Base()+"/login/twofactor?akcia="+action, "")
	if err != nil {
		return answer, err
	}
	if err = json.Unmarshal(data, &answer); err != nil {
		return answer, ErrSecondFactor
	}
	return answer, nil
}

// Confirmed asks whether the sign-in was confirmed in the EduPage app.
func (t *TwoFactor) Confirmed(ctx context.Context) (bool, error) {
	answer, err := t.call(ctx, "checkIfConfirmed")
	switch {
	case err != nil:
		return false, err
	case answer.Status == "fail":
		return false, nil
	case answer.Status != "ok":
		return false, ErrSecondFactor
	}
	if json.Unmarshal(answer.Data, &t.code) != nil || t.code == "" {
		return false, ErrSecondFactor
	}
	return true, nil
}

// SendEmail asks EduPage to mail a code. Until its countdown ends it answers RetryLaterError.
func (t *TwoFactor) SendEmail(ctx context.Context) error {
	answer, err := t.call(ctx, "sendEmail")
	if err != nil {
		return err
	}
	if answer.Status != "ok" {
		return answer.retryLater()
	}
	var d struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(answer.Data, &d) == nil && d.Email != "" {
		t.Email = d.Email
	}
	return nil
}

// Finish completes a sign-in confirmed in the app (after Confirmed returned true).
func (t *TwoFactor) Finish(ctx context.Context) error {
	if t.code == "" {
		return ErrNotConfirmed
	}
	return t.FinishWithCode(ctx, t.code)
}

// FinishWithCode completes the sign-in with a code from the mail or the app.
func (t *TwoFactor) FinishWithCode(ctx context.Context, code string) error {
	c := t.c
	if t.modern {
		var answer rpcLogin
		err := c.rpc(ctx, c.Base()+"/login/?cmd=MainLogin&akcia=login", map[string]any{
			"t2fasec": code, "2fNoSave": "y", "2fform": "1", "tu": nil, "gu": nil, "au": nil,
		}, &answer)
		if err != nil || answer.Status != "OK" {
			return ErrSecondFactor
		}
		redirect := answer.RedirectURL
		if redirect == "" {
			redirect = "/user/"
		}
		base, _ := url.Parse(c.Base() + "/")
		target, err := base.Parse(redirect)
		if err != nil {
			return err
		}
		resp, page, err := c.get(ctx, target.String())
		if err != nil {
			return err
		}
		if school := c.schoolOf(resp.Request.URL); school != "" {
			c.School = school // login1 sends the user on to their school only now
		}
		return c.parseUserHome(string(page))
	}
	form := url.Values{"csrfauth": {t.csrf}, "t2fasec": {code}, "2fNoSave": {"y"}, "2fform": {"1"}, "gu": {t.gu}, "au": {t.au}}
	_, body, err := c.postForm(ctx, c.Base()+"/login/edubarLogin.php", form.Encode())
	if err != nil {
		return err
	}
	if !strings.Contains(string(body), "window.location = gu;") {
		return ErrSecondFactor
	}
	_, page, err := c.get(ctx, c.Base()+"/user")
	if err != nil {
		return err
	}
	return c.parseUserHome(string(page))
}
