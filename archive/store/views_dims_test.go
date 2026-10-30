package store

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
)

// The message projection is a second place dimensions can silently go missing.
// MediaInfo can extract them correctly and the row can hold them, yet the read
// path still hand back zeros — which is exactly what happened: media.width was
// populated while messageViewColumns never selected it, so the grid and the
// message list both got 0 and the layout kept jumping.
func TestMessageViewReportsMediaDimensions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	msg := mediaMessage(&tg.MessageMediaPhoto{Photo: &tg.Photo{
		ID: 31337,
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 100},
			&tg.PhotoSize{Type: "y", W: 1280, H: 720, Size: 200},
		},
	}})
	m := FromTG(msg)
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	meta, ok := MediaInfo(msg)
	if !ok {
		t.Fatal("expected downloadable media")
	}
	if err := s.EnsureMedia(ctx, m, meta); err != nil {
		t.Fatalf("EnsureMedia: %v", err)
	}
	rowID, err := s.RowID(ctx, m.ChatID, m.MessageID)
	if err != nil {
		t.Fatalf("RowID: %v", err)
	}

	views, err := s.MessageViewsByRowIDs(ctx, []int64{rowID})
	if err != nil {
		t.Fatalf("MessageViewsByRowIDs: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d views want 1", len(views))
	}
	if views[0].Width != 1280 || views[0].Height != 720 {
		t.Fatalf("view dims=%dx%d want 1280x720 (projection must carry them)", views[0].Width, views[0].Height)
	}
	// The scan order is positional, so a column added in the wrong place would
	// shift every field after it. Assert neighbours to catch that. MediaSHA256 is
	// empty here because the row is still pending — a freshly queued job has no
	// hash yet — so MediaStatus is the field that proves the scan landed.
	if views[0].MediaStatus != "pending" {
		t.Fatalf("MediaStatus=%q want pending: scan order drifted", views[0].MediaStatus)
	}
	if views[0].MessageID != m.MessageID {
		t.Fatalf("MessageID=%d want %d: scan order drifted", views[0].MessageID, m.MessageID)
	}
}

// A media row that legitimately has no dimensions must read back as 0 here, not
// as a shifted field. This is the neighbour case for the projection above.
func TestMessageViewWithoutMediaDimensions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	msg := mediaMessage(&tg.MessageMediaPhoto{Photo: &tg.Photo{
		ID:    5,
		Sizes: []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1}}},
	}})
	m := FromTG(msg)
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	meta, _ := MediaInfo(msg)
	if err := s.EnsureMedia(ctx, m, meta); err != nil {
		t.Fatalf("EnsureMedia: %v", err)
	}
	rowID, err := s.RowID(ctx, m.ChatID, m.MessageID)
	if err != nil {
		t.Fatalf("RowID: %v", err)
	}
	views, err := s.MessageViewsByRowIDs(ctx, []int64{rowID})
	if err != nil {
		t.Fatalf("MessageViewsByRowIDs: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d views want 1", len(views))
	}
	if views[0].Width != 0 || views[0].Height != 0 {
		t.Fatalf("view dims=%dx%d want 0x0", views[0].Width, views[0].Height)
	}
	if views[0].Deleted {
		t.Fatal("Deleted=true: the boolean after height shifted")
	}
}
