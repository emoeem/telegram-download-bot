package archive

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"html"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/login.html
var loginPageAsset embed.FS

var loginPageHTML = func() string {
	b, _ := loginPageAsset.ReadFile("web/login.html")
	return string(b)
}()

const (
	loginPath     = "/login"
	logoutPath    = "/logout"
	sessionCookie = "tdl_archive_session"
	// sessionTTLDefault is deliberately short: the archive exposes an entire
	// message history, so a stolen cookie should age out within a work day.
	sessionTTLDefault = 12 * time.Hour
	// maxLoginBody caps the form body so a forged Content-Length cannot make
	// the server buffer megabytes before rejecting it.
	maxLoginBody = 8 << 10
)

// LoginEnabled reports whether the configuration describes a form-login setup.
// Runtime decisions go through WebServer.loginEnabled, which also sees a
// password that was changed after startup.
func (c WebAuthConfig) LoginEnabled() bool {
	return strings.TrimSpace(c.LoginUser) != "" && (c.PasswordHash != "" || c.LoginPassword != "")
}

// authConfigured reports whether any credential source is set. When it is
// false the archive is wide open and serve refuses to bind off-loopback.
func (c WebAuthConfig) authConfigured() bool {
	return c.Token != "" || c.BasicUser != "" || c.LoginEnabled()
}

// loginCredential is the live login identity. It is swapped atomically by
// /api/auth/account so a password change takes effect on the next request
// without a restart and without a lock on the hot path.
type loginCredential struct {
	user string
	cred Credential
	// file is where a change is persisted. Empty means the credential came
	// from flags only and cannot be changed at runtime.
	file string
}

// newLoginCredential builds the initial state from the startup config.
// PasswordHash wins over LoginPassword: once the credential file has been
// hashed, the startup flag is stale by definition.
func newLoginCredential(cfg WebAuthConfig) *loginCredential {
	lc := &loginCredential{user: cfg.LoginUser, file: cfg.CredentialFile}
	switch {
	case cfg.PasswordHash != "":
		if c, err := DecodeCredential(cfg.PasswordHash); err == nil {
			lc.cred = c
			return lc
		}
		// A malformed override must not silently fall back to "no password".
		lc.cred = Credential{}
	case cfg.LoginPassword != "":
		lc.cred = Credential{Username: cfg.LoginUser, plaintext: cfg.LoginPassword, legacy: true}
	}
	return lc
}

// cred returns the current credential. A nil pointer means NewWebServer was
// bypassed (struct literal in a test); rebuilding it from Auth keeps that path
// working instead of panicking inside an auth check.
func (w *WebServer) cred() *loginCredential {
	if lc := w.login.Load(); lc != nil {
		return lc
	}
	lc := newLoginCredential(w.Auth)
	w.login.Store(lc)
	return lc
}

// loginEnabled reports whether the form-login flow is active right now.
func (w *WebServer) loginEnabled() bool {
	lc := w.cred()
	return strings.TrimSpace(lc.user) != "" && lc.cred.HasPassword()
}

// sessionSecret derives the cookie signing key from the credentials.
// Deriving (instead of storing a second secret) means rotating the password
// instantly invalidates every session that was issued under the old one.
func (w *WebServer) sessionSecret() []byte {
	if len(w.Auth.SessionSecret) > 0 {
		return w.Auth.SessionSecret
	}
	lc := w.cred()
	h := sha256.New()
	h.Write([]byte("tdl-archive/session-key/v1"))
	h.Write([]byte(lc.user))
	h.Write([]byte{0})
	h.Write([]byte(lc.cred.Verifier()))
	return h.Sum(nil)
}

func (w *WebServer) sessionTTL() time.Duration {
	if w.Auth.SessionTTL > 0 {
		return w.Auth.SessionTTL
	}
	return sessionTTLDefault
}

// constantTimeEqual hashes both sides before comparing so the comparison cost
// cannot leak the credential length.
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// issueSession returns a signed cookie value of the form v1|<expiry>|<mac>.
// Only the expiry is inside the MAC; the username is implicit because the
// server only ever serves a single configured operator account.
func (w *WebServer) issueSession(now time.Time) (string, time.Time) {
	exp := now.Add(w.sessionTTL())
	expStr := strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, w.sessionSecret())
	mac.Write([]byte("v1|" + expStr))
	return "v1|" + expStr + "|" + hex.EncodeToString(mac.Sum(nil)), exp
}

