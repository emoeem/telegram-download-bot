package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openSerialStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedMedia archives one message with one attached media item and returns the
// media row id. out=true marks it as sent by this account.
func seedMedia(t *testing.T, s *Store, chatID int64, msgID int, date int64, kind string, out bool) int64 {
	t.Helper()
	ctx := context.Background()
	m := Message{ChatID: chatID, MessageID: msgID, Date: time.Unix(date, 0), HasMedia: true, Text: "caption", Out: out}
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: kind, Name: kind + ".bin", MIME: "image/jpeg", Size: 1024}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.DB().QueryRow(`SELECT media.id FROM media JOIN messages ON messages.id=media.message_rowid
 WHERE messages.chat_id=? AND messages.message_id=?`, chatID, msgID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Every ingested media item gets a number, and the numbers only go up.
func TestEnsureMediaAssignsMonotonicSerial(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	var got []int64
	for i := 1; i <= 5; i++ {
		seedMedia(t, s, -100, i, int64(1700000000+i), "photo", false)
		list, err := s.Serials(ctx, SerialFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("msg %d: serials len=%d", i, len(list))
		}
		got = append(got, list[0].Serial)
	}
	for i, v := range got {
		if v != int64(i+1) {
			t.Fatalf("serial[%d]=%d want %d (all=%v)", i, v, i+1, got)
		}
	}
}

// Re-ingesting the same message (edit, rescan) must not renumber it.
func TestEnsureMediaKeepsSerialOnRepeat(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	id := seedMedia(t, s, -100, 7, 1700000000, "photo", false)
	first, err := s.MediaSerial(ctx, id)
	if err != nil || first == 0 {
		t.Fatalf("first serial=%d err=%v", first, err)
	}
	for i := 0; i < 3; i++ {
		m := Message{ChatID: -100, MessageID: 7, Date: time.Unix(1700000000, 0), HasMedia: true, Caption: fmt.Sprintf("edit %d", i)}
		if err := s.EnsureMedia(ctx, m, MediaMeta{Kind: "photo", MIME: "image/jpeg", Size: 2048}); err != nil {
			t.Fatal(err)
		}
	}
	again, err := s.MediaSerial(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("serial moved from %d to %d on re-ingest", first, again)
	}
	if sum, _ := s.SerialSummary(ctx); sum.Max != first {
		t.Fatalf("max serial=%d want %d", sum.Max, first)
	}
}

// The ledger separates inbound from outbound, and filters narrow it.
func TestSerialsInboundOnlyAndFilters(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	seedMedia(t, s, -100, 1, 1700000000, "photo", false)
	seedMedia(t, s, -100, 2, 1700000001, "video", true) // outbound
	seedMedia(t, s, -100, 3, 1700000002, "photo", false)

	inbound, err := s.Serials(ctx, SerialFilter{Limit: 10, InboundOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(inbound) != 2 {
		t.Fatalf("inbound=%d want 2", len(inbound))
	}
	for _, it := range inbound {
		if it.Out {
			t.Fatalf("inbound list contains outbound serial %d", it.Serial)
		}
	}
	videos, err := s.Serials(ctx, SerialFilter{Limit: 10, Kind: "video"})
	if err != nil || len(videos) != 1 || !videos[0].Out {
		t.Fatalf("video filter: %v err=%v", videos, err)
	}
	// Newest number first.
	all, _ := s.Serials(ctx, SerialFilter{Limit: 10})
	if len(all) != 3 || all[0].Serial != 3 || all[2].Serial != 1 {
		t.Fatalf("ordering wrong: %+v", all)
	}
	// Cursor form used by exports and range views.
	upTo, _ := s.Serials(ctx, SerialFilter{Limit: 10, MaxSerial: 2})
	if len(upTo) != 2 || upTo[0].Serial != 2 {
		t.Fatalf("max_serial filter wrong: %+v", upTo)
	}
}

// Legacy rows (serial NULL) are numbered oldest-message-first, not by row id.
func TestBackfillMediaSerialsOrdersByMessageDate(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	newest := seedMedia(t, s, -100, 1, 1700000300, "photo", false)
	middle := seedMedia(t, s, -100, 2, 1700000200, "photo", false)
	oldest := seedMedia(t, s, -100, 3, 1700000100, "photo", false)
	// Simulate a pre-serial database.
	if _, err := s.DB().Exec("UPDATE media SET serial=NULL"); err != nil {
		t.Fatal(err)
	}
	n, err := s.BackfillMediaSerials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("backfilled=%d want 3", n)
	}
	// Oldest message (id 3) is #1, newest (id 1) is #3.
	for id, want := range map[int64]int64{oldest: 1, middle: 2, newest: 3} {
		got, err := s.MediaSerial(ctx, id)
		if err != nil || got != want {
			t.Fatalf("media %d serial=%d want %d err=%v", id, got, want, err)
		}
	}
	// Idempotent: a second run numbers nothing.
	if n, err := s.BackfillMediaSerials(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill=%d err=%v", n, err)
	}
}

// A dry run reports exactly what would be deleted and touches nothing.
func TestPurgeDryRunTouchesNothing(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	id := seedMedia(t, s, -100, 9, 1700000000, "photo", false)
	sha := "aa" + fmt.Sprintf("%062d", 1)
	if _, err := s.DB().Exec("UPDATE media SET status='completed',sha256=?,original_path=? WHERE id=?", sha, "/tmp/obj/"+sha, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,100,'image/jpeg','ready',1,1)", sha); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Purge(ctx, PurgeSelector{Serial: 1, Messages: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Media != 1 || rep.Messages != 1 || len(rep.Files) != 1 {
		t.Fatalf("dry run report wrong: %+v", rep)
	}
	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM media").Scan(&n); err != nil || n != 1 {
		t.Fatalf("dry run deleted rows: media=%d err=%v", n, err)
	}
}

