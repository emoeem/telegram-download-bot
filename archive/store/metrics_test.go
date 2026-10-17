package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMetricsSnapshotEmpty(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.MetricsSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Jobs.Running != 0 || m.Jobs.Queued != 0 || m.Media.Pending != 0 || m.DB.MessagesTotal != 0 || m.DB.MediaTotal != 0 || m.DB.LinksTotal != 0 || m.DB.EventsTotal != 0 {
		t.Fatalf("unexpected empty metrics: %+v", m)
	}
}

func TestMetricsSnapshotWithData(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().Unix()
	_, err = s.DB().ExecContext(ctx, "INSERT INTO messages(chat_id,message_id,date,text,created_at,updated_at) VALUES(1,1,?,?,?,?)", now, "hello", now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().ExecContext(ctx, "INSERT INTO message_links(message_rowid,url,domain) VALUES(1,?,?)", "https://example.com", "example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().ExecContext(ctx, "INSERT INTO media(message_rowid,type,size,status,error_kind,created_at,updated_at) VALUES(1,?,?,?,?,?,?)", 1, 1234, "completed", "", now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().ExecContext(ctx, "INSERT INTO jobs(kind,status,created_at,updated_at) VALUES(?,?,?,?)", "backfill", "running", now, now)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.MetricsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Jobs.Running != 1 || m.Media.Completed != 1 || m.Media.TotalBytesDownloaded != 1234 || m.DB.MessagesTotal != 1 || m.DB.MediaTotal != 1 || m.DB.LinksTotal != 1 {
		t.Fatalf("unexpected populated metrics: %+v", m)
	}
}
