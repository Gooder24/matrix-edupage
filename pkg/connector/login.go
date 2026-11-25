package connector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

const (
	LoginFlowPassword = "password"      // a second factor is confirmed in the EduPage app
	LoginFlowCode     = "password_code" // a second factor is a code EduPage mails
	stepCredentials   = "io.github.gooder24.edupage.login.credentials"
	stepConfirm       = "io.github.gooder24.edupage.login.confirm"
	stepCode          = "io.github.gooder24.edupage.login.code"
	stepDone          = "io.github.gooder24.edupage.login.complete"
	confirmWait       = 20 * time.Second
	confirmEvery      = 2 * time.Second
)

func (ec *EduPageConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Username and password",
		Description: "Sign in like on edupage.org; a second factor is confirmed in the EduPage app",
		ID:          LoginFlowPassword,
	}, {
		Name:        "Username, password and mailed code",
		Description: "Sign in like on edupage.org; a second factor is a code EduPage sends by email",
		ID:          LoginFlowCode,
	}}
}

func (ec *EduPageConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowPassword && flowID != LoginFlowCode {
		return nil, fmt.Errorf("unknown login flow %q", flowID)
	}
	return &EduPageLogin{Main: ec, User: user, flow: flowID}, nil
}

// LoginError is a sign-in failure the user can act on.
type LoginError struct{ Code, Message string }

func (e LoginError) Error() string { return e.Message }

func loginError(err error) error {
	var retry edupage.RetryLaterError
	switch {
	case errors.Is(err, edupage.ErrBadCredentials):
		return bridgev2.RespError{ErrCode: "IO.GITHUB.GOODER24.EDUPAGE.BAD_CREDENTIALS", Err: "EduPage did not accept the username or password.", StatusCode: 400}
	case errors.Is(err, edupage.ErrCaptcha):
		return bridgev2.RespError{ErrCode: "IO.GITHUB.GOODER24.EDUPAGE.CAPTCHA", Err: "EduPage wants a captcha. Sign in once on edupage.org in a browser, then try again.", StatusCode: 400}
	case errors.Is(err, edupage.ErrSecondFactor):
		return bridgev2.RespError{ErrCode: "IO.GITHUB.GOODER24.EDUPAGE.SECOND_FACTOR", Err: "EduPage did not accept the confirmation or code. Start signing in again.", StatusCode: 400}
	case errors.As(err, &retry):
		return bridgev2.RespError{ErrCode: "IO.GITHUB.GOODER24.EDUPAGE.RETRY_LATER", Err: fmt.Sprintf("EduPage asks to wait %s before sending another code.", retry.After), StatusCode: 429}
	case err != nil:
		return bridgev2.RespError{ErrCode: "IO.GITHUB.GOODER24.EDUPAGE.UNREACHABLE", Err: "EduPage did not answer. Try again in a moment.", StatusCode: 504}
	}
	return nil
}

type EduPageLogin struct {
	Main     *EduPageConnector
	User     *bridgev2.User
	flow     string
	client   *edupage.Client
	password string
	two      *edupage.TwoFactor
}

var (
	_ bridgev2.LoginProcessUserInput      = (*EduPageLogin)(nil)
	_ bridgev2.LoginProcessDisplayAndWait = (*EduPageLogin)(nil)
)

func (el *EduPageLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       stepCredentials,
		Instructions: "Your EduPage username and password. The school is the first part of its address (myschool for myschool.edupage.org); leave it empty if you do not know it.",
		UserInputParams: &bridgev2.LoginUserInputParams{Fields: []bridgev2.LoginInputDataField{
			{Type: bridgev2.LoginInputFieldTypeUsername, ID: "username", Name: "Username", AutofillDomain: "edupage.org"},
			{Type: bridgev2.LoginInputFieldTypePassword, ID: "password", Name: "Password", AutofillDomain: "edupage.org"},
			{Type: bridgev2.LoginInputFieldTypeDomain, ID: "school", Name: "School", Optional: true, Validate: cleanSchool},
		}},
	}, nil
}

var reSchoolURL = regexp.MustCompile(`(\.edupage\.org.*|^https?://)`)

func cleanSchool(s string) (string, error) {
	s = reSchoolURL.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "")
	if s != "" && !edupage.ValidSchool(s) {
		return "", errors.New("the school is the first part of its address, like myschool")
	}
	return s, nil
}

func (el *EduPageLogin) Cancel() {}

