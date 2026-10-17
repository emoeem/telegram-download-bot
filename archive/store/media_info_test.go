package store

import (
	"testing"

	"github.com/gotd/td/tg"
)

// mediaMessage builds a message whose conditional media field is properly set:
// GetMedia() reads the TL flag, not just the struct field.
func mediaMessage(media tg.MessageMediaClass) *tg.Message {
	m := &tg.Message{ID: 1, PeerID: &tg.PeerChannel{ChannelID: 7}}
	m.SetMedia(media)
	return m
}

func docMessage(attrs ...tg.DocumentAttributeClass) *tg.Message {
	return mediaMessage(&tg.MessageMediaDocument{
		Document: &tg.Document{ID: 555, MimeType: "application/octet-stream", Size: 4242, Attributes: attrs},
	})
}

func TestMediaInfoKindDetection(t *testing.T) {
	cases := []struct {
		name string
		msg  *tg.Message
		want string
	}{
		{"photo", mediaMessage(&tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 9}}), "photo"},
		{"video", docMessage(&tg.DocumentAttributeVideo{W: 1920, H: 1080, Duration: 12}), "video"},
		{"voice", docMessage(&tg.DocumentAttributeAudio{Voice: true, Duration: 3}), "voice"},
		{"audio", docMessage(&tg.DocumentAttributeAudio{Duration: 200}), "audio"},
		{"sticker", docMessage(&tg.DocumentAttributeSticker{}), "sticker"},
		{"animation", docMessage(&tg.DocumentAttributeAnimated{}), "animation"},
		{"document", docMessage(&tg.DocumentAttributeFilename{FileName: "report.pdf"}), "document"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta, ok := MediaInfo(c.msg)
			if !ok {
				t.Fatal("expected downloadable media")
			}
			if meta.Kind != c.want {
				t.Fatalf("kind=%q want %q", meta.Kind, c.want)
			}
		})
	}
}

func TestMediaInfoReportsSizeAndFilename(t *testing.T) {
	meta, ok := MediaInfo(docMessage(
		&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
		&tg.DocumentAttributeVideo{W: 640, H: 480, Duration: 5},
	))
	if !ok {
		t.Fatal("expected downloadable media")
	}
	if meta.Name != "clip.mp4" || meta.MIME != "application/octet-stream" || meta.Size != 4242 {
		t.Fatalf("meta=%+v", meta)
	}
	if meta.Width != 640 || meta.Height != 480 || meta.Duration != 5 {
		t.Fatalf("dims=%+v", meta)
	}
	if meta.FileUniqueID != "tg-document-555" {
		t.Fatalf("file unique id=%q", meta.FileUniqueID)
	}
}

// Media without a downloadable file must never be queued: the worker would fail
// forever on a job that can never succeed.
func TestMediaInfoSkipsNonDownloadableMedia(t *testing.T) {
	msgs := map[string]tg.MessageMediaClass{
		"geo":     &tg.MessageMediaGeo{},
		"poll":    &tg.MessageMediaPoll{},
		"contact": &tg.MessageMediaContact{},
		"webpage": &tg.MessageMediaWebPage{},
		"empty":   &tg.MessageMediaEmpty{},
	}
	for name, media := range msgs {
		if _, ok := MediaInfo(mediaMessage(media)); ok {
			t.Fatalf("%s: must not be queued as media", name)
		}
	}
	if _, ok := MediaInfo(&tg.Message{}); ok {
		t.Fatal("a message without media must not be queued")
	}
}
