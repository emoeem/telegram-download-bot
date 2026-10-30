package archive

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A handler panic must become a 500 for the client, not a dropped connection,
// and the request must still be counted in the metrics.
func TestRequestMiddlewareRecoversPanics(t *testing.T) {
	w := &WebServer{}
	h := w.requestMiddleware(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

// A panic after the response started cannot be turned into a 500; the middleware
// must not try to write a second status line on top of the stream.
func TestRequestMiddlewarePanicAfterWriteKeepsStatus(t *testing.T) {
	w := &WebServer{}
	h := w.requestMiddleware(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusPartialContent)
		_, _ = rw.Write([]byte("partial"))
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/boom", nil))
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status=%d, want the already-sent 206", rec.Code)
	}
}
