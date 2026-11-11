package archive

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	cstorage "github.com/iyear/tdl/core/storage"
	tclientcore "github.com/iyear/tdl/core/tclient"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/spf13/viper"
)

const (
	// maxCodeAttempts bounds wrong-code retries on one code hash. Telegram
	// starts throttling well before this, so the limit only exists to stop a
	// stuck UI from looping forever.
	maxCodeAttempts = 3
	// maxPasswordAttempts bounds wrong 2FA retries. Cloud passwords are typed
	// by hand and typos are the normal case, so this is a little more generous.
	maxPasswordAttempts = 3
	// phoneLoginTTL bounds one phone-login session end to end (send code, code
	// entry, optional 2FA). Telegram's login code lives for about five minutes;
	// without this bound a flow whose modal got closed mid-wait blocks every
	// later attempt with "another login is running" forever, because nothing
	// ever releases qrCancel.
	phoneLoginTTL = 10 * time.Minute
)

// StartPhoneLogin sends a login code to the given phone number and starts the
// phone login flow on a dedicated client, mirroring StartQRLogin.
//
// The code is delivered by Telegram to the user's other sessions (or SMS), so
// this call only covers "send"; SubmitLoginCode then completes it.
func (c *Controller) StartPhoneLogin(phone string) error {
	phone = normalizePhone(phone)
	if phone == "" {
		return errors.New("请填写手机号（含国际区号，如 +8613800138000）")
	}
	if err := c.checkCreds(); err != nil {
		return err
	}
	if st := c.WaitState(15 * time.Second); st.Authorized {
		return errors.New("already logged in")
	}

	c.loginMu.Lock()
	if c.qrCancel != nil {
		// A previous flow must not lock the user out of retrying. The usual
		// cause is a modal closed while waiting for a code: the UI resets but
		// the goroutine stays blocked, so every later attempt is refused.
		// Re-sending for the SAME number while its code is still out is not an
		// error the user can act on, and re-requesting would burn a Telegram
		// rate-limit slot; report it as already-sent instead.
		sameNumber := c.pendingCodeFor(phone)
		c.loginMu.Unlock()
		if sameNumber {
			return errors.New("验证码已发送，请直接输入收到的验证码；要换号码或重发，请先点「取消」")
		}
		c.stopQR()
		c.loginMu.Lock()
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.loginGen++
	gen := c.loginGen
	c.qrCancel = cancel
	c.qrPwd = make(chan string, 1)
	c.codeCh = make(chan string, 1)
	c.codeSent = false
	c.passwordMode = false
	c.loginKind = loginKindPhone
	c.loginPhone = phone
	c.qrErr = ""
	c.loginMu.Unlock()
	c.qrToken.Store("")

	go c.runPhoneLogin(ctx, phone, gen)
	return nil
}

// pendingCodeFor reports whether the running flow is waiting for a code that
// was already sent to this exact number. Callers must hold loginMu.
//
// This is the difference between "the user double-clicked send" (harmless, just
// tell them to type the code) and "the user is trying a different number after
// abandoning the last attempt" (the stale flow has to be torn down, otherwise
// it blocks every retry forever).
func (c *Controller) pendingCodeFor(phone string) bool {
	return c.loginKind == loginKindPhone && c.loginPhone == phone && c.codeSent
}

func (c *Controller) runPhoneLogin(ctx context.Context, phone string, gen uint64) {
	// Bound the whole session so an abandoned flow releases qrCancel instead of
	// blocking every later attempt. The deferred cleanup below runs on timeout
	// just like on success or cancel.
	ctx, ttlCancel := context.WithTimeout(ctx, phoneLoginTTL)
	defer ttlCancel()
	defer c.finishLoginFlow(gen)

	d := tg.NewUpdateDispatcher()
	lc, err := tclientcore.New(ctx, tclientcore.Options{
		AppID: c.AppID, AppHash: c.AppHash,
		Session:          cstorage.NewSession(c.KV, false),
		Proxy:            viper.GetString(consts.FlagProxy),
		ProxyInsecure:    viper.GetBool(consts.FlagProxyInsecure),
		ReconnectTimeout: viper.GetDuration(consts.FlagReconnectTimeout),
		UpdateHandler:    d,
	})
	if err != nil {
		c.setQRError(loginErrText(err))
		return
	}

	err = lc.Run(ctx, func(ctx context.Context) error {
		if st, err := lc.Auth().Status(ctx); err == nil && st.Authorized {
			// The KV session was already authorized — nothing left to do.
			return nil
		}
		return c.phoneSignIn(ctx, lc, phone)
	})
	if err != nil && ctx.Err() == nil && c.State().State != StateAuthorized {
		c.setQRError(loginErrText(err))
	}
	// Surface a timeout instead of silently dropping back to idle: the user was
	// waiting on a code that can no longer be submitted.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && c.State().State != StateAuthorized {
		c.setQRError("登录超时（验证码已失效），请重新发送验证码")
	}
	if c.State().Authorized {
		c.kick()
		c.ensureSupervisor() // reconnect with the authorized session
	}
}

