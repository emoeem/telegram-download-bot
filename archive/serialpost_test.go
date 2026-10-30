package archive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// The chain in one test: an inbound message with media is numbered, then
// resolved back from that number into a ready-to-send post.
func TestLoadSerialPostFromNumber(t *testing.T) {
	s, root := newSerialArchive(t)
	_, serial, obj := seedCompleted(t, s, root, 4, "video", "video/mp4", []byte("0123456789"))
	if _, err := LinkSerialView(root, serial, "video", "video/mp4", obj); err != nil {
		t.Fatal(err)
	}
	p, ok, err := LoadSerialPost(context.Background(), s, root, serial)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if p.Text != "hello" {
		t.Fatalf("text=%q", p.Text)
	}
	if !p.HasFile || p.Path != SerialViewPath(root, serial, "video", "video/mp4") {
		t.Fatalf("path=%q hasFile=%v", p.Path, p.HasFile)
	}
	if p.KindLabel != "视频" {
		t.Fatalf("kindLabel=%q", p.KindLabel)
	}
	if p.SizeHuman == "" {
		t.Fatal("size not rendered")
	}
	if !strings.Contains(p.SourceURL, "chat_id=-1001") {
		t.Fatalf("source url=%q", p.SourceURL)
	}

	out, err := RenderSerialPost(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, SerialRef(serial)) {
		t.Fatalf("post does not lead with the number: %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("post lost the body: %q", out)
	}
	if !strings.Contains(out, p.FileName) {
		t.Fatalf("post lost the file name: %q", out)
	}
}

// A custom template sees the number and the material fields.
func TestRenderSerialPostCustomTemplate(t *testing.T) {
	s, root := newSerialArchive(t)
	_, serial, _ := seedCompleted(t, s, root, 2, "photo", "image/jpeg", []byte("x"))
	p, _, err := LoadSerialPost(context.Background(), s, root, serial)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderSerialPost(p, "{{.Ref}}|{{.Kind}}|{{.Text}}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, SerialRef(serial)) || !strings.Contains(out, "photo") || !strings.Contains(out, "hello") {
		t.Fatalf("rendered=%q", out)
	}
}

// A number with no downloaded bytes still resolves; the post just has no file.
func TestLoadSerialPostWithoutBytes(t *testing.T) {
	s, root := newSerialArchive(t)
	ctx := context.Background()
	m := store.Message{ChatID: -1, MessageID: 1, Date: time.Unix(1700000000, 0), HasMedia: true, Caption: "pending item"}
	if err := s.UpsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureMedia(ctx, m, store.MediaMeta{Kind: "photo", MIME: "image/jpeg", Size: 10}); err != nil {
		t.Fatal(err)
	}
	list, err := s.Serials(ctx, store.SerialFilter{Limit: 1})
	if err != nil || len(list) != 1 {
		t.Fatalf("serials=%v err=%v", list, err)
	}
	p, ok, err := LoadSerialPost(ctx, s, root, list[0].Serial)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if p.HasFile {
		t.Fatal("reported a file for a pending item")
	}
	out, err := RenderSerialPost(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pending item") {
		t.Fatalf("post=%q", out)
	}
}

func TestLoadSerialPostMissingNumber(t *testing.T) {
	s, root := newSerialArchive(t)
	if _, ok, err := LoadSerialPost(context.Background(), s, root, 9999); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}