func (el *EduPageLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if el.two != nil {
		if err := el.two.FinishWithCode(ctx, strings.TrimSpace(input["code"])); err != nil {
			return nil, loginError(err)
		}
		return el.complete(ctx)
	}
	username, password := strings.TrimSpace(input["username"]), input["password"]
	if username == "" || password == "" {
		return nil, bridgev2.RespError{ErrCode: "M_BAD_JSON", Err: "Fill in the username and the password.", StatusCode: 400}
	}
	school, err := cleanSchool(input["school"])
	if err != nil {
		return nil, bridgev2.RespError{ErrCode: "M_BAD_JSON", Err: err.Error(), StatusCode: 400}
	}
	el.client = edupage.New(el.Main.Config.EduPageURL, 30*time.Second)
	el.password = password
	two, err := el.client.Login(ctx, username, password, school)
	if err != nil {
		return nil, loginError(err)
	}
	if two == nil {
		return el.complete(ctx)
	}
	el.two = two
	if el.flow == LoginFlowCode {
		return el.askCode(ctx)
	}
	return el.confirmStep(), nil
}

func (el *EduPageLogin) confirmStep() *bridgev2.LoginStep {
	where := "your phone"
	if len(el.two.Devices) > 0 {
		where = strings.Join(el.two.Devices, " or ")
	}
	return &bridgev2.LoginStep{
		Type:                 bridgev2.LoginStepTypeDisplayAndWait,
		StepID:               stepConfirm,
		Instructions:         "EduPage wants this sign-in confirmed. Confirm it in the EduPage app on " + where + ".",
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{Type: bridgev2.LoginDisplayTypeNothing},
	}
}

// askCode has EduPage mail a code, waiting out its countdown (about 30 s after the password).
func (el *EduPageLogin) askCode(ctx context.Context) (*bridgev2.LoginStep, error) {
	for range 4 {
		err := el.two.SendEmail(ctx)
		var retry edupage.RetryLaterError
		if !errors.As(err, &retry) || retry.After > time.Minute {
			if err != nil {
				return nil, loginError(err)
			}
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retry.After):
		}
	}
	where := "your email"
	if el.two.Email != "" {
		where = el.two.Email
	}
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       stepCode,
		Instructions: "EduPage sent a code to " + where + ". It is valid for 5 minutes.",
		UserInputParams: &bridgev2.LoginUserInputParams{Fields: []bridgev2.LoginInputDataField{
			{Type: bridgev2.LoginInputFieldType2FACode, ID: "code", Name: "Code"},
		}},
	}, nil
}

// Wait checks for the app's confirmation for a while; without it the same step comes back.
func (el *EduPageLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	if el.two == nil {
		return nil, errors.New("no sign-in is waiting for a confirmation")
	}
	deadline := time.Now().Add(confirmWait)
	for time.Now().Before(deadline) {
		ok, err := el.two.Confirmed(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, loginError(err)
		}
		if ok {
			if err = el.two.Finish(ctx); err != nil {
				return nil, loginError(err)
			}
			return el.complete(ctx)
		}
		select {
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), bridgev2.ErrLoginStepCancelled) {
				return nil, bridgev2.ErrLoginStepCancelled
			}
			return nil, ctx.Err()
		case <-time.After(confirmEvery):
		}
	}
	return el.confirmStep(), nil
}

func (el *EduPageLogin) complete(ctx context.Context) (*bridgev2.LoginStep, error) {
	c := el.client
	if c.UserID() == "" {
		return nil, loginError(edupage.ErrBadCredentials)
	}
	name := c.MyName()
	meta := &UserLoginMetadata{School: c.School, Username: c.Username, Password: el.password, Session: c.Session(), UserID: c.UserID()}
	id := loginID(c.School, c.UserID())
	if old, _ := el.Main.Bridge.GetExistingUserLoginByID(ctx, id); old != nil {
		backfilled, pending := old.Metadata.(*UserLoginMetadata).progress()
		meta.Backfilled = backfilled
		if !pending.IsZero() {
			meta.Pending = pending.Unix()
		}
	}
	ul, err := el.User.NewLogin(ctx, &database.UserLogin{
		ID:            id,
		RemoteName:    name,
		RemoteProfile: status.RemoteProfile{Name: name, Username: c.Username},
		Metadata:      meta,
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			login.Client = &EduPageClient{Main: el.Main, UserLogin: login, edu: c}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save the login: %w", err)
	}
	ul.Client.Connect(ul.Log.WithContext(context.Background()))
	return &bridgev2.LoginStep{
		Type:           bridgev2.LoginStepTypeComplete,
		StepID:         stepDone,
		Instructions:   "Signed in as " + name,
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: ul.ID, UserLogin: ul},
	}, nil
}

// progress says whether the first read was made and how far back the next poll reads.
func (m *UserLoginMetadata) progress() (backfilled bool, pending time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Pending != 0 {
		pending = time.Unix(m.Pending, 0).In(edupage.Location)
	}
	return m.Backfilled, pending
}
