package archive

import (
	"io"
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iyear/tdl/archive/store"
)

type HTTPMetrics struct {
	requestsTotal atomic.Uint64
	requests4xx   atomic.Uint64
	requests5xx   atomic.Uint64
	durationMS    atomic.Uint64
}

func (m *HTTPMetrics) record(status int, duration time.Duration) {
	m.requestsTotal.Add(1)
	if status >= 400 && status < 500 {
		m.requests4xx.Add(1)
	}
	if status >= 500 {
		m.requests5xx.Add(1)
	}
	m.durationMS.Add(uint64(duration.Milliseconds()))
}

func (m *HTTPMetrics) snapshot() (total, r4xx, r5xx uint64, avgMS float64) {
	total = m.requestsTotal.Load()
	r4xx = m.requests4xx.Load()
	r5xx = m.requests5xx.Load()
	if total > 0 {
		avgMS = float64(m.durationMS.Load()) / float64(total)
	}
	return
}

type ProcessMetrics struct {
	startedAt time.Time
	dedupHits atomic.Uint64
}

var processMetrics = &ProcessMetrics{startedAt: time.Now()}

func recordDedupHit() {
	processMetrics.dedupHits.Add(1)
}

type metricsResponse struct {
	UptimeSeconds float64            `json:"uptime_seconds"`
	Jobs          store.JobMetrics   `json:"jobs"`
	Media         mediaMetricsJSON   `json:"media"`
	Errors        store.ErrorMetrics `json:"errors"`
	DB            store.DBMetrics    `json:"db"`
	HTTP          httpMetricsJSON    `json:"http"`
}

type mediaMetricsJSON struct {
	Pending              int64  `json:"pending"`
	Completed            int64  `json:"completed"`
	Failed               int64  `json:"failed"`
	Skipped              int64  `json:"skipped"`
	TotalBytesDownloaded int64  `json:"total_bytes_downloaded"`
	DedupHits            uint64 `json:"dedup_hits"`
}

type httpMetricsJSON struct {
	RequestsTotal uint64  `json:"requests_total"`
	Requests4xx   uint64  `json:"requests_4xx"`
	Requests5xx   uint64  `json:"requests_5xx"`
	AvgDurationMS float64 `json:"avg_duration_ms"`
}

// snapshotTTL is how long an aggregated snapshot is served from cache before
// one caller recomputes it.
const snapshotTTL = 5 * time.Second

type metricsCache struct {
	mu         sync.Mutex
	at         time.Time
	snapshot   store.Metrics
	refreshing bool
}

type serialCache struct {
	mu         sync.Mutex
	at         time.Time
	snapshot   store.SerialSummary
	refreshing bool
}

// serialSummary 走 5 秒缓存：/api/serials 是 UI 轮询页，而 SerialSummary 是
// media JOIN messages 的全表聚合——几十万行的库上每次轮询都现算会把
// SQLite 读锁占满（metricsSnapshot 同款模式的复制，TTL 语义一致）。
//
// 缓存未命中时不再持锁等聚合算完：那是全表扫描，把 mu 跨在它上面会让所有
// 并发调用方一起排队。改成立刻交出上一份快照，由第一个发现过期的调用方在
// 锁外刷新；刷新期间其他人拿旧值，刷新失败也保留旧值而不是把错误抛给 UI。
func (w *WebServer) serialSummary(ctx context.Context) (store.SerialSummary, error) {
	w.serialCache.mu.Lock()
	if time.Since(w.serialCache.at) < snapshotTTL {
		snap := w.serialCache.snapshot
		w.serialCache.mu.Unlock()
		return snap, nil
	}
	if w.serialCache.refreshing {
		stale := w.serialCache.snapshot
		w.serialCache.mu.Unlock()
		return stale, nil
	}
	w.serialCache.refreshing = true
	w.serialCache.mu.Unlock()

	snapshot, err := w.store().SerialSummary(ctx)

	w.serialCache.mu.Lock()
	w.serialCache.refreshing = false
	if err == nil {
		w.serialCache.snapshot = snapshot
		w.serialCache.at = time.Now()
	} else if !w.serialCache.at.IsZero() {
		snapshot, err = w.serialCache.snapshot, nil
	}
	w.serialCache.mu.Unlock()
	return snapshot, err
}