// phoneSignIn drives send-code → sign-in → optional 2FA inside a single client
// run, so all three steps share one MTProto connection.
func (c *Controller) phoneSignIn(ctx context.Context, lc *telegram.Client, phone string) error {
	sent, err := lc.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return phoneError(err, "发送验证码")
	}
	sc, ok := sent.(*tg.AuthSentCode)
	if !ok {
		// AuthSentCodeSuccess: this client is already authorized.
		if _, done := sent.(*tg.AuthSentCodeSuccess); done {
			return nil
		}
		return errors.Errorf("无法向该号码发送验证码（%T），请改用扫码登录", sent)
	}

	if err := c.awaitCodeAndSignIn(ctx, lc, phone, sc.PhoneCodeHash); err != nil {
		return err
	}

	user, err := lc.Self(ctx)
	if err != nil {
		return err
	}
	c.setState(CtrlState{State: StateAuthorized, Authorized: true, User: userFromTG(user)})
	return nil
}

// awaitCodeAndSignIn waits for codes from the UI and retries a bounded number of
// times on a wrong code, keeping the same phoneCodeHash.
func (c *Controller) awaitCodeAndSignIn(ctx context.Context, lc *telegram.Client, phone, hash string) error {
	c.setCodeSent(true)

	for attempt := 1; attempt <= maxCodeAttempts; attempt++ {
		var code string
		select {
		case code = <-c.codeCh:
		case <-ctx.Done():
			return ctx.Err()
		}
		c.setCodeSent(false)

		_, err := lc.Auth().SignIn(ctx, phone, code, hash)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, auth.ErrPasswordAuthNeeded):
			return c.awaitPassword(ctx, lc)
		}

		var signUp *auth.SignUpRequired
		if errors.As(err, &signUp) {
			return errors.New("该手机号还没有 Telegram 账号，请先在手机上注册")
		}
		switch {
		case tgerr.Is(err, "PHONE_CODE_INVALID", "PHONE_CODE_EMPTY"):
			if attempt == maxCodeAttempts {
				return errors.New("验证码错误次数过多，请重新获取验证码")
			}
			// Keep the flow alive: the same code hash accepts another attempt.
			c.setQRError("验证码错误，请重新输入（" + strconv.Itoa(attempt) + "/" + strconv.Itoa(maxCodeAttempts) + "）")
			c.setCodeSent(true)
			continue
		case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
			return errors.New("验证码已过期，请重新获取")
		case tgerr.Is(err, "PHONE_NUMBER_INVALID"):
			return errors.New("手机号格式不正确，请带上国际区号")
		case tgerr.Is(err, "PHONE_NUMBER_BANNED"):
			return errors.New("该手机号已被 Telegram 封禁")
		case tgerr.Is(err, "PHONE_NUMBER_UNOCCUPIED"):
			return errors.New("该手机号还没有 Telegram 账号，请先在手机上注册")
		}
		return phoneError(err, "登录")
	}
	return errors.New("验证码错误次数过多，请重新获取验证码")
}

// awaitPassword handles the 2FA step, allowing retries because a mistyped cloud
// password should not force the user to restart the whole login.
func (c *Controller) awaitPassword(ctx context.Context, lc *telegram.Client) error {
	for attempt := 1; attempt <= maxPasswordAttempts; attempt++ {
		c.setPasswordMode(true)

		var pwd string
		select {
		case pwd = <-c.qrPwd:
		case <-ctx.Done():
			c.setPasswordMode(false)
			return ctx.Err()
		}
		c.setPasswordMode(false)

		_, err := lc.Auth().Password(ctx, pwd)
		if err == nil {
			return nil
		}
		if errors.Is(err, auth.ErrPasswordInvalid) {
			if attempt == maxPasswordAttempts {
				return errors.New("两步验证密码错误次数过多")
			}
			c.setQRError("两步验证密码错误，请重试（" + strconv.Itoa(attempt) + "/" + strconv.Itoa(maxPasswordAttempts) + "）")
			continue
		}
		return phoneError(err, "两步验证")
	}
	return errors.New("两步验证密码错误次数过多")
}

