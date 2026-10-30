package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func newMediaTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if err := s.SetChat(ctx, 100, "Media Source", "source", "channel"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO users(id,username,first_name,created_at,updated_at) VALUES(42,'alice','Alice',1,1)"); err != nil {
		t.Fatal(err)
	}
	return s
}

func insertMediaTestMessage(t *testing.T, s *Store, id int, sender int64, date time.Time, mime, mediaType, status string) {
	t.Helper()
	ctx := context.Background()
	m := Message{ChatID: 100, MessageID: id, SenderID: sender, Date: date, Text: fmt.Sprintf("media %d", id), HasMedia: true}
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	rowID, err := s.RowID(ctx, 100, id)
	if err != nil {
		t.Fatal(err)
	}
	sha := fmt.Sprintf("%064x", id)
	if _, err := s.DB().ExecContext(ctx, "INSERT INTO media(message_rowid,type,mime_type,size,width,height,sha256,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)", rowID, mediaType, mime, int64(1024), 640, 480, sha, status, date.Unix(), date.Unix()); err != nil {
		t.Fatal(err)
	}
}

func TestListMediaMessagesImagesOnly(t *testing.T) {
	s := newMediaTestStore(t)
	base := time.Unix(1700000000, 0)
	insertMediaTestMessage(t, s, 1, 42, base, "image/jpeg", "photo", "completed")
	insertMediaTestMessage(t, s, 2, 42, base.Add(time.Minute), "video/mp4", "video", "completed")
	insertMediaTestMessage(t, s, 3, 42, base.Add(2*time.Minute), "application/pdf", "document", "completed")
	page, err := s.ListMediaMessages(context.Background(), MediaFilter{Kind: MediaKindImage, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].MediaMime != "image/jpeg" {
		t.Fatalf("images=%+v total=%d", page.Items, page.Total)
	}
}

func TestListMediaMessagesVideosOnly(t *testing.T) {
	s := newMediaTestStore(t)
	base := time.Unix(1700000000, 0)
	insertMediaTestMessage(t, s, 1, 42, base, "image/jpeg", "photo", "completed")
	insertMediaTestMessage(t, s, 2, 42, base.Add(time.Minute), "video/mp4", "video", "completed")
	page, err := s.ListMediaMessages(context.Background(), MediaFilter{Kind: MediaKindVideo, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Items[0].MediaMime != "video/mp4" {
		t.Fatalf("videos=%+v total=%d", page.Items, page.Total)
	}
}

func TestListMediaMessagesFilesOnly(t *testing.T) {
	s := newMediaTestStore(t)
	base := time.Unix(1700000000, 0)
	insertMediaTestMessage(t, s, 1, 42, base, "image/jpeg", "photo", "completed")
	insertMediaTestMessage(t, s, 2, 42, base.Add(time.Minute), "video/mp4", "video", "completed")
	insertMediaTestMessage(t, s, 3, 42, base.Add(2*time.Minute), "application/zip", "document", "completed")
	page, err := s.ListMediaMessages(context.Background(), MediaFilter{Kind: MediaKindFile, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Items[0].MediaMime != "application/zip" {
		t.Fatalf("files=%+v total=%d", page.Items, page.Total)
	}
}

func TestListMediaMessagesPagination(t *testing.T) {
	s := newMediaTestStore(t)
	base := time.Unix(1700000000, 0)
	for i := 1; i <= 120; i++ {
		insertMediaTestMessage(t, s, i, 42, base.Add(time.Duration(i)*time.Minute), "image/jpeg", "photo", "completed")
	}
	for pageNo, want := range map[int]int{1: 50, 2: 50, 3: 20} {
		page, err := s.ListMediaMessages(context.Background(), MediaFilter{Kind: MediaKindImage, Page: pageNo, PageSize: 50})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 120 || len(page.Items) != want {
			t.Fatalf("page=%d total=%d items=%d want=%d", pageNo, page.Total, len(page.Items), want)
		}
	}
}

func TestListMediaMessagesWithSearchSyntax(t *testing.T) {
	s := newMediaTestStore(t)
	base := time.Unix(1700000000, 0)
	insertMediaTestMessage(t, s, 1, 42, base.AddDate(0, 0, -2), "image/jpeg", "photo", "completed")
	insertMediaTestMessage(t, s, 2, 42, base, "image/jpeg", "photo", "completed")
	insertMediaTestMessage(t, s, 3, 42, base.AddDate(0, 0, 2), "image/jpeg", "photo", "completed")
	page, err := s.ListMediaMessages(context.Background(), MediaFilter{
		Kind: MediaKindImage, Query: "from:@alice after:2023-11-14 before:2023-11-15", PageSize: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].MessageID != 2 {
		t.Fatalf("search result=%+v total=%d", page.Items, page.Total)
	}
}

// TestListMediaDedupedCollapsesReposts covers the gallery reading "301 videos"
// when the library holds 190 files: a reward bot reposts the same clip on every
// /start, so one frame appeared thirteen times in a row.
func TestListMediaDedupedCollapsesReposts(t *testing.T) {
	s := newMediaTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0)

	post := func(msgID int, sha, text string) {
		t.Helper()
		m := Message{ChatID: 100, MessageID: msgID, Date: base.Add(time.Duration(msgID) * time.Second), Text: text, HasMedia: true}
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		rowID, err := s.RowID(ctx, 100, msgID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().ExecContext(ctx,
			"INSERT INTO media(message_rowid,type,mime_type,size,width,height,sha256,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
			rowID, "video", "video/mp4", int64(1024), 640, 480, sha, "completed", base.Unix(), base.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	post(1, "sha-same", "编号 SW1561 一键查询")
	post(2, "sha-same", "")
	post(3, "sha-same", "")
	post(4, "sha-other", "另一段")

	page, err := s.ListMediaMessages(ctx, MediaFilter{Kind: MediaKindVideo, Dedupe: true, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("total = %d, want 2 distinct files", page.Total)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(page.Items))
	}
	got := map[int64]int{}
	for _, it := range page.Items {
		got[it.RowID] = it.DupCount
	}
	// The representative must be the OLDEST posting: that is the one carrying the
	// caption, and the only one a reader can identify.
	if n, ok := got[1]; !ok || n != 3 {
		t.Fatalf("oldest row must represent the group with dup_count=3, got %+v", got)
	}
	// The raw view keeps every posting, so an operator can still audit reposts.
	raw, err := s.ListMediaMessages(ctx, MediaFilter{Kind: MediaKindVideo, Dedupe: false, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if raw.Total != 4 || len(raw.Items) != 4 {
		t.Fatalf("raw total=%d items=%d, want 4 and 4", raw.Total, len(raw.Items))
	}
}