// validSession checks the signature first and the expiry second; a forged
// value never reaches the clock comparison.
func (w *WebServer) validSession(value string, now time.Time) bool {
	parts := strings.Split(value, "|")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, w.sessionSecret())
	mac.Write([]byte("v1|" + parts[1]))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return false
	}
	return now.Unix() < exp
}

// loginLimiter throttles password guessing per client address. A pure in-memory
// map is the right trade here: the archive is a single-process service, and
// persisting lockouts would let an attacker keep an operator locked out across
// restarts.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*loginFailure
}

type loginFailure struct {
	count int
	until time.Time
	// last 是最近一次失败的时间：count<3 时 until==now（wait=0），
	// 回收判定必须看它而不是 until，否则连续失败会被自己的清扫清零。
	last time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{fails: make(map[string]*loginFailure)}
}

// ensure makes the zero value usable so a WebServer built by hand (tests,
// embedders) never panics on a nil map.
func (l *loginLimiter) ensure() {
	if l.fails == nil {
		l.fails = make(map[string]*loginFailure)
	}
}

// blocked reports the remaining lockout for a key.
func (l *loginLimiter) blocked(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensure()
	f, ok := l.fails[key]
	if !ok || f.count == 0 {
		return 0, false
	}
	if now.Before(f.until) {
		return f.until.Sub(now), true
	}
	return 0, false
}

// fail records a wrong password and returns the resulting lockout window.
// The delay doubles per consecutive failure and is capped at 15 minutes.
func (l *loginLimiter) fail(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ensure()
	// 顺带回收陈旧项：条目只在成功登录时被 reset 删除，攻击者轮换源地址
	// 会让 map 无限增长。判定看 last（最近失败时间）而非 until——count<3
	// 时 until==now，按 until 收会把正在累加的计数器清掉，锁死永远建立
	// 不起来。1 小时空闲远超 15 分钟封顶窗口，回收是安全的。
	for k, v := range l.fails {
		if now.Sub(v.last) > time.Hour {
			delete(l.fails, k)
		}
	}
	f, ok := l.fails[key]
	if !ok {
		f = &loginFailure{}
		l.fails[key] = f
	}
	f.count++
	var wait time.Duration
	if f.count >= 3 {
		shift := f.count - 3
		if shift > 6 {
			shift = 6
		}
		wait = time.Minute << uint(shift)
		if wait > 15*time.Minute {
			wait = 15 * time.Minute
		}
	}
	f.last = now
	f.until = now.Add(wait)
	return wait
}

// reset clears the counter after a successful login.
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// clientKey buckets lockouts by address only. Behind a reverse proxy every
// request shares the proxy address, which is acceptable: the operator is the
// only legitimate user, so a shared bucket is a safety net, not a bottleneck.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// safeNext only ever returns a same-origin absolute path. Without this a
// crafted ?next=//evil.example turns the login page into an open redirect.
func safeNext(next string) string {
	if next == "" {
		return "/"
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	if strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	if next == loginPath || next == logoutPath {
		return "/"
	}
	return next
}

// renderLogin writes the login page. It is always a 200 so password errors do
// not get cached or surfaced differently by proxies.
func (w *WebServer) renderLogin(rw http.ResponseWriter, r *http.Request, username, message string, status int) {
	next := safeNext(r.URL.Query().Get("next"))
	errBlock := ""
	if message != "" {
		errBlock = `<div class="err">` + html.EscapeString(message) + `</div>`
	}
	page := loginPageHTML
	page = strings.ReplaceAll(page, "{{ERROR}}", errBlock)
	page = strings.ReplaceAll(page, "{{USER}}", html.EscapeString(username))
	page = strings.ReplaceAll(page, "{{NEXT}}", html.EscapeString(next))
	page = strings.ReplaceAll(page, "{{TTL}}", humanTTL(w.sessionTTL()))

	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	// style-src 必须带 'self'：登录页引用 /static/theme.css 取共用 token。
	// 只加 'self'，脚本仍是内联白名单、其余一律 default-src 'none' ——
	// 样式表不构成脚本执行面，而 'unsafe-inline' 本来就已经允许内联样式。
	rw.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	rw.WriteHeader(status)
	_, _ = rw.Write([]byte(page))
}

func humanTTL(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + " 小时"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + " 分钟"
	default:
		return d.String()
	}
}

