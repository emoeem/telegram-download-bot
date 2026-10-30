package store

import (
	"testing"

	"github.com/gotd/td/tg"
)

// webpageMessage builds a message whose media is a link preview, the shape
// Telegram sends for "here is a link and a still from it".
func webpageMessage(pageType, url string) *tg.Message {
	wp := &tg.WebPage{ID: 42, URL: url}
	if pageType != "" {
		wp.SetType(pageType)
	}
	return mediaMessage(&tg.MessageMediaWebPage{Webpage: wp})
}

func TestMediaInfoLinkVideo(t *testing.T) {
	// Telegram classifies the page itself: that verdict needs no host list.
	msg := webpageMessage("video", "https://example.org/watch/1")
	meta, ok := MediaInfo(msg)
	if !ok {
		t.Fatal("a page Telegram classified as video must be queued")
	}
	if meta.Kind != "link" {
		t.Fatalf("kind=%q, want link", meta.Kind)
	}
	if meta.URL != "https://example.org/watch/1" {
		t.Fatalf("url=%q", meta.URL)
	}
}

func TestMediaInfoLinkVideoByHost(t *testing.T) {
	// Pages Telegram files under "article" are still videos when the host is
	// one the external downloader knows.
	for _, url := range []string{
		"https://www.youtube.com/watch?v=abc",
		"https://youtu.be/abc",
		"https://m.bilibili.com/video/BV1xx",
		"https://x.com/user/status/1",
	} {
		meta, ok := MediaInfo(webpageMessage("article", url))
		if !ok {
			t.Fatalf("%s: must be queued as a link", url)
		}
		if meta.Kind != "link" {
			t.Fatalf("%s: kind=%q", url, meta.Kind)
		}
	}
}

func TestMediaInfoSkipsPlainArticle(t *testing.T) {
	// A news article has no video to fetch; queueing it would create a job that
	// fails extraction on every pass.
	for _, url := range []string{
		"https://example.com/news/2026/01",
		"https://blog.example.org/post",
		"",
	} {
		if _, ok := MediaInfo(webpageMessage("article", url)); ok {
			t.Fatalf("%q: a plain article must not be queued", url)
		}
	}
}

func TestMediaInfoWebpageEmbeddedDocument(t *testing.T) {
	// Telegram sometimes wraps a real video document in a webpage preview. The
	// bytes are on Telegram's side, so the extractor must not be involved.
	wp := &tg.WebPage{ID: 43, URL: "https://example.org/x"}
	wp.SetType("video")
	wp.SetDocument(&tg.Document{
		ID:       777,
		Size:     2048,
		MimeType: "video/mp4",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeVideo{W: 1280, H: 720, Duration: 30},
			&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
		},
	})
	meta, ok := MediaInfo(mediaMessage(&tg.MessageMediaWebPage{Webpage: wp}))
	if !ok {
		t.Fatal("an embedded video document must be queued")
	}
	if meta.Kind != "video" {
		t.Fatalf("kind=%q, want video (the document path, not the link path)", meta.Kind)
	}
	if meta.FileUniqueID != "tg-document-777" || meta.Name != "clip.mp4" {
		t.Fatalf("meta=%+v", meta)
	}
	if meta.URL != "" {
		t.Fatalf("an embedded document must not carry a link url, got %q", meta.URL)
	}
}

func TestIsVideoLink(t *testing.T) {
	cases := []struct {
		url, pageType string
		want          bool
	}{
		{"https://youtube.com/watch?v=1", "", true},
		{"https://WWW.YouTube.com/watch?v=1", "", true},
		{"https://music.youtube.com/watch?v=1", "", true},
		{"https://b23.tv/abc", "", true},
		{"https://example.com/a", "video", true},
		{"https://example.com/a", "gif", true},
		{"https://example.com/a", "article", false},
		{"ftp://youtube.com/watch", "", false},
		{"not a url", "", false},
		{"", "", false},
		// A lookalike domain must not match: suffix matching is on labels.
		{"https://notyoutube.com/watch", "", false},
		{"https://youtube.com.evil.tld/watch", "", false},
	}
	for _, c := range cases {
		if got := IsVideoLink(c.url, c.pageType); got != c.want {
			t.Fatalf("IsVideoLink(%q,%q)=%v want %v", c.url, c.pageType, got, c.want)
		}
	}
}

func TestFromTGSplitsCaption(t *testing.T) {
	// A media message keeps its words on the message body; the archive stores
	// them as Caption so "image with a caption" survives as a distinct shape.
	msg := docMessage(&tg.DocumentAttributeVideo{})
	msg.Message = "hello caption"
	got := FromTG(msg)
	if got.Caption != "hello caption" || got.Text != "" {
		t.Fatalf("media message: caption=%q text=%q", got.Caption, got.Text)
	}

	// A text-only message keeps them on Text.
	plain := &tg.Message{ID: 2, PeerID: &tg.PeerChannel{ChannelID: 7}, Message: "just text"}
	got = FromTG(plain)
	if got.Caption != "" || got.Text != "just text" {
		t.Fatalf("plain message: caption=%q text=%q", got.Caption, got.Text)
	}
}
