package archive

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
)

//go:embed web/index.html web/static/theme.css
var webAssets embed.FS

var indexHTMLAsset = func() string {
	b, _ := webAssets.ReadFile("web/index.html")
	return string(b)
}()

type WebAuthConfig struct {
	Token         string
	BasicUser     string
	BasicPassword string
	// LoginUser/LoginPassword switch the archive to form-based session auth:
	// visitors land on a real login page instead of the browser's Basic dialog,
	// and get a signed, expiring cookie plus an explicit logout.
	LoginUser     string
	LoginPassword string
	// PasswordHash is the PBKDF2 verifier that supersedes LoginPassword once
	// the credential file exists. It lives here, not in a second store, so the
	// "which secret signs the cookie" question has exactly one answer:
	// LoginCredential.Verifier().
	PasswordHash string
	// CredentialFile is where /api/auth/account writes a changed username or
	// password. Empty disables the endpoint (the server is then configured
	// entirely from flags, which is what every test does).
	CredentialFile string
	// SessionSecret overrides the cookie signing key. When empty it is derived
	// from the credentials, so changing the password revokes every session.
	SessionSecret []byte
	// SessionTTL defaults to 12h.
	SessionTTL time.Duration
}

type WebServer struct {
	Store *store.Store
	Auth  WebAuthConfig
	Ctl   *Controller // optional; nil keeps the server a read-only viewer
	// Accounts, when set, makes Store and Ctl projections of the account that
	// is currently active. The plain fields stay as the fallback so a read-only
	// server — and every test that builds one directly — keeps working.
	Accounts *AccountManager
	// Root is the archive data root (objects/, by-serial/). Empty means
	// "<DataDir>/archive"; tests and alternate deployments set it explicitly so
	// a purge can never reach into the live archive by accident.
	Root         string
	httpMetrics  HTTPMetrics
	metricsCache metricsCache
	// loginLimiter throttles password guessing; the zero value is usable.
	loginLimiter loginLimiter
	// login holds the live credential. It is an atomic pointer rather than a
	// field on Auth because /api/auth/account mutates it while other requests
	// read it, and a torn read here would silently accept a wrong password.
	login atomic.Pointer[loginCredential]
}

// store resolves the database of the account being served. Resolving per call,
// rather than caching a pointer at startup, is what makes switching accounts
// take effect without a restart.
func (w *WebServer) store() *store.Store {
	if w.Accounts != nil {
		if s := w.Accounts.Current(); s != nil && s.Store != nil {
			return s.Store
		}
	}
	return w.Store
}

// ctl resolves the controller of the account being served, or nil when that
// account has no control plane (read-only mode, or no Telegram credentials).
func (w *WebServer) ctl() *Controller {
	if w.Accounts != nil {
		if s := w.Accounts.Current(); s != nil {
			return s.Ctl
		}
		return nil
	}
	return w.Ctl
}

