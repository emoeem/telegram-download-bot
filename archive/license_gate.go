package archive

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iyear/tdl/license"
)

// licencePageAsset is the activation form.
//
//go:embed web/activate.html
var licencePageAsset embed.FS

// licencePageHTML is the form markup, read once at startup so the gate never
// touches the filesystem on the request path.
var licencePageHTML = func() string {
	b, err := licencePageAsset.ReadFile("web/activate.html")
	if err != nil {
		return "<!doctype html><meta charset=utf-8><p>激活页缺失，请重新安装。</p>"
	}
	return string(b)
}()

const (
	// activatePath is the activation form: GET renders it, POST submits it.
	activatePath = "/activate"
	// licenceStatusPath reports activation state as JSON.
	licenceStatusPath = "/api/license/status"
	// licenceActivatePath activates from JSON, for clients that are not the
	// browser form (desktop shell, container health tooling).
	licenceActivatePath = "/api/license/activate"
	// maxLicenceBody caps the activation body: a key is tens of bytes and the
	// form adds little, so 16 KiB is generous without allowing a stream.
	maxLicenceBody = 16 << 10
)

// now is the clock the licence gate uses. Tests override it via Now.
func (w *WebServer) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// licenceActive reports whether this deployment enforces licensing at all.
// A nil or unconfigured manager means a source build, which stays ungated.
func (w *WebServer) licenceActive() bool {
	return w.Licence != nil && w.Licence.Configured()
}

// licenceServerDefault is the online activation service compiled into this
// build. Empty means offline-only activation.
//
//	-X github.com/iyear/tdl/archive.licenceServerDefault=https://lic.example.com
//
// A release that ships to phones sets it so the customer never has to type a
// server address; the desktop and container builds leave it empty and use
// --license-server instead.
var licenceServerDefault = ""

// licenceServerAddr resolves the effective online activation address:
// explicit configuration wins, the compiled-in default is the fallback.
func (w *WebServer) licenceServerAddr() string {
	if s := strings.TrimSpace(w.LicenceServer); s != "" {
		return s
	}
	return strings.TrimSpace(licenceServerDefault)
}

