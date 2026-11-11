package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

func TestMaybeBackupCreatesAndPrunes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tgarchive.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	first, err := MaybeBackup(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("expected a snapshot to be created")
	}
	if fi, err := os.Stat(first); err != nil || fi.Size() == 0 {
		t.Fatalf("snapshot missing or empty: %v", err)
	}

	// A second call right away must be a no-op (fresh snapshot exists).
	second, err := MaybeBackup(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if second != "" {
		t.Fatalf("expected no new snapshot within the backup period, got %s", second)
	}

	// Back-date the snapshot beyond the period and seed stale snapshots beyond
	// the keep limit: a new one is created and the prune caps at backupKeep.
	past := time.Now().Add(-2 * backupPeriod)
	if err := os.Chtimes(first, past, past); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < backupKeep+2; i++ {
		stale := filepath.Join(filepath.Dir(first), backupPrefix+time.Now().Add(-time.Duration(i+1)*time.Hour).Format("20060102-150405")+".db")
		if err := os.WriteFile(stale, []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(stale, past, past); err != nil {
			t.Fatal(err)
		}
	}
	third, err := MaybeBackup(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if third == "" {
		t.Fatal("expected a new snapshot after the period elapsed")
	}
	entries, _ := filepath.Glob(filepath.Join(filepath.Dir(first), backupPrefix+"*.db"))
	if len(entries) > backupKeep {
		t.Fatalf("prune failed: %d snapshots remain", len(entries))
	}
}

func TestFreeDiskBytes(t *testing.T) {
	free, err := FreeDiskBytes(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Fatal("free bytes should be positive")
	}
}

func TestStorageEndpointCachesWithinTTL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tgarchive.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(NewWebServer(s).Handler())
	defer ts.Close()

	get := func() storageAPIResponse {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/storage")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st storageAPIResponse
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	first := get()
	// A file created after the first response must not appear until the cache
	// expires; otherwise every UI poll would walk the whole objects tree again.
	objDir := filepath.Join(dir, "objects", "aa", "bb")
	if err := os.MkdirAll(objDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objDir, "object"), make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}
	second := get()
	if second.ObjectsBytes != first.ObjectsBytes {
		t.Fatalf("storage response was not cached: first=%d second=%d",
			first.ObjectsBytes, second.ObjectsBytes)
	}
}

func TestStorageEndpoint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tgarchive.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(NewWebServer(s).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/storage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st storageAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.DBBytes <= 0 {
		t.Fatalf("db_bytes should be positive, got %d", st.DBBytes)
	}
	if st.MinFreeSpace != DefaultMinFreeSpace {
		t.Fatalf("min_free_space = %d", st.MinFreeSpace)
	}
}