type deletedAPIResponse struct {
	Items    []store.MessageView `json:"items"`
	Total    int                 `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

type editedAPIItem struct {
	Message   store.MessageView `json:"message"`
	EditCount int               `json:"edit_count"`
}

type editedAPIResponse struct {
	Items    []editedAPIItem `json:"items"`
	Total    int             `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

type mediaAPIResponse struct {
	Items    []mediaAPIItem  `json:"items"`
	Total    int             `json:"total"`
	PageSize int             `json:"page_size"`
	Page     int             `json:"page"`
	Kind     store.MediaKind `json:"kind"`
}

type mediaAPIItem struct {
	store.MediaItem
	MediaErrorKind   string `json:"media_error_kind,omitempty"`
	MediaErrorDetail string `json:"media_error_detail,omitempty"`
	MediaNextRetryAt *int64 `json:"media_next_retry_at,omitempty"`
}

type jobsAPIResponse struct {
	Items []Job `json:"items"`
}
type downloadsSummaryResponse = store.DownloadSummary

type linksAPIResponse struct {
	Items    []store.LinkItem `json:"items"`
	Total    int              `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

type messagesAPIResponse struct {
	Items    []store.MessageView `json:"items"`
	Total    int                 `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

type messageAPIResponse struct {
	Message store.MessageView `json:"message"`
}

type linkDomainsAPIResponse struct {
	Items []store.LinkDomainCount `json:"items"`
}

func NewWebServer(s *store.Store, auth ...WebAuthConfig) *WebServer {
	cfg := WebAuthConfig{}
	if len(auth) > 0 {
		cfg = auth[0]
	}
	w := &WebServer{Store: s, Auth: cfg}
	w.login.Store(newLoginCredential(cfg))
	return w
}

// NewManagedWebServer returns a viewer with the control plane enabled: QR
// login, monitor management and the supervised realtime archiver.
func NewManagedWebServer(s *store.Store, ctl *Controller, auth ...WebAuthConfig) *WebServer {
	w := NewWebServer(s, auth...)
	w.Ctl = ctl
	return w
}
func (w *WebServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/static/theme.css", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		b, err := webAssets.ReadFile("web/static/theme.css")
		if err != nil {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = rw.Write(b)
	})
	mux.HandleFunc("/healthz", w.healthz)
	mux.HandleFunc("/readyz", w.readyz)
	mux.HandleFunc("/", w.index)
	mux.HandleFunc("/deleted", w.index)
	mux.HandleFunc("/edits", w.index)
	mux.HandleFunc("/media/images", w.index)
	mux.HandleFunc("/media/videos", w.index)
	mux.HandleFunc("/media/files", w.index)
	mux.HandleFunc("/links", w.index)
	mux.HandleFunc("/messages", w.index)
	mux.HandleFunc("/storage", w.index)
	mux.HandleFunc("/inbox", w.index)
	mux.HandleFunc("/materials", w.index)
	mux.HandleFunc("/api/stats", w.stats)
	mux.HandleFunc("/api/metrics", w.metricsAPI)
	mux.HandleFunc("/api/deleted", w.deleted)
	mux.HandleFunc("/api/edited", w.edited)
	mux.HandleFunc("/api/media", w.mediaList)
	mux.HandleFunc("/api/links", w.linksList)
	mux.HandleFunc("/api/link_domains", w.linkDomains)
	mux.HandleFunc("/api/targets", w.targets)
	mux.HandleFunc("/api/monitor/sync", w.monitorSync)
	mux.HandleFunc("/api/search", w.search)
	mux.HandleFunc("/api/messages", w.messages)
	mux.HandleFunc("/api/messages/page", w.messagesPage)
	mux.HandleFunc("/api/topics", w.topics)
	mux.HandleFunc("/api/message/rowid", w.messageByRowID)
	mux.HandleFunc("/api/message/group", w.messageGroup)
	mux.HandleFunc("/api/message/delete", w.messageDelete)
	mux.HandleFunc("/api/message", w.message)
	mux.HandleFunc("/api/edits", w.edits)
	mux.HandleFunc("/media/", w.media)
	mux.HandleFunc("/avatar/", w.avatar)
	mux.HandleFunc("/api/destinations", w.destinations)
	mux.HandleFunc("/api/auth/status", w.authStatus)
	// The panel login credential. Reached only after authMiddleware, so an
	// anonymous POST cannot be used to probe the current password.
	mux.HandleFunc("/api/auth/account", w.accountAPI)
	mux.HandleFunc("/api/accounts", w.accountsAPI)
	mux.HandleFunc("/api/accounts/", w.accountRoute)
	mux.HandleFunc("/api/login/qr/start", w.loginQRStart)
	mux.HandleFunc("/api/login/qr/status", w.loginQRStatus)
	mux.HandleFunc("/api/login/qr/image", w.loginQRImage)
	mux.HandleFunc("/api/login/qr/cancel", w.loginQRCancel)
	mux.HandleFunc("/api/login/password", w.loginPassword)
	mux.HandleFunc("/api/jobs", w.jobs)
	mux.HandleFunc("/api/jobs/", w.jobRoute)
	mux.HandleFunc("/api/downloads/summary", w.downloadSummary)
	mux.HandleFunc("/api/downloads/active", w.downloadActive)
	mux.HandleFunc("/api/downloads/pause", w.downloadPause)
	mux.HandleFunc("/api/downloads/resume", w.downloadResume)
	mux.HandleFunc("/api/downloads/retry_failed", w.downloadRetryFailed)
	mux.HandleFunc("/api/storage", w.storage)
	mux.HandleFunc("/api/dialogs", w.dialogs)
	mux.HandleFunc("/api/export", w.export)
	mux.HandleFunc("/api/serials", w.serials)
	mux.HandleFunc("/api/materials", w.materials)
	mux.HandleFunc("/api/material/refresh", w.materialRefresh)
	mux.HandleFunc("/api/serial", w.serialDetail)
	mux.HandleFunc("/api/purge", w.purge)
	mux.HandleFunc("/api/notify/rules", w.notifyRules)
	mux.HandleFunc("/api/notify/test", w.notifyTest)
	mux.HandleFunc("/api/autoclick", w.autoClickSettings)
	mux.HandleFunc("/api/autoclick/scan", w.autoClickScan)
	mux.HandleFunc("/api/autoclick/test", w.autoClickTest)
	mux.HandleFunc("/api/autoclick/log", w.autoClickLog)
	mux.HandleFunc("/api/autoclick/click", w.autoClickClick)
	return w.requestMiddleware(w.csrfGuard(w.loginRoutes(w.authMiddleware(w.accountGuard(mux)))))
}

// accountGuard keeps a configured server usable after the operator removes
// every account: handlers would otherwise resolve a nil store and panic. The
// account endpoints stay reachable so one can be added back, and pages still
// render so the UI can explain the state.
func (w *WebServer) accountGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.Accounts == nil || w.Accounts.Current() != nil {
			next.ServeHTTP(rw, r)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/accounts"),
			r.URL.Path == "/api/auth/status":
			next.ServeHTTP(rw, r)
			return
		case r.URL.Path == "/healthz":
			next.ServeHTTP(rw, r)
			return
		case r.URL.Path == "/readyz":
			// Ready-but-empty is a real state, not a panic: probes must get an
			// answer, and the answer is "not serving an account".
			http.Error(rw, "no active account", http.StatusServiceUnavailable)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") ||
			strings.HasPrefix(r.URL.Path, "/media/") ||
			strings.HasPrefix(r.URL.Path, "/avatar/") {
			http.Error(rw, "no active account: add one from the account panel", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// getOnly enforces the HTTP method on read-only endpoints.
func getOnly(rw http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

// serverError logs the detail and answers with a generic body so table and
// SQL internals never reach the client.
func (w *WebServer) serverError(rw http.ResponseWriter, err error) {
	slog.Error("archive web request failed", slog.Any("error", err))
	http.Error(rw, "internal error", http.StatusInternalServerError)
}

// requireCtl rejects control-plane requests when the server is read-only.
func (w *WebServer) requireCtl(rw http.ResponseWriter) bool {
	if w.ctl() == nil {
		http.Error(rw, "management disabled: restart serve with telegram.app-id/app-hash configured to enable login and monitoring", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (w *WebServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Body != nil {
			r.Body = http.MaxBytesReader(rw, r.Body, 1<<20)
		}
		// Health probes are only reachable without credentials when no auth is
		// configured at all. Once a token or basic auth is set, an open
		// /healthz would let anyone fingerprint and locate the archive, and an
		// open /readyz would let them drive database work.
		if !w.Auth.authConfigured() {
			next.ServeHTTP(rw, r)
			return
		}
		if w.Auth.Token != "" {
			const prefix = "Bearer "
			value := r.Header.Get("Authorization")
			if len(value) >= len(prefix) && subtle.ConstantTimeCompare([]byte(value[:len(prefix)]), []byte(prefix)) == 1 &&
				subtle.ConstantTimeCompare([]byte(value[len(prefix):]), []byte(w.Auth.Token)) == 1 {
				next.ServeHTTP(rw, r)
				return
			}
			rw.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Form login: a signed, expiring cookie replaces the Basic dialog.
		// Bearer is checked first above so scripts can still call the API.
		if w.loginEnabled() {
			if c, err := r.Cookie(sessionCookie); err == nil && w.validSession(c.Value, time.Now()) {
				next.ServeHTTP(rw, r)
				return
			}
			w.denySession(rw, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if ok &&
			subtle.ConstantTimeCompare([]byte(user), []byte(w.Auth.BasicUser)) == 1 &&
			subtle.ConstantTimeCompare([]byte(pass), []byte(w.Auth.BasicPassword)) == 1 {
			next.ServeHTTP(rw, r)
			return
		}
		rw.Header().Set("WWW-Authenticate", `Basic realm="tdl-archive"`)
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
	})
}

func (w *WebServer) healthz(rw http.ResponseWriter, _ *http.Request) {
	writeJSON(rw, map[string]string{"status": "ok"})
}

func (w *WebServer) readyz(rw http.ResponseWriter, r *http.Request) {
	if err := w.store().DB().PingContext(r.Context()); err != nil {
		http.Error(rw, "not ready", http.StatusServiceUnavailable)
		return
	}
	var tables int
	if err := w.store().DB().QueryRowContext(r.Context(), `
		SELECT count(*)
		FROM sqlite_master
		WHERE type = 'table' AND name IN ('messages', 'media', 'jobs')
	`).Scan(&tables); err != nil || tables != 3 {
		http.Error(rw, "not ready", http.StatusServiceUnavailable)
		return
	}
	writeJSON(rw, map[string]string{"status": "ready"})
}

func (w *WebServer) index(rw http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(rw, r) // unknown API route: JSON clients must not get HTML
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The UI uses inline handlers, so script-src keeps 'unsafe-inline'; the
	// directives that still matter are connect/img/media (no exfiltration to a
	// third-party origin), frame-ancestors (no clickjacking) and base-uri.
	rw.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; "+
			"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	rw.Header().Set("Cache-Control", "no-store")
	_, _ = rw.Write([]byte(indexHTML))
}
func (w *WebServer) stats(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	m, media, t, e := w.store().Stats(r.Context())
	if e != nil {
		w.serverError(rw, e)
		return
	}
	writeJSON(rw, map[string]int64{"messages": m, "media": media, "monitors": t})
}
func (w *WebServer) deleted(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	v, err := w.store().ListDeletedMessages(r.Context(), store.DeletedFilter{
		Query: r.URL.Query().Get("q"), Page: page, PageSize: pageSize,
	})
	if err != nil {
		http.Error(rw, err.Error(), 500)
		return
	}
	writeJSON(rw, deletedAPIResponse{Items: v.Items, Total: v.Total, Page: v.Page, PageSize: v.PageSize})
}

func (w *WebServer) edited(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	v, err := w.store().ListEditedMessages(r.Context(), store.EditedFilter{
		Query: r.URL.Query().Get("q"), Page: page, PageSize: pageSize,
	})
	if err != nil {
		http.Error(rw, err.Error(), 500)
		return
	}
	items := make([]editedAPIItem, 0, len(v.Items))
	for _, item := range v.Items {
		items = append(items, editedAPIItem{Message: item.MessageView, EditCount: item.EditCount})
	}
	writeJSON(rw, editedAPIResponse{Items: items, Total: v.Total, Page: v.Page, PageSize: v.PageSize})
}

func (w *WebServer) mediaList(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	kind := store.MediaKind(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind"))))
	if kind != store.MediaKindImage && kind != store.MediaKindVideo && kind != store.MediaKindFile {
		http.Error(rw, "kind must be image, video, or file", http.StatusBadRequest)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	chat, _ := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	topic, _ := strconv.ParseInt(r.URL.Query().Get("topic"), 10, 64)
	v, err := w.store().ListMediaMessages(r.Context(), store.MediaFilter{
		Kind: kind, Query: r.URL.Query().Get("q"), ChatID: chat, Topic: topic, Page: page, PageSize: pageSize,
		// The gallery collapses identical files by default; ?dedupe=0 shows every
		// message so an operator can still audit how many times a bot reposted.
		Dedupe: r.URL.Query().Get("dedupe") != "0",
	})
	if err != nil {
		w.serverError(rw, err)
		return
	}
	ids := make([]int64, 0, len(v.Items))
	for _, item := range v.Items {
		ids = append(ids, item.RowID)
	}
	errs, err := w.store().MediaErrorsByRowIDs(r.Context(), ids)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	items := make([]mediaAPIItem, 0, len(v.Items))
	for _, item := range v.Items {
		x := mediaAPIItem{MediaItem: item}
		if e, ok := errs[item.RowID]; ok {
			x.MediaErrorKind = e.Kind
			x.MediaErrorDetail = e.Detail
			x.MediaNextRetryAt = e.NextRetryAt
		}
		items = append(items, x)
	}
	writeJSON(rw, mediaAPIResponse{Items: items, Total: v.Total, Page: v.Page, PageSize: v.PageSize, Kind: kind})
}

func (w *WebServer) linksList(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	v, err := w.store().ListLinks(r.Context(), store.LinkFilter{
		Query: r.URL.Query().Get("q"), Domain: r.URL.Query().Get("domain"), Page: page, PageSize: pageSize,
	})
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, linksAPIResponse{Items: v.Items, Total: v.Total, Page: v.Page, PageSize: v.PageSize})
}

func (w *WebServer) linkDomains(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	items, err := w.store().ListLinkDomains(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		w.serverError(rw, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(rw, linkDomainsAPIResponse{Items: items})
}

func (w *WebServer) targets(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ts, e := w.store().Targets(r.Context())
		if e != nil {
			w.serverError(rw, e)
			return
		}
		writeJSON(rw, ts)
	case http.MethodPost:
		if !w.requireCtl(rw) {
			return
		}
		var req struct {
			Target            string   `json:"target"`
			Since             string   `json:"since"`
			Limit             int      `json:"limit"`
			DownloadMedia     *bool    `json:"download_media"`
			Keywords          []string `json:"keywords"`
			ExcludeKeywords   []string `json:"exclude_keywords"`
			MediaMaxSize      int64    `json:"media_max_size"`
			AutoClick         bool     `json:"autoclick_enabled"`
			AutoClickKeywords []string `json:"autoclick_keywords"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Target) == "" {
			http.Error(rw, "target is required", http.StatusBadRequest)
			return
		}
		if req.Limit <= 0 {
			req.Limit = 1000
		}
		download := true
		if req.DownloadMedia != nil {
			download = *req.DownloadMedia
		}
		job, err := w.ctl().AddMonitor(AddMonitorOptions{
			Target: req.Target, Since: req.Since, Limit: req.Limit, Download: download,
			Keywords: req.Keywords, ExcludeKeywords: req.ExcludeKeywords, MediaMaxSize: req.MediaMaxSize,
			AutoClick: req.AutoClick, AutoClickKeywords: req.AutoClickKeywords,
		})
		if err != nil {
			http.Error(rw, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(rw, job)
	case http.MethodPatch:
		if !w.requireCtl(rw) {
			return
		}
		var req struct {
			ChatID            int64     `json:"chat_id"`
			DownloadMedia     *bool     `json:"download_media"`
			Enabled           *bool     `json:"enabled"`
			MediaMaxSize      *int64    `json:"media_max_size"`
			Keywords          *[]string `json:"keywords"`
			ExcludeKeywords   *[]string `json:"exclude_keywords"`
			AutoClick         *bool     `json:"autoclick_enabled"`
			AutoClickKeywords *[]string `json:"autoclick_keywords"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ChatID == 0 {
			http.Error(rw, "chat_id is required", http.StatusBadRequest)
			return
		}
		rules := store.TargetRules{
			Enabled:       req.Enabled,
			DownloadMedia: req.DownloadMedia,
			MediaMaxSize:  req.MediaMaxSize,
		}
		if req.Keywords != nil {
			rules.Keywords = *req.Keywords
		}
		if req.ExcludeKeywords != nil {
			rules.ExcludeKeywords = *req.ExcludeKeywords
		}
		if req.AutoClick != nil {
			rules.AutoClick = req.AutoClick
		}
		if req.AutoClickKeywords != nil {
			rules.AutoClickKeywords = *req.AutoClickKeywords
		}
		if err := w.store().UpdateTargetRules(r.Context(), req.ChatID, rules); err != nil {
			http.Error(rw, err.Error(), http.StatusNotFound)
			return
		}
		t, err := w.store().Target(r.Context(), req.ChatID)
		if err != nil {
			w.serverError(rw, err)
			return
		}
		writeJSON(rw, t)
	case http.MethodDelete:
		if !w.requireCtl(rw) {
			return
		}
		chatID, err := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
		if err != nil || chatID == 0 {
			http.Error(rw, "chat_id is required", http.StatusBadRequest)
			return
		}
		if err := w.ctl().RemoveMonitor(chatID); err != nil {
			w.serverError(rw, err)
			return
		}
		writeJSON(rw, map[string]string{"status": "removed"})
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (w *WebServer) monitorSync(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	var req struct {
		Target string `json:"target"`
		Limit  int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Target) == "" {
		http.Error(rw, "target is required", http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 500
	}
	job, err := w.ctl().SyncMonitor(req.Target, req.Limit)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(rw, job)
}

type authStatusAPIResponse struct {
	Managed    bool     `json:"managed"`
	Authorized bool     `json:"authorized"`
	State      string   `json:"state"`
	User       *WebUser `json:"user,omitempty"`
	Error      string   `json:"error,omitempty"`
	QR         string   `json:"qr"`
	QRErr      string   `json:"qr_error,omitempty"`
	Hint       string   `json:"hint,omitempty"`
}

func (w *WebServer) authStatus(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	// Account support present but nothing active means the operator removed
	// every account. That is not read-only mode, and reporting it as such would
	// point them at config.yaml instead of at the add-account button.
	if w.Accounts != nil && w.Accounts.Current() == nil {
		writeJSON(rw, authStatusAPIResponse{
			Managed: true, State: "no_account",
			Hint: "还没有账号，点击「添加账号」新建一个并扫码登录",
		})
		return
	}
	if w.ctl() == nil {
		writeJSON(rw, authStatusAPIResponse{
			Managed: false, State: "unmanaged",
			Hint: "只读模式：在 config.yaml 配置 telegram.app-id/app-hash 后重启 serve，即可在网页端扫码登录并管理监控",
		})
		return
	}
	st := w.ctl().State()
	writeJSON(rw, authStatusAPIResponse{
		Managed: true, Authorized: st.Authorized, State: st.State,
		User: st.User, Error: st.Error, QR: w.ctl().QRState(), QRErr: w.ctl().QRError(),
	})
}

func (w *WebServer) loginQRStart(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	if err := w.ctl().StartQRLogin(); err != nil {
		http.Error(rw, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(rw, map[string]string{"status": "pending"})
}

func (w *WebServer) loginQRStatus(rw http.ResponseWriter, r *http.Request) {
	if !w.requireCtl(rw) {
		return
	}
	st := w.ctl().State()
	writeJSON(rw, map[string]any{
		"qr":         w.ctl().QRState(),
		"authorized": st.Authorized,
		"user":       st.User,
		"error":      w.ctl().QRError(),
	})
}

func (w *WebServer) loginQRImage(rw http.ResponseWriter, r *http.Request) {
	if !w.requireCtl(rw) {
		return
	}
	token := w.ctl().QRToken()
	if token == "" {
		http.Error(rw, "no pending QR token", http.StatusNotFound)
		return
	}
	png, err := qrcode.Encode(token, qrcode.Medium, 320)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	rw.Header().Set("Content-Type", "image/png")
	rw.Header().Set("Cache-Control", "no-store")
	_, _ = rw.Write(png)
}

func (w *WebServer) loginQRCancel(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	w.ctl().CancelQR()
	writeJSON(rw, map[string]string{"status": "cancelled"})
}

func (w *WebServer) loginPassword(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Password == "" {
		http.Error(rw, "password is required", http.StatusBadRequest)
		return
	}
	if err := w.ctl().SubmitPassword(req.Password); err != nil {
		http.Error(rw, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(rw, map[string]string{"status": "submitted"})
}

func (w *WebServer) jobs(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) || !w.requireCtl(rw) {
		return
	}
	parseList := func(v string) []string {
		var out []string
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := w.store().ListJobs(r.Context(), store.JobFilter{Status: parseList(r.URL.Query().Get("status")), Kind: parseList(r.URL.Query().Get("kind")), Limit: limit})
	if err != nil {
		w.serverError(rw, err)
		return
	}
	out := make([]Job, 0, len(jobs))
	for _, dj := range jobs {
		// Archived 只在 live jobs map 里有值，服务一重启就归零 —— 面板上会
		// 出现「✓ 消息完成 · 归档 0 条」这种自相矛盾的卡片。DB 里的
		// progress_done 就是同一件事，先拿它兜底，live 再覆盖。
		j := Job{ID: dj.ID, Kind: dj.Kind, State: dj.Status, Archived: dj.ProgressDone, ProgressDone: dj.ProgressDone, ProgressTotal: dj.ProgressTotal, Error: dj.Error, FinishedAt: dj.FinishedAt}
		if dj.StartedAt != nil {
			j.StartedAt = *dj.StartedAt
		} else {
			j.StartedAt = dj.CreatedAt
		}
		if dj.ChatID != nil {
			j.ChatID = *dj.ChatID
			j.Target = fmt.Sprintf("%d", *dj.ChatID)
		}
		w.ctl().jobsMu.Lock()
		if live, ok := w.ctl().jobs[dj.ID]; ok {
			j.Target = live.Target
			j.Archived = live.Archived
		}
		w.ctl().jobsMu.Unlock()
		out = append(out, j)
	}
	writeJSON(rw, jobsAPIResponse{Items: out})
}

func (w *WebServer) jobRoute(rw http.ResponseWriter, r *http.Request) {
	if !w.requireCtl(rw) {
		return
	}
	idStr := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "/")
	if r.Method == http.MethodPost && strings.HasSuffix(idStr, "/cancel") {
		idStr = strings.TrimSuffix(idStr, "/cancel")
		id, err := strconv.ParseInt(strings.Trim(idStr, "/"), 10, 64)
		if err != nil {
			http.Error(rw, "invalid job id", 400)
			return
		}
		if err := w.ctl().CancelJob(id); err != nil {
			http.Error(rw, err.Error(), 409)
			return
		}
		writeJSON(rw, map[string]string{"status": "cancel_requested"})
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(rw, "invalid job id", 400)
		return
	}
	if !getOnly(rw, r) {
		return
	}
	j, err := w.store().GetJob(r.Context(), id)
	if err != nil {
		http.Error(rw, "job not found", 404)
		return
	}
	out := Job{ID: j.ID, Kind: j.Kind, State: j.Status, ProgressDone: j.ProgressDone, ProgressTotal: j.ProgressTotal, Error: j.Error, FinishedAt: j.FinishedAt}
	if j.StartedAt != nil {
		out.StartedAt = *j.StartedAt
	} else {
		out.StartedAt = j.CreatedAt
	}
	if j.ChatID != nil {
		out.ChatID = *j.ChatID
		out.Target = fmt.Sprintf("%d", *j.ChatID)
	}
	writeJSON(rw, out)
}

func (w *WebServer) downloadSummary(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) || !w.requireCtl(rw) {
		return
	}
	x, err := w.store().DownloadSummary(r.Context(), w.ctl().mediaRunning.Load(), w.ctl().MediaPaused())
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, downloadsSummaryResponse(x))
}

func (w *WebServer) downloadActive(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) || !w.requireCtl(rw) {
		return
	}
	jobs, err := w.store().ListJobs(r.Context(), store.JobFilter{Status: []string{"running"}, Kind: []string{"backfill", "sync"}, Limit: 20})
	if err != nil {
		w.serverError(rw, err)
		return
	}
	type item struct {
		Name          string `json:"name"`
		ProgressDone  int    `json:"progress_done"`
		ProgressTotal *int   `json:"progress_total,omitempty"`
		SpeedBPS      int64  `json:"speed_bps,omitempty"`
		ETASeconds    *int64 `json:"eta_seconds,omitempty"`
		ErrorKind     string `json:"error_kind,omitempty"`
	}
	out := make([]item, 0, len(jobs))
	for _, j := range jobs {
		name := j.Kind
		if j.ChatID != nil {
			name = fmt.Sprintf("%s · %d", j.Kind, *j.ChatID)
		}
		out = append(out, item{Name: name, ProgressDone: j.ProgressDone, ProgressTotal: j.ProgressTotal})
	}
	failed, err := w.store().ListFailedMedia(r.Context(), 10)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, map[string]any{"items": out, "failed": failed})
}

func (w *WebServer) downloadPause(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	w.ctl().SetMediaPaused(true)
	writeJSON(rw, map[string]string{"status": "paused"})
}
func (w *WebServer) downloadResume(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	w.ctl().SetMediaPaused(false)
	writeJSON(rw, map[string]string{"status": "resumed"})
}
func (w *WebServer) downloadRetryFailed(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	n, err := w.store().RetryFailedMedia(r.Context())
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, map[string]int64{"retried": n})
}

// dialogs lists the account's joined chats/channels and private conversations
// so the UI can offer a picker instead of manual target input.
func (w *WebServer) dialogs(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	rt, _, err := w.ctl().runtimeForOps()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusConflict)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	ds, err := ListDialogs(r.Context(), rt, limit)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	monitored := map[int64]bool{}
	if ts, e := w.store().Targets(r.Context()); e == nil {
		for _, t := range ts {
			monitored[t.ChatID] = true
		}
	}
	for i := range ds {
		ds[i].Monitored = monitored[ds[i].ChatID]
	}
	writeJSON(rw, ds)
}

type storageAPIResponse struct {
	DBBytes      int64  `json:"db_bytes"`
	WALBytes     int64  `json:"wal_bytes"`
	ObjectsBytes int64  `json:"objects_bytes"`
	ObjectsCount int    `json:"objects_count"`
	StagingBytes int64  `json:"staging_bytes"`
	BackupsBytes int64  `json:"backups_bytes"`
	BackupsCount int    `json:"backups_count"`
	FreeBytes    uint64 `json:"free_bytes"`
	MinFreeSpace int64  `json:"min_free_space"`
	LatestBackup string `json:"latest_backup,omitempty"`
}

func (w *WebServer) storage(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	// Derive every path from the store actually serving this request: the CLI
	// happens to place it under consts.DataDir, but the storage numbers must
	// describe THIS database, not whatever the default deployment layout is.
	root := filepath.Dir(w.store().Path())
	resp := storageAPIResponse{MinFreeSpace: DefaultMinFreeSpace}
	resp.DBBytes = fileSize(w.store().Path())
	resp.WALBytes = fileSize(w.store().Path() + "-wal")
	resp.ObjectsBytes, resp.ObjectsCount = dirUsage(filepath.Join(root, "objects"))
	resp.StagingBytes, _ = dirUsage(filepath.Join(root, "staging"))
	resp.BackupsBytes, resp.BackupsCount = dirUsage(filepath.Join(root, "backups"))
	resp.LatestBackup = LatestBackup(w.store().Path())
	if free, err := FreeDiskBytes(filepath.Join(root, "objects")); err == nil {
		resp.FreeBytes = free
	}
	writeJSON(rw, resp)
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

func dirUsage(dir string) (int64, int) {
	var size int64
	var count int
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			size += fi.Size()
			count++
		}
		return nil
	})
	return size, count
}
func (w *WebServer) search(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(rw, "q is required", 400)
		return
	}
	chat, _ := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rs, e := w.store().SearchAdvanced(r.Context(), q, store.SearchOptions{ChatID: chat, Limit: limit, Offset: offset})
	if e != nil {
		w.serverError(rw, e)
		return
	}
	writeJSON(rw, rs)
}
func (w *WebServer) messagesPage(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	chat, _ := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	topic, _ := strconv.ParseInt(r.URL.Query().Get("topic"), 10, 64)
	v, err := w.store().ListMessages(r.Context(), store.MessageFilter{Query: r.URL.Query().Get("q"), ChatID: chat, Topic: topic, Page: page, PageSize: pageSize})
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, messagesAPIResponse{Items: v.Items, Total: v.Total, Page: v.Page, PageSize: v.PageSize})
}

// topics lists the forum topics of one chat for the topic bar.
func (w *WebServer) topics(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	chat, err := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	if err != nil || chat == 0 {
		http.Error(rw, "chat is required", http.StatusBadRequest)
		return
	}
	if !w.store().ChatIsForum(r.Context(), chat) {
		writeJSON(rw, map[string]any{"forum": false, "topics": []store.TopicSummary{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ts, err := w.store().Topics(r.Context(), chat, limit)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, map[string]any{"forum": true, "topics": ts})
}

func (w *WebServer) messageByRowID(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	rowID, err := strconv.ParseInt(r.URL.Query().Get("rowid"), 10, 64)
	if err != nil || rowID <= 0 {
		http.Error(rw, "rowid is required", http.StatusBadRequest)
		return
	}
	v, err := w.store().GetMessageByRowID(r.Context(), rowID)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(rw, messageAPIResponse{Message: v})
}

// messageGroup returns every member of one album so the detail view can render
// the post whole. Querying by grouped id alone would need no chat, but scoping
// it keeps a stale link from pulling another chat's rows with the same numeric
// group id.
func (w *WebServer) messageGroup(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	gid, err := strconv.ParseInt(r.URL.Query().Get("gid"), 10, 64)
	if err != nil || gid == 0 {
		http.Error(rw, "gid is required", http.StatusBadRequest)
		return
	}
	chat, _ := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	vs, err := w.store().MessageViewsByGroup(r.Context(), chat, gid)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, messagesAPIResponse{Items: vs, Total: len(vs), Page: 1, PageSize: len(vs)})
}

func (w *WebServer) messages(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	chat, _ := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rs, e := w.store().RecentMessages(r.Context(), chat, limit)
	if e != nil {
		w.serverError(rw, e)
		return
	}
	writeJSON(rw, rs)
}
func (w *WebServer) message(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	chat, e1 := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	id, e2 := strconv.Atoi(r.URL.Query().Get("id"))
	if e1 != nil || e2 != nil {
		http.Error(rw, "chat and id are required", 400)
		return
	}
	v, e := w.store().MessageView(r.Context(), chat, id)
	if e != nil {
		http.Error(rw, e.Error(), 404)
		return
	}
	writeJSON(rw, v)
}
func (w *WebServer) edits(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	chat, e1 := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	id, e2 := strconv.Atoi(r.URL.Query().Get("id"))
	if e1 != nil || e2 != nil {
		http.Error(rw, "chat and id are required", 400)
		return
	}
	v, e := w.store().Edits(r.Context(), chat, id)
	if e != nil {
		w.serverError(rw, e)
		return
	}
	writeJSON(rw, v)
}
func (w *WebServer) destinations(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	ds, err := w.store().Destinations(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), 500)
		return
	}
	writeJSON(rw, ds)
}

// sha256HexRE validates the content address in a /media/ path.
var sha256HexRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (w *WebServer) media(rw http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/media/")
	variant, sha := "original", path
	if strings.HasPrefix(path, "thumb/") {
		variant, sha = "thumb", strings.TrimPrefix(path, "thumb/")
	}
	if strings.HasPrefix(path, "preview/") {
		variant, sha = "preview", strings.TrimPrefix(path, "preview/")
	}
	if !sha256HexRE.MatchString(sha) {
		http.NotFound(rw, r)
		return
	}
	var p string
	if variant != "original" {
		var err error
		p, err = w.store().MediaVariant(r.Context(), sha, variant)
		if err != nil || p == "" {
			http.NotFound(rw, r)
			return
		}
		// generated previews are always jpeg
		rw.Header().Set("Content-Type", "image/jpeg")
	} else {
		p = filepath.Join(consts.DataDir, "archive", "objects", sha[:2], sha[2:4], sha)
		// Never let http.ServeFile sniff the type: a monitored .html payload
		// served as text/html would execute in the archive origin (stored XSS).
		// The allowlist matters too: an image/svg+xml document is a script, so
		// it is served as an attachment like any other active content.
		mime := w.store().FilesMime(r.Context(), sha)
		if inlineMIME(mime) {
			rw.Header().Set("Content-Type", mime)
		} else {
			rw.Header().Set("Content-Type", "application/octet-stream")
			rw.Header().Set("Content-Disposition", "attachment")
		}
	}
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	// Cached media is per-session: a shared proxy must not hand one operator's
	// attachments to another request, and the sandbox CSP neutralises script if
	// a browser ever renders an unexpected type inline.
	rw.Header().Set("Cache-Control", "private, max-age=3600")
	rw.Header().Set("Vary", "Authorization, Cookie")
	rw.Header().Set("Content-Security-Policy", "sandbox")
	root, _ := filepath.Abs(filepath.Join(consts.DataDir, "archive"))
	abs, _ := filepath.Abs(p)
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		http.NotFound(rw, r)
		return
	}
	http.ServeFile(rw, r, abs)
}

// inlineMIME reports whether a content type may be rendered inline. SVG is
// deliberately excluded: it is a script document, not a picture, and serving it
// inline from the archive origin would run archived content with the session
// cookie attached.
func inlineMIME(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch m {
	case "image/svg+xml", "image/svg", "text/html", "application/xhtml+xml":
		return false
	}
	return strings.HasPrefix(m, "image/") ||
		strings.HasPrefix(m, "video/") ||
		strings.HasPrefix(m, "audio/")
}
func (w *WebServer) avatar(rw http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/avatar/"), "/"), "/")
	if len(parts) != 2 {
		http.NotFound(rw, r)
		return
	}
	id, e := strconv.ParseInt(parts[1], 10, 64)
	if e != nil {
		http.NotFound(rw, r)
		return
	}
	p, e := w.store().AvatarPath(r.Context(), parts[0], id)
	if e != nil || p == "" {
		http.NotFound(rw, r)
		return
	}
	root, _ := filepath.Abs(filepath.Join(consts.DataDir, "archive", "avatars"))
	abs, _ := filepath.Abs(p)
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		http.NotFound(rw, r)
		return
	}
	http.ServeFile(rw, r, abs)
}

// writeJSON 序列化 JSON 响应；nil slice 一律输出 []，
// 避免前端/脚本拿到 null 还要额外判空（列表接口保持一致的空集合语义）。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
		v = reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	_ = json.NewEncoder(w).Encode(v)
}

var indexHTML = indexHTMLAsset
