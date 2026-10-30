package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyear/tdl/archive/store"
)

// newAdminWeb builds a managed viewer whose data root is a temp dir, so a hard
// delete in a test can never reach the real archive.
func newAdminWeb(t *testing.T, managed bool) (*store.Store, *httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewWebServer(s)
	if managed {
		w.Ctl = &Controller{jobs: map[int64]*Job{}}
	}
	w.Root = root
	ts := httptest.NewServer(w.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return s, ts, root
}

func rowIDFor(t *testing.T, s *store.Store, msgID int) int64 {
	t.Helper()
	var rowID int64
	if err := s.DB().QueryRow(`SELECT id FROM messages WHERE chat_id=? AND message_id=?`, int64(-1001), msgID).Scan(&rowID); err != nil {
		t.Fatalf("row id for message %d: %v", msgID, err)
	}
	return rowID
}

func TestMessageDeleteSoftThenRestore(t *testing.T) {
	s, ts, root := newAdminWeb(t, true)
	_, _, _ = seedCompleted(t, s, root, 7, "photo", "image/jpeg", []byte("soft"))
	rowID := rowIDFor(t, s, 7)

	resp := postJSON(t, ts.URL+"/api/message/delete", map[string]any{"rowid": rowID, "mode": "soft"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("soft delete status %d", resp.StatusCode)
	}
	resp.Body.Close()

	var deleted int
	if err := s.DB().QueryRow(`SELECT deleted FROM messages WHERE id=?`, rowID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("message should be soft deleted, deleted=%d", deleted)
	}
	// The bytes must survive a soft delete: that is the whole point of the mode.
	var objectPath string
	if err := s.DB().QueryRow(`SELECT original_path FROM media WHERE message_rowid=?`, rowID).Scan(&objectPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(objectPath); err != nil {
		t.Fatalf("soft delete must keep the file: %v", err)
	}

	resp = postJSON(t, ts.URL+"/api/message/delete", map[string]any{"rowid": rowID, "mode": "restore"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore status %d", resp.StatusCode)
	}
	resp.Body.Close()
	if err := s.DB().QueryRow(`SELECT deleted FROM messages WHERE id=?`, rowID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("restore should clear the flag, deleted=%d", deleted)
	}
}

func TestMessageDeleteHardRemovesRowAndObject(t *testing.T) {
	s, ts, root := newAdminWeb(t, true)
	_, _, obj := seedCompleted(t, s, root, 9, "photo", "image/jpeg", []byte("hard"))
	rowID := rowIDFor(t, s, 9)
	if _, err := os.Stat(obj); err != nil {
		t.Fatalf("seed should have written the object: %v", err)
	}

	resp := postJSON(t, ts.URL+"/api/message/delete", map[string]any{"rowid": rowID, "mode": "hard"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hard delete status %d", resp.StatusCode)
	}
	var out struct {
		Status       string `json:"status"`
		FilesRemoved int    `json:"files_removed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE id=?`, rowID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("message row should be gone, found %d", n)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM media WHERE message_rowid=?`, rowID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("media row should be gone, found %d", n)
	}
	if _, err := os.Stat(obj); !os.IsNotExist(err) {
		t.Fatalf("object file should be unlinked, stat err=%v", err)
	}
	if out.FilesRemoved != 1 {
		t.Fatalf("report should claim one removed file, got %d", out.FilesRemoved)
	}
}

// A text-only message never matches Purge's media join. Without the explicit
// row delete the caller would see "deleted" and still find the row.
func TestMessageDeleteHardTextOnly(t *testing.T) {
	s, ts, _ := newAdminWeb(t, true)
	ctx := context.Background()
	m := store.Message{ChatID: -1001, MessageID: 11, Text: "no media here"}
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	rowID := rowIDFor(t, s, 11)

	resp := postJSON(t, ts.URL+"/api/message/delete", map[string]any{"rowid": rowID, "mode": "hard"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hard delete status %d", resp.StatusCode)
	}
	resp.Body.Close()

	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE id=?`, rowID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("text-only message row should be gone, found %d", n)
	}
}

func TestMessageDeleteRejectsNonJSONAndUnmanaged(t *testing.T) {
	_, ts, root := newAdminWeb(t, true)
	_ = root
	// text/plain is the content type a cross-site form can send without a
	// preflight, so it must be refused before the body is read.
	resp, err := http.Post(ts.URL+"/api/message/delete", "text/plain", strings.NewReader(`{"rowid":1,"mode":"hard"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain should be 415, got %d", resp.StatusCode)
	}

	_, ts2, _ := newAdminWeb(t, false)
	resp = postJSON(t, ts2.URL+"/api/message/delete", map[string]any{"rowid": 1, "mode": "soft"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unmanaged server should be 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMessageDeleteRejectsBadMode(t *testing.T) {
	s, ts, root := newAdminWeb(t, true)
	_, _, _ = seedCompleted(t, s, root, 13, "photo", "image/jpeg", []byte("x"))
	rowID := rowIDFor(t, s, 13)
	resp := postJSON(t, ts.URL+"/api/message/delete", map[string]any{"rowid": rowID, "mode": "nuke"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown mode should be 400, got %d", resp.StatusCode)
	}
}