// SubmitLoginCode hands a code received from Telegram to the running phone
// login flow.
func (c *Controller) SubmitLoginCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("验证码不能为空")
	}
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.qrCancel == nil || c.loginKind != loginKindPhone || !c.codeSent {
		return errors.New("当前没有等待验证码的登录流程")
	}
	select {
	case c.codeCh <- code:
		return nil
	default:
		return errors.New("验证码已提交，请稍候")
	}
}

// ImportSession installs a Telethon-format StringSession, replacing whatever
// session is in the KV store, then reconnects.
//
// This is the escape hatch for accounts where interactive login is blocked
// (no access to the phone, a code Telegram refuses to send, or a datacenter the
// login flow cannot reach).
func (c *Controller) ImportSession(sess string) error {
	sess = strings.TrimSpace(sess)
	if sess == "" {
		return errors.New("session 不能为空")
	}
	data, err := session.TelethonSession(sess)
	if err != nil {
		return errors.Errorf("session 解析失败：%v", err)
	}
	if len(data.AuthKey) == 0 || data.DC == 0 || data.Addr == "" {
		return errors.New("session 内容不完整（缺少 auth key / DC / 地址）")
	}

	c.loginMu.Lock()
	running := c.qrCancel != nil
	c.loginMu.Unlock()
	if running {
		return errors.New("登录流程正在进行，请先取消")
	}

	ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()
	loader := &session.Loader{Storage: cstorage.NewSession(c.KV, false)}
	if err := loader.Save(ctx, data); err != nil {
		return errors.Wrap(err, "保存 session")
	}

	c.kick()
	c.ensureSupervisor()
	c.setState(CtrlState{State: StateConnecting})
	return nil
}

func (c *Controller) setCodeSent(v bool) {
	c.loginMu.Lock()
	c.codeSent = v
	c.loginMu.Unlock()
}

func (c *Controller) setPasswordMode(v bool) {
	c.loginMu.Lock()
	c.passwordMode = v
	c.loginMu.Unlock()
}

// phoneError turns a Telegram RPC failure into something a user can act on.
func phoneError(err error, stage string) error {
	switch {
	case tgerr.IsCode(err, 420):
		return errors.Errorf("%s失败：操作过于频繁，请稍后再试", stage)
	case tgerr.Is(err, "PHONE_NUMBER_INVALID"):
		return errors.New("手机号格式不正确，请带上国际区号")
	case tgerr.Is(err, "API_ID_INVALID", "API_ID_PUBLISHED_FLOOD"):
		return errors.New("内置 API 凭据被限流，请到设置里换成自己的 api_id / api_hash")
	case tgerr.Is(err, "AUTH_RESTART"):
		return errors.New("登录状态异常，请重新发起登录")
	}
	return errors.Wrap(err, stage)
}

// normalizePhone strips the formatting people paste from their contacts app and
// keeps a single leading "+".
func normalizePhone(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && b.Len() == 0:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "+" || out == "" {
		return ""
	}
	// Telegram always wants the country code; a bare local number cannot work.
	if digits := strings.TrimPrefix(out, "+"); len(digits) < 6 {
		return ""
	}
	return out
}

// maskPhone keeps the country/prefix and the last two digits: +8613******00.
func maskPhone(phone string) string {
	if phone == "" {
		return ""
	}
	if len(phone) <= 5 {
		return phone
	}
	return phone[:4] + strings.Repeat("*", len(phone)-6) + phone[len(phone)-2:]
}

// loginErrText strips the transport wrapper that telegram.Client.Run puts in
// front of callback errors. Users should read "手机号格式不正确", not
// "callback: 手机号格式不正确".
func loginErrText(err error) string {
	s := err.Error()
	for _, prefix := range []string{"callback: ", "run: ", "auth: "} {
		s = strings.TrimPrefix(s, prefix)
	}
	return s
}
