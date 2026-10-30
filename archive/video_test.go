package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"
)

// fakeTool writes a shell script that stands in for yt-dlp. Every branch is
// driven by its arguments so one script can impersonate a probe, a download and
// a rejected URL without touching the network.
func fakeTool(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVideoFetcherProbe(t *testing.T) {
	tool := fakeTool(t, `cat <<'JSON'
{"id":"abc","title":"A clip","duration":93.5,"width":1920,"height":1080,"ext":"mp4","thumbnail":"http://127.0.0.1:1/x.jpg","webpage_url":"https://example.org/v/abc"}
JSON`)
	f := &VideoFetcher{Binary: tool}
	info, err := f.Probe(context.Background(), "https://example.org/v/abc")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "A clip" || info.Width != 1920 || info.Height != 1080 || info.Duration != 93.5 {
		t.Fatalf("info=%+v", info)
	}
	if info.WebpageURL != "https://example.org/v/abc" {
		t.Fatalf("webpage url=%q", info.WebpageURL)
	}
}

func TestVideoFetcherDownloadReadsBackPath(t *testing.T) {
	// The download branch must trust the path yt-dlp prints, because it chooses
	// the extension. A hardcoded ".mp4" breaks on every site that serves webm.
	// The stand-in honours the -o template exactly like yt-dlp does, so this
	// also asserts that Download actually passes one.
	tool := fakeTool(t, `prev=""
tmpl=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then tmpl="$a"; fi
  prev="$a"
done
out=$(echo "$tmpl" | sed 's/%(ext)s/webm/')
: > "$out"
echo "$out"
echo "A clip"`)
	dir := t.TempDir()
	f := &VideoFetcher{Binary: tool}
	dl, err := f.Download(context.Background(), "https://example.org/v/abc", dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dl.Path) != dir {
		t.Fatalf("path=%q, want a file inside %q", dl.Path, dir)
	}
	if filepath.Ext(dl.Path) != ".webm" {
		t.Fatalf("path=%q, want the extension yt-dlp reported", dl.Path)
	}
	if dl.Title != "A clip" {
		t.Fatalf("title=%q", dl.Title)
	}
	if dl.MIME != "video/webm" {
		t.Fatalf("mime=%q", dl.MIME)
	}
}

func TestVideoFetcherClassifiesUnsupported(t *testing.T) {
	tool := fakeTool(t, `echo "ERROR: Unsupported URL: https://example.org/nope" >&2
exit 1`)
	f := &VideoFetcher{Binary: tool}
	_, err := f.Download(context.Background(), "https://example.org/nope", t.TempDir())
	if !errors.Is(err, ErrVideoUnsupported) {
		t.Fatalf("err=%v, want ErrVideoUnsupported", err)
	}
	// The message must survive as one line: it is stored in media.error_detail.
	if got := err.Error(); len(got) > 200 {
		t.Fatalf("classified error should be terse, got %d bytes", len(got))
	}
}

func TestVideoFetcherKeepsRetryableErrors(t *testing.T) {
	// A transient failure must not be classified as terminal, or the job is
	// parked forever instead of being retried.
	tool := fakeTool(t, `echo "ERROR: unable to download: connection reset" >&2
exit 1`)
	f := &VideoFetcher{Binary: tool}
	_, err := f.Download(context.Background(), "https://example.org/v", t.TempDir())
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrVideoUnsupported) || errors.Is(err, ErrVideoToolMissing) {
		t.Fatalf("a network error must stay retryable, got %v", err)
	}
}

func TestVideoFetcherMissingTool(t *testing.T) {
	f := &VideoFetcher{Binary: filepath.Join(t.TempDir(), "does-not-exist")}
	if f.Available() {
		t.Fatal("Available must be false for a missing binary")
	}
	if _, err := f.Probe(context.Background(), "https://x/y"); !errors.Is(err, ErrVideoToolMissing) {
		t.Fatalf("err=%v, want ErrVideoToolMissing", err)
	}
}