// licencePost calls one endpoint of the licence service.
//
// It returns the server's machine-readable code on a successful HTTP exchange,
// and an error only when the call itself failed (network, DNS, non-JSON). That
// split matters: a definitive denial must clear the local activation, while a
// network failure must not — a customer on a plane keeps the licence they paid
// for.
func licencePost(ctx context.Context, server, path, key, device string) (string, error) {
	body, _ := json.Marshal(map[string]string{"key": key, "device": device})

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(server, "/")+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("连不上授权服务（%s），请检查网络后重试", server)
	}
	defer resp.Body.Close()

	var out struct {
		OK      bool   `json:"ok"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("授权服务返回异常（HTTP %d）", resp.StatusCode)
	}
	if out.OK {
		return "ok", nil
	}
	msg := out.Message
	if msg == "" {
		msg = out.Code
	}
	return out.Code, errors.New(msg)
}

// denied reports whether a server code means "this key is not usable here",
// as opposed to "the call failed".
func denied(code string) bool {
	switch code {
	case "revoked", "expired", "unknown_key", "bound_elsewhere":
		return true
	}
	return false
}

// licenceRegisterOnline claims this device for key on the licence server.
func licenceRegisterOnline(ctx context.Context, server, key, device string) error {
	_, err := licencePost(ctx, server, "/v1/activate", key, device)
	return err
}

// licenceCheckInterval is how long a successful online check stays valid.
func (w *WebServer) licenceCheckInterval() time.Duration {
	if w.LicenceCheckInterval > 0 {
		return w.LicenceCheckInterval
	}
	return 12 * time.Hour
}

// rememberOnline records where this activation came from, so later starts know
// where to re-check without the customer retyping an address.
func (w *WebServer) rememberOnline(ctx context.Context, key string, now time.Time) {
	srv := w.licenceServerAddr()
	if srv == "" {
		return
	}
	_ = w.Licence.SetServer(srv)
	_ = w.Licence.MarkChecked(now)
}

// maybeRefreshLicence phones home when the last successful check is stale.
//
// This is where remote revocation actually lands: the server says "revoked",
// the local activation is dropped, and the next request gets the activation
// form again.
func (w *WebServer) maybeRefreshLicence(now time.Time) {
	if w.Licence == nil {
		return
	}
	srv := w.licenceServerAddr()
	if srv == "" {
		return
	}
	if !w.Licence.NeedsOnlineCheck(now, w.licenceCheckInterval()) {
		return
	}
	// 单飞：到期边界上的并发请求只放一个去核对，其余直接走陈旧但有效的
	// 本地激活——否则一次突发就是 N 个并发的上游 verify + N 个请求各阻塞
	// 到超时。核对失败的调用方照旧拿到本地授权（网络错误不清授权）。
	if !w.licenceCheck.CompareAndSwap(false, true) {
		return
	}
	defer w.licenceCheck.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := licencePost(ctx, srv, "/v1/verify",
		w.Licence.CurrentKey(), w.Licence.ThisDevice())
	if err == nil {
		_ = w.Licence.MarkChecked(now)
		return
	}
	if denied(code) {
		// 服务端明确否定：本地授权必须同步失效。
		_ = w.Licence.Clear()
		return
	}
	// 网络问题：保留本地授权，等下一次心跳。
}

// licenceGate enforces the activation before anything else in the chain.
//
// It sits outside the login middleware on purpose: activation is what makes the
// installation legitimate at all, and the login form is for protecting the
// archived data. Asking for a password on an unlicensed install would be
// security theatre in the wrong order.
func (w *WebServer) licenceGate(next http.Handler) http.Handler {
	if !w.licenceActive() {
		return next
	}
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// The activation page and its API must stay reachable while the gate is
		// closed, and so must the assets it loads: a bare unstyled form is what
		// you get when a stylesheet redirects to HTML.
		if r.URL.Path == activatePath ||
			r.URL.Path == licenceStatusPath ||
			r.URL.Path == licenceActivatePath ||
			strings.HasPrefix(r.URL.Path, "/static/") ||
			r.URL.Path == "/manifest.webmanifest" ||
			r.URL.Path == "/sw.js" {
			switch r.URL.Path {
			case activatePath:
				w.activatePage(rw, r)
				return
			case licenceStatusPath:
				w.licenceStatusAPI(rw, r)
				return
			case licenceActivatePath:
				w.licenceActivateAPI(rw, r)
				return
			}
			next.ServeHTTP(rw, r)
			return
		}

		// Health probes answer without a licence: they are what a supervisor
		// uses to decide whether the process is alive, and they expose no
		// archive content.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(rw, r)
			return
		}

		// 到期就向服务端核对一次：撤销在这里真正落地。
		w.maybeRefreshLicence(w.now())

		err := w.Licence.Gate(w.now())
		if err == nil {
			next.ServeHTTP(rw, r)
			return
		}
		if isLicenceAPIPath(r.URL.Path) {
			rw.Header().Set("Content-Type", "application/json; charset=utf-8")
			rw.Header().Set("Cache-Control", "no-store")
			rw.WriteHeader(http.StatusPaymentRequired)
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"ok":     false,
				"error":  err.Error(),
				"status": w.Licence.Status(w.now()),
			})
			return
		}
		// A navigation goes to the activation form, carrying where the visitor
		// was headed so activation lands them there instead of at the root.
		target := activatePath
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if q := r.URL.RequestURI(); q != "" && !strings.HasPrefix(q, activatePath) {
				target += "?next=" + urlQueryEscape(q)
			}
		}
		http.Redirect(rw, r, target, http.StatusSeeOther)
	})
}

// isLicenceAPIPath reports whether the caller expects JSON.
func isLicenceAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/media/") ||
		strings.HasPrefix(p, "/avatar/")
}

// urlQueryEscape is url.QueryEscape, kept as a helper so the import list in
// this file stays minimal and the intent is obvious at the call site.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// activatePage serves GET and handles POST /activate.
func (w *WebServer) activatePage(rw http.ResponseWriter, r *http.Request) {
	if !w.licenceActive() {
		// Activation disabled after the visitor bookmarked the page.
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	now := w.now()
	st := w.Licence.Status(now)

	// Already valid: there is nothing to activate. Send them on to whatever the
	// login middleware wants to ask next.
	if st.Valid {
		http.Redirect(rw, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.renderActivate(rw, r, "", "", http.StatusOK)
		return
	case http.MethodPost:
		// 表单端点在 csrfGuard 之前，必须自带同源校验：跨站表单 POST 可以把
		// 攻击者的密钥绑到受害者设备上（login-CSRF 级别；JSON 双胞胎已被
		// csrfGuard 覆盖，这里补齐表单这一侧）。
		//
		// 只看头是不够的：Android WebView 提交回环文档时会发 Origin: null，
		// 反向代理部署会把 Host 改写成上游地址 —— 两种真实客户端都会因此
		// 拿到 403。表单里带一个本进程签发的一次性 token 作为等价凭据，
		// 跨站页面既读不到它（X-Frame-Options: DENY + frame-ancestors 'none'
		// 挡住了 iframe），也无法预先知道它。
		r.Body = http.MaxBytesReader(rw, r.Body, maxLicenceBody)
		if err := r.ParseForm(); err != nil {
			w.renderActivate(rw, r, "", "请求格式不正确，请重试。", http.StatusOK)
			return
		}
		if !sameOrigin(r) && !w.activateTokenOK(r) {
			slog.Warn("activate form rejected",
				"origin", r.Header.Get("Origin"),
				"host", r.Host,
				"fetch_site", r.Header.Get("Sec-Fetch-Site"))
			http.Error(rw, "cross-origin form post rejected", http.StatusForbidden)
			return
		}
	default:
		rw.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := strings.TrimSpace(r.PostFormValue("key"))
	if key == "" {
		w.renderActivate(rw, r, "", "请输入授权密钥。", http.StatusOK)
		return
	}
	// 联网登记先于本地验签。表单入口与 JSON API 入口都必须过这一关，
	// 否则绕开 API 直接提交表单就能跳过服务端绑定。
	if srv := w.licenceServerAddr(); srv != "" {
		if err := licenceRegisterOnline(r.Context(), srv, key, w.Licence.ThisDevice()); err != nil {
			w.renderActivate(rw, r, key, err.Error(), http.StatusOK)
			return
		}
	}
	if _, err := w.Licence.Activate(key, now); err != nil {
		w.renderActivate(rw, r, key, licenceErrorMessage(err), http.StatusOK)
		return
	}
	w.rememberOnline(r.Context(), key, now)
	http.Redirect(rw, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

// licenceErrorMessage turns a verifier error into something a customer can act
// on. The distinction between "typo" and "wrong machine" is the whole point of
// keeping those errors separate in the license package.
func licenceErrorMessage(err error) string {
	switch {
	case errors.Is(err, license.ErrMalformed):
		return "密钥格式不正确，请检查是否漏字符或混入了其它内容。"
	case errors.Is(err, license.ErrSignature):
		return "这个密钥无效。请确认复制完整，或联系提供软件的人重新签发。"
	case errors.Is(err, license.ErrExpired):
		return "这个密钥已经过期，需要续期。"
	case errors.Is(err, license.ErrDevice):
		return "这个密钥是给另一台设备签发的，不能在本机使用。请把本页的设备码发给提供软件的人。"
	case errors.Is(err, license.ErrClock):
		return "系统时间比上次运行时更早，请先把时间校准。"
	case errors.Is(err, license.ErrNotConfigured):
		return "本版本没有内置授权校验，请联系提供软件的人。"
	default:
		return "激活失败：" + err.Error()
	}
}

// activateToken returns the token embedded in the activation form. It is
// generated once per process: a submission is only accepted by the instance
// that rendered the page, which is exactly the scope a restart invalidates.
func (w *WebServer) activateToken() string {
	w.csrfOnce.Do(func() {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			// Leave it empty so the handler falls back to the header check
			// instead of accepting every submission with an empty token.
			return
		}
		w.csrfToken = hex.EncodeToString(b[:])
	})
	return w.csrfToken
}

// activateTokenOK compares the hidden field against this process's token.
func (w *WebServer) activateTokenOK(r *http.Request) bool {
	want := w.activateToken()
	if want == "" {
		return false
	}
	got := r.PostFormValue("csrf")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// renderActivate draws the form.
func (w *WebServer) renderActivate(rw http.ResponseWriter, r *http.Request, key, message string, status int) {
	errBlock := ""
	if message != "" {
		errBlock = `<div class="err">` + html.EscapeString(message) + `</div>`
	}
	// The hint explains what to do when a licence already exists but is no
	// longer valid — expired, or bound elsewhere.
	hint := "密钥由软件提供方签发，一机一码，不能转给其他设备使用。"
	if st := w.Licence.Status(w.now()); st.Activated && !st.Valid {
		switch {
		case st.Reason == license.ErrExpired.Error():
			hint = "上一枚密钥已于 " + html.EscapeString(st.ExpiresAt) + " 到期，请向提供软件的人申请续期。"
		default:
			hint = "上一枚密钥当前不可用：" + html.EscapeString(st.Reason)
		}
	}
	theme := `<button type="button" class="theme" id="theme">切换主题</button>`

	page := licencePageHTML
	page = strings.ReplaceAll(page, "{{ERROR}}", errBlock)
	page = strings.ReplaceAll(page, "{{DEVICE}}", html.EscapeString(w.Licence.ThisDevice()))
	page = strings.ReplaceAll(page, "{{KEY}}", html.EscapeString(key))
	page = strings.ReplaceAll(page, "{{HINT}}", hint)
	page = strings.ReplaceAll(page, "{{THEME}}", theme)
	// next must survive the POST that follows, so it rides along as a hidden
	// field rather than relying on the query string surviving the redirect.
	page = strings.ReplaceAll(page, "{{NEXT}}", html.EscapeString(safeNext(r.URL.Query().Get("next"))))
	page = strings.ReplaceAll(page, "{{CSRF}}", html.EscapeString(w.activateToken()))

	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	rw.WriteHeader(status)
	_, _ = io.WriteString(rw, page)
}

// licenceStatusAPI reports the current activation state.
func (w *WebServer) licenceStatusAPI(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	writeJSON(rw, w.Licence.Status(w.now()))
}

// licenceActivateAPI accepts {"key": "..."} and reports the outcome.
func (w *WebServer) licenceActivateAPI(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", "POST")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, maxLicenceBody)
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(rw).Encode(map[string]any{"ok": false, "error": "请求格式不正确"})
		return
	}
	key := strings.TrimSpace(body.Key)

	// 联网登记优先于本地验签。本地验签只能证明"密钥是发行方签的"，
	// 证明不了"同一把密钥没被复制到第二台设备"——那是服务端绑定表的职责。
	if srv := w.licenceServerAddr(); srv != "" {
		if err := licenceRegisterOnline(r.Context(), srv, key, w.Licence.ThisDevice()); err != nil {
			rw.Header().Set("Content-Type", "application/json; charset=utf-8")
			rw.Header().Set("Cache-Control", "no-store")
			rw.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"ok":     false,
				"error":  err.Error(),
				"status": w.Licence.Status(w.now()),
			})
			return
		}
	}

	st, err := w.Licence.Activate(key, w.now())
	if err == nil {
		w.rememberOnline(r.Context(), key, w.now())
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	if err != nil {
		rw.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"ok":     false,
			"error":  licenceErrorMessage(err),
			"status": st,
		})
		return
	}
	_ = json.NewEncoder(rw).Encode(map[string]any{"ok": true, "status": st})
}
