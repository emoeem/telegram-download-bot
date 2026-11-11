//go:build !android

package license

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 服务端错误码。客户端按 code 分支，不解析中文文案——文案会改，code 不会。
const (
	CodeOK           = "ok"
	CodeUnknownKey   = "unknown_key"
	CodeBoundElse    = "bound_elsewhere"
	CodeRevoked      = "revoked"
	CodeExpired      = "expired"
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeRateLimited  = "rate_limited"
)

// ActivateRequest 是 /v1/activate 与 /v1/verify 的请求体。
type ActivateRequest struct {
	Key    string `json:"key"`
	Device string `json:"device"`
}

// ActivateResponse 是激活/校验的统一响应。
//
// 成功时携带剩余有效期，让客户端可以据此决定何时续期；失败时 code 是机器可读
// 的原因，message 给人看。
type ActivateResponse struct {
	OK        bool       `json:"ok"`
	Code      string     `json:"code"`
	Message   string     `json:"message,omitempty"`
	Device    string     `json:"device,omitempty"`
	Kind      string     `json:"kind,omitempty"`
	Forever   bool       `json:"forever,omitempty"`
	DaysLeft  int        `json:"days_left,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	BoundAt   *time.Time `json:"bound_at,omitempty"`
	ServerNow time.Time  `json:"server_now"`
}

// AdminRequest 是运营接口的请求体。
type AdminRequest struct {
	Key       string     `json:"key"`
	Kind      string     `json:"kind,omitempty"`
	IssuedAt  *time.Time `json:"issued_at,omitempty"`
	ExpiryDay uint16     `json:"expiry_day,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// ListResponse 是 /v1/keys 的响应。密钥全文只在运营接口出现。
type ListResponse struct {
	OK    bool            `json:"ok"`
	Count int             `json:"count"`
	Keys  []RegistryEntry `json:"keys"`
}

// Server 是在线授权服务。
//
// 它解决的问题是离线签名做不到的那一半：一把密钥只能绑一台设备、可以远程撤销、
// 换机必须运营放行。离线签名保证"密钥不是我造的"，这个服务保证"一把密钥只能
// 用在一个地方"。
type Server struct {
	reg        *Registry
	adminToken string
	limiter    *rateLimiter
	log        *slog.Logger
	now        func() time.Time
}

// NewServer 构造服务。adminToken 为空时，所有运营接口关闭（只保留激活/校验）。
func NewServer(reg *Registry, adminToken string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		reg:        reg,
		adminToken: adminToken,
		limiter:    newRateLimiter(30, time.Minute),
		log:        log,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// Handler 返回带全部路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"keys":   s.reg.Count(),
			"server": "tdl-license",
		})
	})
	mux.HandleFunc("/v1/activate", s.handleActivate)
	mux.HandleFunc("/v1/verify", s.handleVerify)
	mux.HandleFunc("/v1/revoke", s.adminOnly(s.handleRevoke))
	mux.HandleFunc("/v1/release", s.adminOnly(s.handleRelease))
	mux.HandleFunc("/v1/admit", s.adminOnly(s.handleAdmit))
	mux.HandleFunc("/v1/keys", s.adminOnly(s.handleList))
	return mux
}

// clientIP 从请求里取客户端地址。
//
// Cloudflare Tunnel 会把真实来源放在 CF-Connecting-IP；本地直连时用 RemoteAddr。
// 这个值只用于限速分桶，不用于任何鉴权判断——伪造它最多让你换个桶继续试，
// 而激活本身还需要一把有效密钥。
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) allow(r *http.Request) bool {
	return s.limiter.allow(clientIP(r))
}

// decode 解析请求体，兼容 JSON 与表单/查询参数。
//
// 兼容 query 是为了运维方便：`curl '.../v1/verify?key=X&device=Y'` 比手搓
// JSON 快得多，而运营在半夜处理客户问题时不希望先打开编辑器。
func decode(r *http.Request, dst *ActivateRequest) error {
	if r.Method == http.MethodGet {
		dst.Key = strings.TrimSpace(r.URL.Query().Get("key"))
		dst.Device = strings.TrimSpace(r.URL.Query().Get("device"))
		return nil
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<10)).Decode(dst); err != nil {
		// 退一步试表单编码，postman/curl -d 默认就是这种。
		_ = r.ParseForm()
		dst.Key = strings.TrimSpace(r.PostForm.Get("key"))
		dst.Device = strings.TrimSpace(r.PostForm.Get("device"))
		if dst.Key == "" {
			return fmt.Errorf("请求体既不是 JSON 也不是表单: %w", err)
		}
	}
	dst.Key = strings.TrimSpace(dst.Key)
	dst.Device = strings.TrimSpace(dst.Device)
	return nil
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r) {
		writeJSON(w, http.StatusTooManyRequests, ActivateResponse{
			Code: CodeRateLimited, Message: "请求过于频繁，请稍后再试", ServerNow: s.now(),
		})
		return
	}

	var req ActivateRequest
	if err := decode(r, &req); err != nil || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, ActivateResponse{
			Code: CodeBadRequest, Message: "缺少 key", ServerNow: s.now(),
		})
		return
	}

	entry, err := s.reg.Activate(req.Key, req.Device, s.now())
	if err != nil {
		s.replyErr(w, err, entry)
		return
	}
	s.log.Info("licence activated",
		slog.String("key", KeyFingerprint(req.Key)),
		slog.String("device", entry.Device),
		slog.Int("days_left", entry.DaysLeft(s.now())))
	writeJSON(w, http.StatusOK, s.successResp(entry))
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !s.allow(r) {
		writeJSON(w, http.StatusTooManyRequests, ActivateResponse{
			Code: CodeRateLimited, Message: "请求过于频繁，请稍后再试", ServerNow: s.now(),
		})
		return
	}

	var req ActivateRequest
	if err := decode(r, &req); err != nil || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, ActivateResponse{
			Code: CodeBadRequest, Message: "缺少 key", ServerNow: s.now(),
		})
		return
	}

	entry, err := s.reg.Verify(req.Key, req.Device, s.now())
	if err != nil {
		s.replyErr(w, err, entry)
		return
	}
	writeJSON(w, http.StatusOK, s.successResp(entry))
}