// Executing removes the row, releases the object reference and reports the file.
func TestPurgeRemovesRowsAndReportsFiles(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	id := seedMedia(t, s, -100, 9, 1700000000, "photo", false)
	sha := "bb" + fmt.Sprintf("%062d", 2)
	if _, err := s.DB().Exec("UPDATE media SET status='completed',sha256=?,original_path=? WHERE id=?", sha, "/tmp/obj/"+sha, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,100,'image/jpeg','ready',1,1)", sha); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Purge(ctx, PurgeSelector{Serial: 1, Messages: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Media != 1 || rep.Messages != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if len(rep.Files) != 1 || rep.Files[0] != "/tmp/obj/"+sha {
		t.Fatalf("files=%v", rep.Files)
	}
	var media, files int
	_ = s.DB().QueryRow("SELECT COUNT(*) FROM media").Scan(&media)
	_ = s.DB().QueryRow("SELECT COUNT(*) FROM files").Scan(&files)
	if media != 0 || files != 0 {
		t.Fatalf("after purge media=%d files=%d", media, files)
	}
	if sum, _ := s.SerialSummary(ctx); sum.Total != 0 {
		t.Fatalf("summary after purge=%+v", sum)
	}
}

// An empty selector is refused rather than interpreted as "everything".
func TestPurgeRefusesEmptySelector(t *testing.T) {
	s := openSerialStore(t)
	if _, err := s.Purge(context.Background(), PurgeSelector{Messages: true}); err == nil {
		t.Fatal("empty selector was accepted")
	}
}

// Hard-deleting one message must not unlink an object another message still
// references through content dedupe. The bug was that PurgeMediaRows reported
// every path it saw, without consulting files.ref_count.
func TestDeleteMessageHardKeepsSharedObject(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	first := seedMedia(t, s, -100, 1, 1700000000, "photo", false)
	second := seedMedia(t, s, -100, 2, 1700000001, "photo", false)
	sha := "cc" + fmt.Sprintf("%062d", 3)
	obj := "/tmp/obj/" + sha
	for _, id := range []int64{first, second} {
		if _, err := s.DB().Exec("UPDATE media SET status='completed',sha256=?,original_path=? WHERE id=?", sha, obj, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec("INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,100,'image/jpeg','ready',2,1)", sha); err != nil {
		t.Fatal(err)
	}
	rowID := func(msgID int) int64 {
		var id int64
		if err := s.DB().QueryRow("SELECT id FROM messages WHERE chat_id=? AND message_id=?", int64(-100), msgID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	rep, err := s.DeleteMessageHard(ctx, rowID(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 0 {
		t.Fatalf("deleting one of two references reported the file: %v", rep.Files)
	}
	var left int
	if err := s.DB().QueryRow("SELECT ref_count FROM files WHERE sha256=?", sha).Scan(&left); err != nil {
		t.Fatalf("files row must survive while referenced: %v", err)
	}
	if left != 1 {
		t.Fatalf("ref_count=%d want 1", left)
	}

	// The last reference going away reports the file exactly once.
	rep, err = s.DeleteMessageHard(ctx, rowID(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 1 || rep.Files[0] != obj {
		t.Fatalf("files=%v want [%s]", rep.Files, obj)
	}
	if n := countRows(t, s, "files"); n != 0 {
		t.Fatalf("files rows left=%d", n)
	}
}

// PurgeMediaRows deletes by id, so a media row that never got a serial is still
// removable, and it applies the same shared-object rule.
func TestPurgeMediaRowsKeepsSharedObject(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	first := seedMedia(t, s, -100, 1, 1700000000, "photo", false)
	second := seedMedia(t, s, -100, 2, 1700000001, "photo", false)
	sha := "dd" + fmt.Sprintf("%062d", 4)
	for _, id := range []int64{first, second} {
		if _, err := s.DB().Exec("UPDATE media SET status='completed',sha256=?,original_path=? WHERE id=?", sha, "/tmp/obj/"+sha, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec("INSERT INTO files(sha256,size,mime_type,state,ref_count,created_at) VALUES(?,100,'image/jpeg','ready',2,1)", sha); err != nil {
		t.Fatal(err)
	}
	// A row with no serial must still be purgeable by id.
	if _, err := s.DB().Exec("UPDATE media SET serial=NULL WHERE id=?", second); err != nil {
		t.Fatal(err)
	}

	rep, err := s.PurgeMediaRows(ctx, []int64{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Media != 2 {
		t.Fatalf("media=%d want 2", rep.Media)
	}
	if len(rep.Files) != 1 {
		t.Fatalf("files=%v want the object once both references are gone", rep.Files)
	}
	if n := countRows(t, s, "files"); n != 0 {
		t.Fatalf("files rows left=%d", n)
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --keep-messages drops media but leaves the message row.
func TestPurgeKeepMessages(t *testing.T) {
	s := openSerialStore(t)
	ctx := context.Background()
	seedMedia(t, s, -100, 5, 1700000000, "photo", false)
	rep, err := s.Purge(ctx, PurgeSelector{Serial: 1, Messages: false})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Media != 1 || rep.Messages != 0 {
		t.Fatalf("report=%+v", rep)
	}
	var msgs int
	if err := s.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&msgs); err != nil || msgs != 1 {
		t.Fatalf("messages=%d err=%v", msgs, err)
	}
}
