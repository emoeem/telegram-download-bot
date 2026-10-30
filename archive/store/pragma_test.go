package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// The pragmas must be part of the DSN so every pooled connection gets them. The
// failure mode they guard is silent: a recycled connection without
// foreign_keys=1 leaves orphan rows after a purge instead of erroring.
func TestOpenAppliesConnectionPragmas(t *testing.T) {
	s := openSerialStore(t)
	var fk, busy int
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys=%d, want 1", fk)
	}
	if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy != 10000 {
		t.Fatalf("busy_timeout=%d, want 10000", busy)
	}
	var journal string
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode=%q, want wal", journal)
	}
}

// A cascade delete proves the pragma is active on the connection doing the
// write, not just on whichever connection ran the schema.
func TestForeignKeysCascade(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	if err := s.UpsertMessage(ctx, Message{ChatID: -100, MessageID: 1, Date: time.Unix(1700000000, 0), Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("INSERT INTO message_edits(message_rowid,version,text,edit_date,archived_at) SELECT id,1,'old',1,1 FROM messages"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("DELETE FROM messages"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "message_edits"); n != 0 {
		t.Fatalf("message_edits left=%d after the message was deleted", n)
	}
}

// Two processes share the database (the CLI and `serve`). busy_timeout is what
// makes the second writer wait for the lock instead of failing with
// "database is locked" the moment both are active.
func TestSecondWriterWaitsForLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	tx, err := first.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO archive_settings(key,value,updated_at) VALUES('lock-holder','1',1)"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := second.DB().Exec("INSERT INTO archive_settings(key,value,updated_at) VALUES('waiter','1',1)")
		done <- err
	}()

	select {
	case err := <-done:
		_ = tx.Rollback()
		t.Fatalf("second writer did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second writer failed after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second writer never completed after the lock was released")
	}
}

// The hot-path indexes must exist in the schema, because CREATE TABLE IF NOT
// EXISTS will not add them to an existing database.
func TestHotPathIndexesExist(t *testing.T) {
	s := openSerialStore(t)
	for _, name := range []string{
		"idx_autoclick_log_chat_msg",
		"idx_messages_chat_thread",
		"idx_messages_chat_reply_top",
		"idx_materials_date",
	} {
		var n int
		if err := s.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("index %s is missing", name)
		}
	}
}

// A no-op update must not delete and re-insert the FTS row: the upsert names
// fts_text on every edit and rescan.
func TestFTSUpdateTriggerChecksForChange(t *testing.T) {
	s := openSerialStore(t)
	var ddl string
	if err := s.DB().QueryRow("SELECT sql FROM sqlite_master WHERE type='trigger' AND name='messages_au'").Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "IS NOT new.fts_text") {
		t.Fatalf("trigger lacks the change guard: %s", ddl)
	}
}

// SearchAdvanced used to normalise the limit only inside buildSearchFilter,
// which works on a copy: Limit=0 became SQL "LIMIT 0" (always empty) and a
// negative value became "LIMIT -1" (unbounded, ignoring the 500 cap).
func TestSearchAdvancedNormalizesPaging(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		m := Message{ChatID: -100, MessageID: i, Date: time.Unix(1700000000+int64(i), 0), Text: "hello sqlite"}
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RefreshMetadata(ctx, Message{ChatID: -100, MessageID: 1}); err != nil {
		t.Fatal(err)
	}

	for _, limit := range []int{0, -1} {
		got, err := s.SearchAdvanced(ctx, "hello", SearchOptions{Limit: limit})
		if err != nil {
			t.Fatalf("limit=%d: %v", limit, err)
		}
		if len(got) == 0 {
			t.Fatalf("limit=%d returned no rows; paging was not normalised", limit)
		}
		if len(got) > 50 {
			t.Fatalf("limit=%d returned %d rows, want the default 50 cap", limit, len(got))
		}
	}
}

// Entities are written in one transaction and a re-delivery of the same payload
// updates nothing but still succeeds.
func TestUpsertEntitiesIdempotent(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	e := tg.Entities{
		Users: map[int64]*tg.User{
			1: {ID: 1, Username: "alice", FirstName: "Alice", AccessHash: 42},
			2: {ID: 2, Username: "bob", FirstName: "Bob", AccessHash: 43},
		},
		Channels: map[int64]*tg.Channel{
			100: {ID: 100, Username: "news", Title: "News", AccessHash: 44},
		},
	}
	for i := 0; i < 3; i++ {
		if err := s.UpsertEntities(ctx, e); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if n := countRows(t, s, "users"); n != 2 {
		t.Fatalf("users=%d", n)
	}
	if n := countRows(t, s, "chats"); n != 1 {
		t.Fatalf("chats=%d", n)
	}
	var username, title string
	if err := s.DB().QueryRow("SELECT COALESCE(username,''),COALESCE(title,'') FROM chats WHERE id=100").Scan(&username, &title); err != nil {
		t.Fatal(err)
	}
	if username != "news" || title != "News" {
		t.Fatalf("channel row username=%q title=%q", username, title)
	}
}