func (s *Server) successResp(e RegistryEntry) ActivateResponse {
	resp := ActivateResponse{
		OK:        true,
		Code:      CodeOK,
		Device:    e.Device,
		Kind:      e.Kind,
		Forever:   e.Forever,
		DaysLeft:  e.DaysLeft(s.now()),
		ServerNow: s.now(),
	}
	if !e.BoundAt.IsZero() {
		t := e.BoundAt
		resp.BoundAt = &t
	}
	if !e.Forever {
		t := e.ExpiresAt()
		resp.ExpiresAt = &t
	}
	return resp
}

// replyErr 把内部错误映射成 HTTP 状态 + code。
//
// 状态码的选择是有意的：未知密钥回 403 而不是 404，因为 404 会告诉扫描者
// "这个 key 格式对但不存在"，而 403 与"已绑定其他设备"在外观上一致，
// 不给密钥枚举提供区分信号。
func (s *Server) replyErr(w http.ResponseWriter, err error, entry RegistryEntry) {
	status := http.StatusForbidden
	code := CodeBadRequest
	msg := err.Error()

	switch {
	case errors.Is(err, ErrKeyUnknown):
		code, msg = CodeUnknownKey, "密钥无效"
	case errors.Is(err, ErrKeyBoundElsewhere):
		code, msg = CodeBoundElse, "密钥已绑定到其他设备"
	case errors.Is(err, ErrKeyRevoked):
		code, msg = CodeRevoked, "密钥已被撤销"
	case errors.Is(err, ErrKeyExpired):
		code, msg = CodeExpired, "密钥已过期"
	case errors.Is(err, ErrDeviceMissing):
		status, code = http.StatusBadRequest, CodeBadRequest
	}

	s.log.Warn("licence refused",
		slog.String("code", code),
		slog.String("device", entry.Device),
		slog.Int("refusals", entry.Refusals))

	writeJSON(w, status, ActivateResponse{Code: code, Message: msg, ServerNow: s.now()})
}

// adminOnly 包装运营接口：要求 Authorization: Bearer <token>。
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"ok": false, "code": CodeUnauthorized,
				"message": "本服务器未配置 admin token，运营接口已关闭",
			})
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		// 定长比较，避免通过响应时间猜出 token。
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.adminToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"ok": false, "code": CodeUnauthorized, "message": "admin token 不正确",
			})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	var req AdminRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<10)).Decode(&req); err != nil || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": CodeBadRequest, "message": "缺少 key"})
		return
	}
	if _, err := s.reg.Revoke(req.Key, s.now()); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "code": CodeUnknownKey, "message": err.Error()})
		return
	}
	s.log.Warn("licence revoked", slog.String("key", KeyFingerprint(req.Key)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": CodeOK})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req AdminRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<10)).Decode(&req); err != nil || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": CodeBadRequest, "message": "缺少 key"})
		return
	}
	entry, err := s.reg.Release(req.Key)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "code": CodeUnknownKey, "message": err.Error()})
		return
	}
	s.log.Info("licence released", slog.String("key", KeyFingerprint(req.Key)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": CodeOK, "entry": entry})
}

func (s *Server) handleAdmit(w http.ResponseWriter, r *http.Request) {
	var req AdminRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<10)).Decode(&req); err != nil || req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": CodeBadRequest, "message": "缺少 key"})
		return
	}
	issued := s.now()
	if req.IssuedAt != nil {
		issued = req.IssuedAt.UTC()
	}
	entry, err := s.reg.Admit(req.Key, req.Kind, issued, req.ExpiryDay, req.Note)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": CodeBadRequest, "message": err.Error()})
		return
	}
	s.log.Info("licence admitted", slog.String("key", KeyFingerprint(req.Key)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": CodeOK, "entry": entry})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	keys := s.reg.List()
	writeJSON(w, http.StatusOK, ListResponse{OK: true, Count: len(keys), Keys: keys})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rateLimiter 是每 IP 的滑动窗口限速。
//
// 目的不是防 DDoS（那该由 Cloudflare 做），而是让"拿字典爆破密钥"变得不划算：
// 密钥空间是 10^40，但每秒 30 个请求的试探连猜中一个短密钥的边都碰不到。
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: map[string][]time.Time{}, limit: limit, window: window}
}

func (l *rateLimiter) allow(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	prev := l.hits[ip]
	kept := prev[:0]
	for _, t := range prev {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	return true
}
