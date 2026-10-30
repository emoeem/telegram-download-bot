package archive

import (
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

type metricsCache struct {
	mu       sync.Mutex
	at       time.Time
	snapshot store.Metrics
}

func (w *WebServer) metricsSnapshot(ctx context.Context) (store.Metrics, error) {
	w.metricsCache.mu.Lock()
	defer w.metricsCache.mu.Unlock()
	if time.Since(w.metricsCache.at) < 5*time.Second {
		return w.metricsCache.snapshot, nil
	}
	snapshot, err := w.store().MetricsSnapshot(ctx)
	if err != nil {
		return store.Metrics{}, err
	}
	w.metricsCache.snapshot = snapshot
	w.metricsCache.at = time.Now()
	return snapshot, nil
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
