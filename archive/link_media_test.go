package archive

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
)

func newArchiveStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A channel with previews disabled posts a bare URL. Telegram gives the message
// no media payload at all, so this is the only path that can queue the video.
func TestCollectorQueuesTextLinks(t *testing.T) {
	s := newArchiveStore(t)
	c := NewCollector(s)
	ctx := context.Background()
	chatID, msgID := int64(-1001234), 77

	if err := s.UpsertMessage(ctx, store.Message{ChatID: chatID, MessageID: msgID, Date: time.Unix(0, 0), Text: "看这个 https://youtu.be/aaa 还有 https://www.bilibili.com/video/BV1zz"}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	target := store.Target{Enabled: true, ArchiveMedia: true, DownloadMedia: true}
	m := store.Message{ChatID: chatID, MessageID: msgID}
	msg := &tg.Message{Message: "看这个 https://youtu.be/aaa 还有 https://www.bilibili.com/video/BV1zz"}

	if err := c.ensureLinkMedia(ctx, m, msg, target); err != nil {
		t.Fatalf("ensureLinkMedia: %v", err)
	}
	jobs, err := s.PendingMedia(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("queued %d link jobs, want 2", len(jobs))
	}
	for _, j := range jobs {
		if j.Type != "link" || j.URL == "" {
			t.Errorf("job type=%q url=%q, want a link with a URL", j.Type, j.URL)
		}
	}
}

// The media switches must gate this the same way they gate attached files,
// otherwise a target configured for text-only archiving silently starts
// pulling gigabytes of video.
func TestCollectorLinkQueueRespectsMediaSwitches(t *testing.T) {
	s := newArchiveStore(t)
	c := NewCollector(s)
	ctx := context.Background()
	chatID, msgID := int64(-1001234), 77

	if err := s.UpsertMessage(ctx, store.Message{ChatID: chatID, MessageID: msgID, Date: time.Unix(0, 0), Text: "https://youtu.be/aaa"}); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	m := store.Message{ChatID: chatID, MessageID: msgID}
	msg := &tg.Message{Message: "https://youtu.be/aaa"}

	for _, target := range []store.Target{
		{Enabled: true, ArchiveMedia: false, DownloadMedia: true},
		{Enabled: true, ArchiveMedia: true, DownloadMedia: false, ArchiveMode: "local"},
	} {
		if err := c.ensureLinkMedia(ctx, m, msg, target); err != nil {
			t.Fatalf("ensureLinkMedia: %v", err)
		}
	}
	jobs, err := s.PendingMedia(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("queued %d jobs despite media archiving being off", len(jobs))
	}
}
