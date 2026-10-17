package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

func exportTestServer(t *testing.T) (*WebServer, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewWebServer(s), s
}

func seedExportMessages(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		if err := s.UpsertMessage(ctx, store.Message{
			ChatID: 77, MessageID: i, SenderID: 5,
			Date: base.Add(time.Duration(i) * time.Minute),
			Text: "message " + string(rune('0'+i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportMessagesCSV(t *testing.T) {
	w, s := exportTestServer(t)
	seedExportMessages(t, s)

	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export?kind=messages&format=csv", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("content-disposition=%q", cd)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type=%q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff on export")
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 4 { // header + 3 messages
		t.Fatalf("csv lines=%d: %q", len(lines), rec.Body.String())
	}
	if !strings.HasPrefix(lines[0], "row_id,chat_id,message_id") {
		t.Fatalf("header=%q", lines[0])
	}
	if !strings.Contains(rec.Body.String(), "message 2") {
		t.Fatal("message body missing from export")
	}
}

func TestExportMessagesJSON(t *testing.T) {
	w, s := exportTestServer(t)
	seedExportMessages(t, s)

	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export?kind=messages&format=json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var rows []map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0]["chat_id"] != "77" || rows[0]["message_id"] == "" || rows[0]["text"] == "" {
		t.Fatalf("row=%+v", rows[0])
	}
}

// Every exportable view must answer, including when the archive is empty.
func TestExportAllKindsOnEmptyArchive(t *testing.T) {
	w, _ := exportTestServer(t)
	for _, kind := range []string{"messages", "deleted", "edited", "links", "media"} {
		for _, format := range []string{"csv", "json"} {
			url := "/api/export?kind=" + kind + "&format=" + format + "&media_kind=image"
			rec := httptest.NewRecorder()
			w.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s", url, rec.Code, rec.Body.String())
			}
			body := strings.TrimSpace(rec.Body.String())
			if format == "json" && body != "[]" {
				t.Fatalf("%s: empty json body=%q", url, body)
			}
		}
	}
}

func TestExportRejectsUnknownKindAndNonGET(t *testing.T) {
	w, _ := exportTestServer(t)

	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export?kind=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind status=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export?kind=messages&format=xml", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown format status=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/export?kind=messages", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("POST must not be accepted on a read-only export endpoint")
	}
}

func TestPatchTargetUpdatesRules(t *testing.T) {
	w, s := exportTestServer(t)
	ctx := context.Background()
	if err := s.AddTarget(ctx, store.Target{ChatID: 99, Username: "chan", Enabled: true, ArchiveText: true, ArchiveMedia: true, DownloadMedia: true}); err != nil {
		t.Fatal(err)
	}
	// requireCtl only checks that a controller exists for management endpoints.
	w.Ctl = &Controller{Store: s}

	body := `{"chat_id":99,"keywords":["go"],"exclude_keywords":["ads"],"media_max_size":2048,"download_media":false,"enabled":false}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/targets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := s.Target(ctx, 99)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.DownloadMedia || got.MediaMaxSize != 2048 || len(got.Keywords) != 1 || len(got.ExcludeKeywords) != 1 {
		t.Fatalf("target after PATCH = %+v", got)
	}

	// An unknown chat is rejected instead of silently succeeding.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/api/targets", strings.NewReader(`{"chat_id":123456,"keywords":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	w.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown chat status=%d", rec.Code)
	}
}
