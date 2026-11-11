package archive

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// noRedirect keeps 3xx responses observable; the default client would follow
// them and hide whether the server actually redirected.
func noRedirect() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func loginTestServer(t *testing.T, cfg WebAuthConfig) *httptest.Server {
	t.Helper()
	_, ts := newAuthTestServer(t, cfg)
	return ts
}

// readAll drains a response body for assertions.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoginRedirectsAnonymousBrowserToForm(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client := noRedirect()

	resp, err := client.Get(ts.URL + "/messages?page=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, loginPath+"?next=") {
		t.Fatalf("Location=%q, want %s?next=...", loc, loginPath)
	}
	// The original destination must survive so the operator lands where they
	// were headed after signing in.
	if !strings.Contains(loc, "%2Fmessages") {
		t.Fatalf("Location=%q lost the original path", loc)
	}
}

func TestLoginAPIUnauthorizedIsJSONSafe(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	resp, err := noRedirect().Get(ts.URL + "/api/messages/page?page=1&page_size=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
	// A fetch() must not receive an HTML redirect body.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("api 401 served HTML: %q", ct)
	}
}

func TestLoginPageRendersForm(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	resp, err := noRedirect().Get(ts.URL + loginPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body := readAll(t, resp)
	for _, want := range []string{`name="username"`, `name="password"`, `type="password"`, `method="post"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("login page missing %q", want)
		}
	}
	// The password must never be echoed back into the HTML.
	if strings.Contains(body, "hunter2hunter2") {
		t.Fatal("login page leaked the configured password")
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client := noRedirect()
	resp, err := client.PostForm(ts.URL+loginPath, url.Values{
		"username": {"admin"}, "password": {"wrong-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200 (re-rendered form)", resp.StatusCode)
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatal("wrong password must not issue a session cookie")
	}
	if body := readAll(t, resp); !strings.Contains(body, "账号或密码不正确") {
		t.Fatalf("missing error message, body=%s", body)
	}
}

func TestLoginAcceptsCorrectPasswordAndSetsCookie(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client := noRedirect()
	resp, err := client.PostForm(ts.URL+loginPath, url.Values{
		"username": {"admin"}, "password": {"hunter2hunter2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie issued")
	}
	c := cookies[0]
	if c.Name != sessionCookie {
		t.Fatalf("cookie name=%q", c.Name)
	}
	if !c.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite=%v want Lax", c.SameSite)
	}
	if c.Value == "" || !strings.HasPrefix(c.Value, "v1|") {
		t.Fatalf("cookie value not a signed session: %q", c.Value)
	}

	// The cookie must actually unlock a protected page.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/messages", nil)
	req.AddCookie(c)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status=%d", resp2.StatusCode)
	}
}

func TestSessionCookieTamperingRejected(t *testing.T) {
	w := NewWebServer(nil, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	now := time.Now()
	valid, _ := w.issueSession(now)

	if !w.validSession(valid, now) {
		t.Fatal("freshly issued session rejected")
	}

	cases := map[string]string{
		"flipped mac":     "v1|" + strings.Split(valid, "|")[1] + "|00000000000000000000000000000000000000000000000000000000000000ff",
		"extended expiry": "v1|99999999999|" + strings.Split(valid, "|")[2],
		"wrong version":   "v0" + strings.TrimPrefix(valid, "v1"),
		"empty":           "",
		"garbage":         "not-a-session",
		"no mac":          "v1|" + strings.Split(valid, "|")[1],
	}
	for name, value := range cases {
		if w.validSession(value, now) {
			t.Fatalf("%s: forged cookie accepted", name)
		}
	}

	// A correctly signed but expired token must also fail.
	past := now.Add(-2 * sessionTTLDefault)
	expired, _ := w.issueSession(past)
	if w.validSession(expired, now) {
		t.Fatal("expired session accepted")
	}
}

func TestSessionInvalidatedWhenPasswordChanges(t *testing.T) {
	now := time.Now()
	old := NewWebServer(nil, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	cookie, _ := old.issueSession(now)

	rotated := NewWebServer(nil, WebAuthConfig{LoginUser: "admin", LoginPassword: "different-password"})
	if rotated.validSession(cookie, now) {
		t.Fatal("session survived a password rotation; the signing key is not derived from the credentials")
	}
}

func TestLogoutClearsSessionCookie(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client := noRedirect()
	resp, err := client.PostForm(ts.URL+logoutPath, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != loginPath {
		t.Fatalf("Location=%q want %s", loc, loginPath)
	}
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not expire the session cookie")
	}
}

func TestLogoutRejectsGET(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	resp, err := noRedirect().Get(ts.URL + logoutPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			t.Fatal("a GET logout cleared the session cookie")
		}
	}
}

func TestLoginNextCannotRedirectOffsite(t *testing.T) {
	cases := map[string]string{
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"https://evil.example": "/",
		"http://evil.example":  "/",
		"":                     "/",
		loginPath:              "/",
		logoutPath:             "/",
		"/messages":            "/messages",
		"/media/videos?p=2":    "/media/videos?p=2",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Fatalf("safeNext(%q)=%q want %q", in, got, want)
		}
	}
}

func TestLoginThrottlesRepeatedFailures(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client := noRedirect()
	form := url.Values{"username": {"admin"}, "password": {"nope-nope-nope"}}

	var sawThrottle bool
	for i := 0; i < 12; i++ {
		resp, err := client.PostForm(ts.URL+loginPath, form)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		resp.Body.Close()
		if strings.Contains(body, "尝试次数过多") {
			sawThrottle = true
			break
		}
	}
	if !sawThrottle {
		t.Fatal("12 consecutive wrong passwords never triggered a lockout")
	}

	// A correct password during the lockout window must still be refused:
	// otherwise the throttle only slows attackers who never guess right.
	resp, err := client.PostForm(ts.URL+loginPath, url.Values{
		"username": {"admin"}, "password": {"hunter2hunter2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("lockout was bypassed by a correct password mid-window")
	}
}

func TestLoginBearerStillWorksAlongsideForm(t *testing.T) {
	// Token mode and login mode are mutually exclusive by config validation,
	// but a token must never be silently downgraded to the form redirect.
	w := NewWebServer(nil, WebAuthConfig{Token: "secret-token"})
	h := w.Handler()
	req := httptest.NewRequest(http.MethodGet, "/messages", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("bearer auth status=%d", rw.Code)
	}
}

// 登录页要加载 /static/theme.css 取共用 token。静态资源若被 auth 一起锁死，
// 浏览器对 CSS 请求拿到的是 303，样式表被静默丢弃 —— 未登录用户看到的是一份
// 没有任何样式的裸 HTML（实测就是这么坏的：card 圆角 0px、按钮透明）。
func TestAuthAllowsStaticAssetsForLoginPage(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client := noRedirect()

	for _, p := range []string{"/static/theme.css", "/static/app.css", "/manifest.webmanifest", "/sw.js"} {
		resp, err := client.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		body := readAll(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d want 200：静态资源不放行，登录页就没有样式", p, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Fatalf("%s 返回空体", p)
		}
		// Service Worker 注册对 MIME 极敏感：303 跟随到登录页后是 text/html，
		// 浏览器直接拒绝注册，PWA 的离线壳永远不生效。装得上、跑不起来。
		if p == "/sw.js" {
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
				t.Fatalf("/sw.js Content-Type=%q：SW 必须由 JS MIME 提供，否则注册被拒", ct)
			}
		}
	}

	// 放行必须只限静态层：页面与接口仍然要求凭证。
	for p, want := range map[string]int{
		"/":          http.StatusSeeOther,
		"/api/stats": http.StatusUnauthorized,
	} {
		resp, err := client.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s status=%d want %d：放行范围溢出到受保护资源", p, resp.StatusCode, want)
		}
	}
}

// 登录页 CSP 基线是 default-src 'none'。引同源样式表必须显式写 style-src 'self'，
// 否则浏览器拦下 /static/theme.css —— 这一条只有真跑页面才看得出来。
func TestLoginPageCSPAllowsSelfStylesheet(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	resp, err := noRedirect().Get(ts.URL + loginPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "style-src 'self'") {
		t.Fatalf("CSP 未允许同源样式表，登录页拿不到 theme.css：%q", csp)
	}
	if !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP 基线被放宽了，登录页应保持 default-src 'none'：%q", csp)
	}
	if !strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("script-src 被意外改动：%q", csp)
	}
}

// token 只能有一份真相：登录页引用 theme.css，不再自带 :root 色板。
func TestLoginPageUsesSharedTokens(t *testing.T) {
	raw, err := loginPageAsset.ReadFile("web/login.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	if !strings.Contains(page, `href="/static/theme.css"`) {
		t.Fatal("登录页没有引用 /static/theme.css：会退回各自一套色板")
	}
	for _, tok := range []string{"--bg-app:", "--bg-surface:", "--fg-primary:", "--state-danger:"} {
		if strings.Contains(page, tok) {
			t.Fatalf("登录页仍在自定义 %s，token 出现第二套真相", tok)
		}
	}
}

// TestMediaSubresourceUnauthorizedIs401 pins the fix for a misleading failure:
// a redirected <video> follows the 303, receives the login page as bytes and
// reports "no supported video format". An expired session then looks like a
// corrupt file instead of an expired session.
func TestMediaSubresourceUnauthorizedIs401(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client := noRedirect()
	sha := strings.Repeat("a", 64)

	for _, path := range []string{
		"/media/" + sha,
		"/media/thumb/" + sha,
		"/media/preview/" + sha,
		"/avatar/user/123",
		"/avatar/chat/456",
	} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body := readAll(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status=%d want 401", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Fatalf("%s: still redirects to %q", path, loc)
		}
		if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
			t.Fatalf("%s: 401 served HTML body %q", path, ct)
		}
		if strings.Contains(body, "<form") || strings.Contains(body, "<html") {
			t.Fatalf("%s: 401 body carried the login page", path)
		}
	}
}

// TestMediaPageRoutesStillRedirect guards the other half of that fix: the three
// /media/* navigation routes must keep landing on the login form.
func TestMediaPageRoutesStillRedirect(t *testing.T) {
	ts := loginTestServer(t, WebAuthConfig{LoginUser: "admin", LoginPassword: "hunter2hunter2"})
	client := noRedirect()

	for _, path := range []string{"/media/images", "/media/videos", "/media/files"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: status=%d want 303", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, loginPath+"?next=") {
			t.Fatalf("%s: Location=%q", path, loc)
		}
	}
}
