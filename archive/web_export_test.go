package archive

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestExportMaterialsView is the regression test for a broken SQL column name:
// the material view selected m.view_count/m.forward_count while the messages
// table stores views/forwards. Nothing exercised the view, so the typo survived
// until a real export ran against a real archive.
func TestExportMaterialsView(t *testing.T) {
	_, s := exportTestServer(t)
	seedExportMessages(t, s)
	ctx := context.Background()
	if _, err := NewMaterializer(s).Build(ctx, BuildOptions{}); err != nil {
		t.Fatal(err)
	}

	src, err := BuildExportSource(ctx, s, "materials", "", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := src.fetch(1)
	if err != nil {
		t.Fatalf("materials view must be queryable: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3", len(rows))
	}
	if len(rows[0]) != len(src.header) {
		t.Fatalf("row has %d values for %d columns", len(rows[0]), len(src.header))
	}
	// views/forwards must come from the messages row even when zero.
	idx := map[string]int{}
	for i, h := range src.header {
		idx[h] = i
	}
	if got := rows[0][idx["views"]]; got != "0" {
		t.Fatalf("views=%q, want 0", got)
	}
	if got := rows[0][idx["body"]]; !strings.Contains(got, "message") {
		t.Fatalf("body=%q", got)
	}
}

func TestExportFieldsQueryParam(t *testing.T) {
	w, s := exportTestServer(t)
	seedExportMessages(t, s)

	// fields= selects and renames; the response is CSV with the new header.
	req := httptest.NewRequest(http.MethodGet, "/api/export?kind=messages&format=csv&fields=message_id:msgId,text:content", nil)
	rw := httptest.NewRecorder()
	w.export(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rw.Code, rw.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(rw.Body.String()), "\n")
	if lines[0] != "msgId,content" {
		t.Fatalf("header=%q", lines[0])
	}
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want header + 3", len(lines))
	}
}

func TestExportUnknownFieldIsRejected(t *testing.T) {
	w, s := exportTestServer(t)
	seedExportMessages(t, s)
	req := httptest.NewRequest(http.MethodGet, "/api/export?kind=messages&fields=nope", nil)
	rw := httptest.NewRecorder()
	w.export(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for an unknown column", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "unknown field") {
		t.Fatalf("body=%q", rw.Body.String())
	}
}

func TestExportJSONLFormat(t *testing.T) {
	w, s := exportTestServer(t)
	seedExportMessages(t, s)
	req := httptest.NewRequest(http.MethodGet, "/api/export?kind=messages&format=jsonl", nil)
	rw := httptest.NewRecorder()
	w.export(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rw.Code, rw.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(rw.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	var obj map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &obj); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if _, ok := obj["row_id"]; !ok {
		t.Fatalf("obj=%v", obj)
	}
}

// Regression: the edited export indexed an empty rows slice and panicked on the
// first edited message. The empty-archive case was covered; a non-empty one was
// not.
func TestExportEditedWithRows(t *testing.T) {
	w, s := exportTestServer(t)
	ctx := context.Background()
	seedExportMessages(t, s)
	editDate := time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)
	edited := store.Message{
		ChatID: 77, MessageID: 2, SenderID: 5,
		Date: time.Date(2026, 10, 1, 12, 5, 0, 0, time.UTC),
		Text: "message 2 (edited)", EditDate: &editDate,
	}
	if err := s.RecordEdit(ctx, edited); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export?kind=edited&format=csv", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 { // header + 1 edited message
		t.Fatalf("csv lines=%d: %q", len(lines), rec.Body.String())
	}
	cols := strings.Split(lines[1], ",")
	if len(cols) != len(strings.Split(lines[0], ",")) {
		t.Fatalf("row width %d != header width %d", len(cols), len(strings.Split(lines[0], ",")))
	}
	if editCount := cols[len(cols)-1]; editCount != "1" {
		t.Fatalf("edit_count=%q, want 1", editCount)
	}
}

// Regression: the media export accepted q/chat/topic but silently dropped them,
// so "export this chat's videos" produced the whole archive.
func TestExportMediaRespectsChatFilter(t *testing.T) {
	w, s := exportTestServer(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i, chatID := range []int64{77, 88} {
		m := store.Message{
			ChatID: chatID, MessageID: 1, SenderID: 5,
			Date: base.Add(time.Duration(i) * time.Minute), HasMedia: true, Caption: "pic",
		}
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err := s.EnsureMedia(ctx, m, store.MediaMeta{Kind: "photo", Name: "p.jpg", MIME: "image/jpeg", Size: 10}); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := s.DB().QueryRow(`SELECT id FROM media WHERE message_rowid=(SELECT id FROM messages WHERE chat_id=? AND message_id=?)`, m.ChatID, m.MessageID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkMediaCompleted(ctx, id, fmt.Sprintf("sha-%d", chatID), 10, "image/jpeg", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/export?kind=media&format=csv&media_kind=image&chat=88", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 { // header + only chat 88
		t.Fatalf("csv lines=%d: %q", len(lines), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "88") {
		t.Fatal("expected the chat 88 row in export")
	}
}

// Regression: the serials walk reset its cursor to "unbounded" when a full page
// ended at serial 1, restarting from the newest row and looping to the row cap.
func TestExportSerialsWalkTerminates(t *testing.T) {
	_, s := exportTestServer(t)
	ctx := t.Context()
	base := time.Unix(1700000000, 0)
	for i := 1; i <= exportPageSize; i++ {
		m := store.Message{
			ChatID: -1001, MessageID: i,
			Date: base.Add(time.Duration(i) * time.Second), HasMedia: true,
		}
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err := s.EnsureMedia(ctx, m, store.MediaMeta{Kind: "document", Name: "f.bin", MIME: "application/octet-stream", Size: 10}); err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := s.DB().QueryRow(`SELECT id FROM media WHERE message_rowid=(SELECT id FROM messages WHERE chat_id=? AND message_id=?)`, m.ChatID, i).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkMediaCompleted(ctx, id, fmt.Sprintf("sha-%d", i), 10, "application/octet-stream", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	src, err := BuildExportSource(ctx, s, "serials", "", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := src.fetch(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != exportPageSize {
		t.Fatalf("page 1 rows=%d, want %d", len(rows), exportPageSize)
	}
	// The page ended at serial 1, so the walk must be over.
	rows, err = src.fetch(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("page 2 must be empty after the walk reaches serial 1, got %d rows", len(rows))
	}
}
