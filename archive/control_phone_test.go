package archive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The phone login flow is a three-step conversation (send code → code → 2FA)
// driven by channels. These tests cover the parts that do not need a live
// Telegram connection: input normalisation, state reporting, and the guard rails
// that keep a stale UI from pushing input into the wrong flow.

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"+8613800138000", "+8613800138000"},
		{"+86 138 0013 8000", "+8613800138000"},
		{"+86-138-0013-8000", "+8613800138000"},
		{"  +8613800138000  ", "+8613800138000"},
		{"13800138000", "13800138000"},
		{"+", ""},
		{"", ""},
		{"abc", ""},
		{"+123", ""},                          // too short to carry a country code and a number
		{"++8613800138000", "+8613800138000"}, // only the first + survives
	}
	for _, tc := range cases {
		if got := normalizePhone(tc.in); got != tc.want {
			t.Errorf("normalizePhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"+8613", "+8613"},
		{"+8613800138000", "+861********00"},
	}
	for _, tc := range cases {
		if got := maskPhone(tc.in); got != tc.want {
			t.Errorf("maskPhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A code submitted with no running flow must be rejected: otherwise a stale tab
// could feed a code into a login that already finished.
func TestSubmitLoginCodeWithoutFlowIsRejected(t *testing.T) {
	c := &Controller{}
	err := c.SubmitLoginCode("12345")
	if err == nil {
		t.Fatal("expected an error when no flow is running")
	}
	if !strings.Contains(err.Error(), "没有等待验证码") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A QR flow never waits for a code; pushing one there would block or misfire.
func TestSubmitLoginCodeRejectsQRFlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Controller{
		ctx:       ctx,
		qrCancel:  cancel,
		codeCh:    make(chan string, 1),
		loginKind: loginKindQR,
	}
	if err := c.SubmitLoginCode("12345"); err == nil {
		t.Fatal("expected rejection while a QR flow owns the client")
	}
}

func TestSubmitLoginCodeRejectsEmpty(t *testing.T) {
	c := &Controller{}
	if err := c.SubmitLoginCode("   "); err == nil {
		t.Fatal("expected rejection of a blank code")
	}
}

func TestSubmitLoginCodeDelivers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Controller{
		ctx:       ctx,
		qrCancel:  cancel,
		codeCh:    make(chan string, 1),
		codeSent:  true,
		loginKind: loginKindPhone,
	}
	if err := c.SubmitLoginCode("  12345 "); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case got := <-c.codeCh:
		if got != "12345" {
			t.Fatalf("code = %q, want %q", got, "12345")
		}
	default:
		t.Fatal("code was not delivered to the waiting flow")
	}
}

// The channel is buffered with one slot; a double submit must not block the HTTP
// handler or silently queue a second code.
func TestSubmitLoginCodeRejectsDuplicate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Controller{
		ctx:       ctx,
		qrCancel:  cancel,
		codeCh:    make(chan string, 1),
		codeSent:  true,
		loginKind: loginKindPhone,
	}
	if err := c.SubmitLoginCode("11111"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := c.SubmitLoginCode("22222"); err == nil {
		t.Fatal("expected the second submit to be rejected")
	}
}

func TestQRStateReportsCodeSent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := &Controller{ctx: ctx, qrCancel: cancel, codeSent: true, loginKind: loginKindPhone}
	if got := c.QRState(); got != QRCodeSent {
		t.Fatalf("QRState() = %q, want %q", got, QRCodeSent)
	}

	// 2FA outranks the code step: the UI must switch to the password field.
	c.passwordMode = true
	if got := c.QRState(); got != QRPasswordNeeded {
		t.Fatalf("QRState() = %q, want %q", got, QRPasswordNeeded)
	}

	// Nothing running reports idle even if a stale flag is left behind.
	c.qrCancel = nil
	if got := c.QRState(); got != QRIdle {
		t.Fatalf("QRState() = %q, want %q", got, QRIdle)
	}
}

func TestLoginKindAndMaskedPhone(t *testing.T) {
	c := &Controller{loginKind: loginKindPhone, loginPhone: "+8613800138000"}
	if got := c.LoginKind(); got != loginKindPhone {
		t.Fatalf("LoginKind() = %q", got)
	}
	if got := c.LoginPhone(); got != "+861********00" {
		t.Fatalf("LoginPhone() = %q, want the masked form", got)
	}
}

func TestImportSessionRejectsBadInput(t *testing.T) {
	c := &Controller{}
	cases := []struct {
		name, sess, want string
	}{
		{"empty", "   ", "不能为空"},
		{"garbage", "not-a-session", "解析失败"},
		{"wrong version", "2AAAA", "解析失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.ImportSession(tc.sess)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestStartPhoneLoginRejectsInvalidPhone(t *testing.T) {
	c := &Controller{AppID: 1, AppHash: "hash"}
	if err := c.StartPhoneLogin("abc"); err == nil {
		t.Fatal("expected rejection of a phone number with no digits")
	}
	if err := c.StartPhoneLogin("+123"); err == nil {
		t.Fatal("expected rejection of a too-short phone number")
	}
}

// Without api_id/api_hash there is nothing to authenticate with; the error must
// point at configuration rather than silently starting a doomed flow.
func TestStartPhoneLoginRequiresCreds(t *testing.T) {
	c := &Controller{}
	err := c.StartPhoneLogin("+8613800138000")
	if err == nil {
		t.Fatal("expected a credentials error")
	}
	if !strings.Contains(err.Error(), "app-id") {
		t.Fatalf("error = %v, want it to mention app-id", err)
	}
}

// A flow abandoned mid-wait (modal closed, UI reset) used to block every later
// attempt with "another login is running" and never release qrCancel. These
// tests pin the two halves of the fix: the same-number case is reported as
// already-sent, and a cancelled/timed-out flow returns instead of hanging.
func TestPendingCodeFor(t *testing.T) {
	const num = "+8613800138000"
	c := &Controller{loginKind: loginKindPhone, loginPhone: num, codeSent: true}

	if !c.pendingCodeFor(num) {
		t.Fatal("same number with a code outstanding must be reported as pending")
	}
	if c.pendingCodeFor("+8613900139000") {
		t.Fatal("a different number must not be treated as the pending flow")
	}
	c.codeSent = false
	if c.pendingCodeFor(num) {
		t.Fatal("no code outstanding means the flow is not waiting for one")
	}
	c.codeSent = true
	c.loginKind = ""
	if c.pendingCodeFor(num) {
		t.Fatal("a non-phone flow must never be treated as a pending code")
	}
}

func TestAwaitCodeReturnsOnTimeout(t *testing.T) {
	c := &Controller{codeCh: make(chan string, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.awaitCodeAndSignIn(ctx, nil, "+8613800138000", "hash") }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitCodeAndSignIn blocked past its context deadline; qrCancel would never be released")
	}
}

// 登录流收尾必须只作用于"当前代"：被 stopQR 或新流取代的旧 goroutine
// 收尾时不得重置新流的字段（回归 close of closed channel 进程崩溃）。
func TestFinishLoginFlowOnlyResetsTheCurrentGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Controller{
		ctx:          ctx,
		qrCancel:     cancel,
		qrPwd:        make(chan string, 1),
		codeCh:       make(chan string, 1),
		codeSent:     true,
		passwordMode: true,
		loginKind:    loginKindPhone,
		loginPhone:   "15500000000",
		loginGen:     7,
	}

	// 旧代（6）收尾：什么都不得动
	c.finishLoginFlow(6)
	if c.qrCancel == nil || !c.codeSent || c.loginKind != loginKindPhone || c.loginPhone != "15500000000" || !c.passwordMode {
		t.Fatalf("旧代收尾重置了新流字段: %+v", c)
	}

	// 当前代（7）收尾：全部重置
	c.finishLoginFlow(7)
	if c.qrCancel != nil || c.codeSent || c.loginKind != "" || c.loginPhone != "" || c.passwordMode {
		t.Fatalf("当前代收尾未完成重置: %+v", c)
	}

	// 重复收尾同一代：no-op，不得 panic
	c.finishLoginFlow(7)
}

// stopQR 之后旧 goroutine 才收尾：代际已失效，共享字段必须保持 stopQR
// 留下的干净状态，且不得 panic。
func TestStopQRInvalidatesInFlightLoginCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Controller{
		ctx:       ctx,
		qrCancel:  cancel,
		loginKind: loginKindQR,
		loginGen:  3,
	}

	c.stopQR() // 代际自增 + 字段重置
	if c.loginGen != 4 {
		t.Fatalf("stopQR 应推进代际, got %d", c.loginGen)
	}

	// 旧流（gen=3）此时才收尾：no-op
	c.finishLoginFlow(3)
	if c.qrCancel != nil || c.loginKind != "" {
		t.Fatalf("旧流收尾污染了状态: %+v", c)
	}

	// 立刻启动新流（模拟取消后立即重试）：旧流收尾不得碰新流
	c.qrCancel = cancel
	c.loginKind = loginKindPhone
	c.codeSent = true
	c.loginGen = 5
	c.finishLoginFlow(3)
	if c.qrCancel == nil || !c.codeSent {
		t.Fatalf("旧流收尾清掉了新流: %+v", c)
	}
}
