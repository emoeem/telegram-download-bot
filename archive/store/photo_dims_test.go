package store

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
)

// A photo's dimensions live on its renditions, never on the payload itself.
// Before this was read, every photo archived with NULL width/height, which left
// the browse grid with no aspect ratio to reserve space for.
func TestMediaInfoPhotoReportsDimensions(t *testing.T) {
	msg := mediaMessage(&tg.MessageMediaPhoto{Photo: &tg.Photo{
		ID: 9,
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 100},
			&tg.PhotoSize{Type: "x", W: 800, H: 450, Size: 200},
			&tg.PhotoSizeProgressive{Type: "w", W: 1920, H: 1080, Sizes: []int{500, 900}},
		},
	}})
	meta, ok := MediaInfo(msg)
	if !ok {
		t.Fatal("expected downloadable media")
	}
	if meta.Kind != "photo" {
		t.Fatalf("kind=%q want photo", meta.Kind)
	}
	// The largest rendition, not the last one and not the first.
	if meta.Width != 1920 || meta.Height != 1080 {
		t.Fatalf("dims=%dx%d want 1920x1080", meta.Width, meta.Height)
	}
}

func TestLargestPhotoSize(t *testing.T) {
	cases := []struct {
		name  string
		sizes []tg.PhotoSizeClass
		w, h  int
	}{
		{
			name: "picks largest by area, not by order",
			// Telegram documents no ordering guarantee, so a larger rendition
			// listed first must still win.
			sizes: []tg.PhotoSizeClass{
				&tg.PhotoSizeProgressive{Type: "w", W: 2560, H: 1440},
				&tg.PhotoSize{Type: "m", W: 320, H: 180},
			},
			w: 2560, h: 1440,
		},
		{
			name: "area beats a taller-but-narrower rendition",
			sizes: []tg.PhotoSizeClass{
				&tg.PhotoSize{Type: "a", W: 400, H: 1200}, // 480k
				&tg.PhotoSize{Type: "b", W: 1280, H: 720}, // 921k
			},
			w: 1280, h: 720,
		},
		{
			// "i" entries are decodable placeholders, not renditions. A payload
			// with only these has no known size and must report 0, never a
			// fabricated 0x0 that reads as a real measurement.
			name:  "stripped only yields nothing",
			sizes: []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 2, 3}}},
			w:     0, h: 0,
		},
		{
			name:  "cached size counts",
			sizes: []tg.PhotoSizeClass{&tg.PhotoCachedSize{Type: "x", W: 640, H: 480}},
			w:     640, h: 480,
		},
		{
			name:  "degenerate rendition is ignored",
			sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "s", W: 0, H: 0}},
			w:     0, h: 0,
		},
		{
			name:  "no sizes at all",
			sizes: nil,
			w:     0, h: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := LargestPhotoSize(c.sizes)
			if w != c.w || h != c.h {
				t.Fatalf("LargestPhotoSize = %dx%d want %dx%d", w, h, c.w, c.h)
			}
		})
	}
}

// raw_json is what makes a local backfill possible instead of re-walking
// thousands of messages. This pins the exact shape it has to read.
func TestPhotoDimensionsFromRaw(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		w, h int
	}{
		{
			name: "largest rendition wins",
			raw: `{"Media":{"Photo":{"ID":1,"Sizes":[
				{"Type":"i","Bytes":"AAAA"},
				{"Type":"m","W":320,"H":180,"Size":24195},
				{"Type":"w","W":1920,"H":1080,"Sizes":[34116,98607]}]}}}`,
			w: 1920, h: 1080,
		},
		{
			name: "stripped-only payload has no dimensions",
			raw:  `{"Media":{"Photo":{"ID":1,"Sizes":[{"Type":"i","Bytes":"AAAA"}]}}}`,
			w:    0, h: 0,
		},
		{
			name: "document payload is not a photo",
			raw:  `{"Media":{"Document":{"ID":5,"MimeType":"image/jpeg"}}}`,
			w:    0, h: 0,
		},
		{name: "empty raw", raw: "", w: 0, h: 0},
		{name: "malformed raw", raw: "{not json", w: 0, h: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := photoDimensionsFromRaw(c.raw)
			if w != c.w || h != c.h {
				t.Fatalf("photoDimensionsFromRaw = %dx%d want %dx%d", w, h, c.w, c.h)
			}
		})
	}
}

// The backfill has to work against rows written by the buggy ingest, and the
// store runs on a single connection: reading and writing in one open-Rows loop
// would deadlock, so this exercises the real separation.
func TestBackfillPhotoDimensions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	photo := &tg.Photo{
		ID: 4242,
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 100},
			&tg.PhotoSize{Type: "y", W: 1280, H: 720, Size: 200},
		},
	}
	msg := mediaMessage(&tg.MessageMediaPhoto{Photo: photo})
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
	// Reproduce the historical bug: the row exists but carries no dimensions.
	if _, err := s.db.Exec(`UPDATE media SET width=NULL, height=NULL WHERE type='photo'`); err != nil {
		t.Fatalf("reset dims: %v", err)
	}

	missing, err := s.CountPhotosMissingDimensions(ctx)
	if err != nil {
		t.Fatalf("CountPhotosMissingDimensions: %v", err)
	}
	if missing != 1 {
		t.Fatalf("missing=%d want 1", missing)
	}

	filled, scanned, err := s.BackfillPhotoDimensions(ctx)
	if err != nil {
		t.Fatalf("BackfillPhotoDimensions: %v", err)
	}
	if scanned != 1 || filled != 1 {
		t.Fatalf("scanned=%d filled=%d want 1/1", scanned, filled)
	}

	var w, h int
	if err := s.db.QueryRow(`SELECT width, height FROM media WHERE type='photo'`).Scan(&w, &h); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if w != 1280 || h != 720 {
		t.Fatalf("backfilled %dx%d want 1280x720", w, h)
	}

	// Idempotence: a second run must find nothing left to do rather than
	// rewriting the same rows forever.
	againFilled, againScanned, err := s.BackfillPhotoDimensions(ctx)
	if err != nil {
		t.Fatalf("second BackfillPhotoDimensions: %v", err)
	}
	if againFilled != 0 || againScanned != 0 {
		t.Fatalf("second pass scanned=%d filled=%d, want 0/0", againScanned, againFilled)
	}
}

// A row whose payload genuinely has no rendition size must stay NULL. Writing 0
// would turn "unknown" into "measured 0x0" and the grid would divide by zero.
func TestBackfillPhotoDimensionsLeavesUnknownAlone(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	msg := mediaMessage(&tg.MessageMediaPhoto{Photo: &tg.Photo{
		ID:    7,
		Sizes: []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1}}},
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

	filled, scanned, err := s.BackfillPhotoDimensions(ctx)
	if err != nil {
		t.Fatalf("BackfillPhotoDimensions: %v", err)
	}
	if scanned != 1 || filled != 0 {
		t.Fatalf("scanned=%d filled=%d want 1/0", scanned, filled)
	}

	var w *int
	if err := s.db.QueryRow(`SELECT width FROM media WHERE type='photo'`).Scan(&w); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if w != nil {
		t.Fatalf("width=%d want NULL (unknown must not become 0)", *w)
	}
}
