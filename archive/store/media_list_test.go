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