func TestVideoFetcherFormatSelectorAvoidsMergeWithoutFFmpeg(t *testing.T) {
	// Without ffmpeg, asking for separate streams produces a download that can
	// never be assembled; the selector must stay on one progressive file.
	noFF := &VideoFetcher{HasFFmpeg: false}
	if sel := noFF.formatSelector(); sel != "b[ext=mp4]/b" {
		t.Fatalf("no-ffmpeg selector=%q", sel)
	}
	withFF := &VideoFetcher{HasFFmpeg: true}
	if sel := withFF.formatSelector(); sel == noFF.formatSelector() {
		t.Fatalf("with ffmpeg the selector should allow merging, got %q", sel)
	}
}

func TestMimeForExt(t *testing.T) {
	cases := map[string]string{
		".mp4":  "video/mp4",
		".webm": "video/webm",
		".mkv":  "video/x-matroska",
		".m4a":  "audio/mp4",
		".xyz":  "application/octet-stream",
	}
	for ext, want := range cases {
		if got := mimeForExt(ext); got != want {
			t.Fatalf("mimeForExt(%q)=%q want %q", ext, got, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("a\nb\nc"); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := firstLine("no newline"); got != "no newline" {
		t.Fatalf("got %q", got)
	}
}

func TestThumbLocationPicksLargest(t *testing.T) {
	doc := &tg.Document{ID: 99, AccessHash: 7, FileReference: []byte{1, 2}}
	// SetThumbs must be used: GetThumbs gates on the TL flag, so assigning the
	// field directly reads back as "no thumbnails".
	doc.SetThumbs([]tg.PhotoSizeClass{
		&tg.PhotoSize{Type: "s", W: 90, H: 90},
		&tg.PhotoSize{Type: "m", W: 320, H: 240},
		&tg.PhotoSizeProgressive{Type: "x", W: 800, H: 600},
		// Stripped sizes are not images and must be ignored even when last.
		&tg.PhotoStrippedSize{Bytes: []byte{0x01}},
	})
	msg := &tg.Message{ID: 1, PeerID: &tg.PeerChannel{ChannelID: 2}}
	msg.SetMedia(&tg.MessageMediaDocument{Document: doc})

	loc, ok := thumbLocation(msg).(*tg.InputDocumentFileLocation)
	if !ok {
		t.Fatalf("no thumb location from %T", thumbLocation(msg))
	}
	if loc.ThumbSize != "x" {
		t.Fatalf("thumb size=%q, want the largest (x)", loc.ThumbSize)
	}
	if loc.ID != doc.ID || string(loc.FileReference) != string(doc.FileReference) {
		t.Fatalf("loc=%+v", loc)
	}
}

func TestThumbLocationNoThumbs(t *testing.T) {
	msg := &tg.Message{ID: 1, PeerID: &tg.PeerChannel{ChannelID: 2}}
	msg.SetMedia(&tg.MessageMediaDocument{Document: &tg.Document{ID: 5}})
	if loc := thumbLocation(msg); loc != nil {
		t.Fatalf("expected nil for a document without thumbs, got %T", loc)
	}
	// A photo needs no poster: its main file is the image.
	photo := &tg.Message{ID: 2, PeerID: &tg.PeerChannel{ChannelID: 2}}
	photo.SetMedia(&tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 3}})
	if loc := thumbLocation(photo); loc != nil {
		t.Fatalf("expected nil for a photo, got %T", loc)
	}
}

// TestVideoFetcherLiveProbe exercises the real binary against a real page.
//
// It is gated on an environment variable rather than on testing.Short: it needs
// the network and a working yt-dlp, which a CI box may have neither of, and a
// silent pass from a skipped test is worse than no test at all. Everything the
// unit tests above assert is still covered without it — this one exists to catch
// an upstream change in yt-dlp's output shape.
func TestVideoFetcherLiveProbe(t *testing.T) {
	if os.Getenv("TDL_LIVE_VIDEO") == "" {
		t.Skip("set TDL_LIVE_VIDEO=1 to probe a real URL through yt-dlp")
	}
	f := NewVideoFetcher()
	if !f.Available() {
		t.Skip("yt-dlp is not installed")
	}
	url := os.Getenv("TDL_LIVE_VIDEO_URL")
	if url == "" {
		url = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	}
	info, err := f.Probe(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == "" || info.Title == "" {
		t.Fatalf("probe returned an empty identity: %+v", info)
	}
	t.Logf("probed %s: %q (%dx%d, %.0fs)", info.ID, info.Title, info.Width, info.Height, info.Duration)
}