func (w *WebServer) metricsSnapshot(ctx context.Context) (store.Metrics, error) {
	w.metricsCache.mu.Lock()
	if time.Since(w.metricsCache.at) < snapshotTTL {
		snap := w.metricsCache.snapshot
		w.metricsCache.mu.Unlock()
		return snap, nil
	}
	if w.metricsCache.refreshing {
		stale := w.metricsCache.snapshot
		w.metricsCache.mu.Unlock()
		return stale, nil
	}
	w.metricsCache.refreshing = true
	w.metricsCache.mu.Unlock()

	snapshot, err := w.store().MetricsSnapshot(ctx)

	w.metricsCache.mu.Lock()
	w.metricsCache.refreshing = false
	if err == nil {
		w.metricsCache.snapshot = snapshot
		w.metricsCache.at = time.Now()
	} else if !w.metricsCache.at.IsZero() {
		snapshot, err = w.metricsCache.snapshot, nil
	}
	w.metricsCache.mu.Unlock()
	return snapshot, err
}

func (w *WebServer) metricsAPI(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	snapshot, err := w.metricsSnapshot(r.Context())
	if err != nil {
		w.serverError(rw, err)
		return
	}
	total, r4xx, r5xx, avgMS := w.httpMetrics.snapshot()
	writeJSON(rw, metricsResponse{
		UptimeSeconds: time.Since(processMetrics.startedAt).Seconds(),
		Jobs:          snapshot.Jobs,
		Media: mediaMetricsJSON{
			Pending: snapshot.Media.Pending, Completed: snapshot.Media.Completed,
			Failed: snapshot.Media.Failed, Skipped: snapshot.Media.Skipped,
			TotalBytesDownloaded: snapshot.Media.TotalBytesDownloaded,
			DedupHits:            processMetrics.dedupHits.Load(),
		},
		Errors: snapshot.Errors,
		DB:     snapshot.DB,
		HTTP: httpMetricsJSON{
			RequestsTotal: total, Requests4xx: r4xx, Requests5xx: r5xx, AvgDurationMS: avgMS,
		},
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// ReadFrom 让 http.ServeFile 的 sendfile 快速路径穿透记录器：
// 没有它，每个 /media/ 大视频（几 GB）都要经 32KB 用户态缓冲整段拷贝，
// 在手机上纯属 CPU/耗电浪费。状态仍按首写语义记录。
func (w *responseRecorder) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseRecorder) Flush() {
	w.status = http.StatusOK
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *WebServer) requestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rec := &responseRecorder{ResponseWriter: rw}
		start := time.Now()
		// Baseline headers for every response, including errors and media. The
		// login page and index add their own CSP on top.
		h := rec.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		logger := slog.Default().With(
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		)
		defer func() {
			if p := recover(); p != nil {
				// ErrAbortHandler is the documented way to drop a connection
				// mid-stream; net/http handles it silently, so re-panic.
				if p == http.ErrAbortHandler {
					panic(p)
				}
				logger.Error("http handler panicked",
					slog.Any("panic", p),
					slog.String("stack", string(debug.Stack())),
				)
				if rec.status == 0 {
					http.Error(rec, "internal server error", http.StatusInternalServerError)
				}
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			duration := time.Since(start)
			w.httpMetrics.record(status, duration)
			logger.Info("http request",
				slog.Int("status", status),
				slog.Int64("duration_ms", duration.Milliseconds()),
				slog.String("remote_addr", remoteAddr(r.RemoteAddr)),
			)
		}()
		next.ServeHTTP(rec, r)
	})
}

func remoteAddr(v string) string {
	host, _, err := net.SplitHostPort(v)
	if err == nil {
		return host
	}
	return v
}