// verifyLogin checks user/pass against the live credential.
//
// The password is always hashed, even when the username is already known to be
// wrong: returning early on a bad username would make "does this account
// exist" measurable by response time, and PBKDF2 is where nearly all the
// elapsed time comes from.
func (w *WebServer) verifyLogin(user, pass string) bool {
	lc := w.cred()
	passOK := lc.cred.Verify(pass) == nil
	userOK := constantTimeEqual(user, lc.user)
	return passOK && userOK
}

// loginPage serves GET /login and handles POST /login.
func (w *WebServer) loginPage(rw http.ResponseWriter, r *http.Request) {
	now := time.Now()
	// Already signed in: bounce to the destination instead of showing the form.
	if c, err := r.Cookie(sessionCookie); err == nil && w.validSession(c.Value, now) {
		http.Redirect(rw, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.renderLogin(rw, r, "", "", http.StatusOK)
		return
	case http.MethodPost:
	default:
		rw.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(rw, r.Body, maxLoginBody)
	if err := r.ParseForm(); err != nil {
		w.renderLogin(rw, r, "", "请求格式不正确，请重试。", http.StatusOK)
		return
	}
	user := strings.TrimSpace(r.PostFormValue("username"))
	pass := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	key := clientKey(r)
	if wait, blocked := w.loginLimiter.blocked(key, now); blocked {
		w.renderLogin(rw, r, user,
			"尝试次数过多，请等待 "+humanTTL(wait.Round(time.Second))+" 后再试。", http.StatusOK)
		return
	}
	if !w.verifyLogin(user, pass) {
		w.loginLimiter.fail(key, now)
		w.renderLogin(rw, r, user, "账号或密码不正确。", http.StatusOK)
		return
	}

	w.loginLimiter.reset(key)
	value, exp := w.issueSession(now)
	http.SetCookie(rw, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(w.sessionTTL().Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(rw, r, next, http.StatusSeeOther)
}

// logout always clears the cookie, even when the request carries none, so a
// stale browser state is never left behind.
func (w *WebServer) logout(rw http.ResponseWriter, r *http.Request) {
	// POST only: a GET logout can be triggered by any third-party page that
	// points an <img> or link at /logout, signing the user out.
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", "POST")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(rw, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(rw, r, loginPath, http.StatusSeeOther)
}

// loginRoutes installs the login/logout endpoints ahead of the auth middleware
// so they stay reachable while everything else is locked down.
func (w *WebServer) loginRoutes(next http.Handler) http.Handler {
	if !w.loginEnabled() {
		return next
	}
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginPath:
			w.loginPage(rw, r)
			return
		case logoutPath:
			w.logout(rw, r)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// denySession answers an unauthenticated request: browsers get pushed to the
// login page (preserving where they were headed), API clients and subresources
// get a clean 401 so a fetch() does not try to parse HTML.
func (w *WebServer) denySession(rw http.ResponseWriter, r *http.Request) {
	// /media/<sha256>, /media/thumb|preview/<sha256> and /avatar/* are
	// subresources, not pages. Redirecting them is worse than useless: the
	// element follows the 303, receives the login page as image/video bytes and
	// reports a codec error, so an expired session looks like a corrupt file.
	// The page routes /media/images, /media/videos and /media/files keep the
	// redirect -- they are navigations and must land on the login form.
	if strings.HasPrefix(r.URL.Path, "/api/") || isSubresource(r.URL.Path) {
		rw.Header().Set("WWW-Authenticate", `Form realm="tdl-archive"`)
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}
	target := loginPath
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if q := r.URL.RequestURI(); q != "" && !strings.HasPrefix(q, loginPath) {
			// QueryEscape is mandatory here: a raw "?" or "&" inside the value
			// would split the parameter and drop the page number.
			target += "?next=" + url.QueryEscape(q)
		}
	}
	http.Redirect(rw, r, target, http.StatusSeeOther)
}

// isSubresource reports whether the path addresses bytes embedded by a page
// (img/video/audio src) rather than a navigable document.
func isSubresource(path string) bool {
	if strings.HasPrefix(path, "/avatar/") {
		return true
	}
	if !strings.HasPrefix(path, "/media/") {
		return false
	}
	switch path {
	case "/media/images", "/media/videos", "/media/files":
		return false
	}
	return true
}
