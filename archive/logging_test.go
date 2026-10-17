package archive

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyear/tdl/archive/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSlogJSONHandlerOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := NewStructuredLogger(&buf, LogFormatJSON, 0)
	logger.Info("test event", slog.String("job_id", "42"), slog.Int("status", 200))
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON log: %v; output=%q", err, buf.String())
	}
	if got["msg"] != "test event" || got["job_id"] != "42" || got["status"] != float64(200) {
		t.Fatalf("unexpected log fields: %#v", got)
	}
}

func TestSlogRedactsAuthToken(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	defer slog.SetDefault(old)
	slog.SetDefault(NewStructuredLogger(&buf, LogFormatJSON, 0))
	token := "super-secret-token"
	w := NewWebServer(openTestStore(t), WebAuthConfig{Token: token})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	w.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(buf.String(), token) {
		t.Fatalf("auth token leaked into logs: %s", buf.String())
	}
}
