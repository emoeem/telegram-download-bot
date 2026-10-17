package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreIdempotentSearchAndDelete(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	m := Message{ChatID: 123, MessageID: 7, Date: time.Unix(1700000000, 0), Text: "这是一个 Telegram 中文归档测试"}
	if err = s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	n, _, _, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("messages=%d", n)
	}
	m.Entities = []Entity{{Type: "url", Offset: 0, Length: 4}}
	m.Text += " https://example.com/a"
	if err = s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err = s.RefreshMetadata(ctx, m); err != nil {
		t.Fatal(err)
	}
	var links, entities int
	if err = s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM message_links").Scan(&links); err != nil {
		t.Fatal(err)
	}
	if err = s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM message_entities").Scan(&entities); err != nil {
		t.Fatal(err)
	}
	if links != 1 || entities != 1 {
		t.Fatalf("metadata links=%d entities=%d", links, entities)
	}
	rs, err := s.Search(ctx, "中文归档", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].MessageID != 7 {
		t.Fatalf("search=%+v", rs)
	}
	if err = s.SoftDelete(ctx, 123, []int{7}); err != nil {
		t.Fatal(err)
	}
	first, err := s.FirstMessageID(ctx, 123)
	if err != nil {
		t.Fatal(err)
	}
	if first != 0 {
		t.Fatalf("first after delete=%d", first)
	}
}
func TestTargetRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().Add(-time.Hour)
	want := Target{ChatID: 9, Username: "example", Enabled: true, ArchiveText: true, ArchiveMedia: true, ArchiveFiles: true, DownloadMedia: true, Keywords: []string{"foo"}, ExcludeKeywords: []string{"bar"}, Since: &now}
	if err = s.AddTarget(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Target(ctx, 9)
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "example" || !got.DownloadMedia || len(got.Keywords) != 1 || got.Since == nil {
		t.Fatalf("target=%+v", got)
	}
}

func TestTargetDestinationModesAndMultipleDestinations(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	d1, err := s.AddDestination(ctx, "archive-a", "telegram", ptrInt64(1001))
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.AddDestination(ctx, "archive-b", "telegram", ptrInt64(1002))
	if err != nil {
		t.Fatal(err)
	}
	want := Target{ChatID: 99, Username: "source", Enabled: true, ArchiveText: true, ArchiveMedia: true, ArchiveFiles: true, ArchiveMode: "both", DestinationID: sql.NullInt64{Int64: d1, Valid: true}}
	if err = s.AddTarget(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err = s.AttachDestination(ctx, 99, d2); err != nil {
		t.Fatal(err)
	}
	got, err := s.TargetDestinations(ctx, 99)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("destinations=%d", len(got))
	}
	gotTarget, err := s.Target(ctx, 99)
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget.ArchiveMode != "both" || !gotTarget.DestinationID.Valid {
		t.Fatalf("target=%+v", gotTarget)
	}
}
func ptrInt64(v int64) *int64 { return &v }

func TestAdvancedSearchSyntax(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	_, err = s.DB().ExecContext(ctx, "INSERT INTO users(id,username,created_at,updated_at) VALUES(42,'alice',1,1)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().ExecContext(ctx, "INSERT INTO chats(id,type,username,title,created_at,updated_at) VALUES(123,'channel','source','Source',1,1)")
	if err != nil {
		t.Fatal(err)
	}
	m := Message{ChatID: 123, MessageID: 8, SenderID: 42, Date: time.Unix(1700000000, 0), Text: "NVIDIA archive https://example.com"}
	if err = s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err = s.RefreshMetadata(ctx, m); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Search(ctx, "NVIDIA from:@alice in:@source has:link after:2023-01-01 before:2030-01-01", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].MessageID != 8 {
		t.Fatalf("results=%+v", rs)
	}
}
