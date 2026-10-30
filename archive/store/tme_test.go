package store

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestParseTMeMessage(t *testing.T) {
	cases := []struct {
		in   string
		want TMeRef
		ok   bool
	}{
		// Accepted: every spelling a channel author or the app produces.
		{"t.me/chan/1152", TMeRef{Username: "chan", MessageID: 1152}, true},
		{"https://t.me/chan/1152", TMeRef{Username: "chan", MessageID: 1152}, true},
		{"http://t.me/chan/1152/", TMeRef{Username: "chan", MessageID: 1152}, true},
		{"https://t.me/s/chan/1152", TMeRef{Username: "chan", MessageID: 1152}, true},
		{"telegram.me/chan/1152", TMeRef{Username: "chan", MessageID: 1152}, true},
		{"https://t.me/chan/1152?single", TMeRef{Username: "chan", MessageID: 1152}, true},
		{"https://t.me/c/3748897192/55", TMeRef{ChannelID: 3748897192, MessageID: 55}, true},
		{"t.me/c/3748897192/55", TMeRef{ChannelID: 3748897192, MessageID: 55}, true},

		// Rejected: not a single post of a chat.
		{"t.me/chan", TMeRef{}, false},
		{"t.me/s/chan", TMeRef{}, false},
		{"t.me/+AbCdEfGh", TMeRef{}, false},
		{"t.me/joinchat/AbCdEfGh", TMeRef{}, false},
		{"t.me/somebot?start=payload", TMeRef{}, false},
		{"t.me/addstickers/Pack", TMeRef{}, false},
		{"t.me/share/url?url=x", TMeRef{}, false},
		{"t.me/chan/0", TMeRef{}, false},
		{"t.me/chan/notanumber", TMeRef{}, false},
		{"t.me/c/abc/55", TMeRef{}, false},
		{"t.me/c/3748897192", TMeRef{}, false},
		{"https://example.com/a/1", TMeRef{}, false},
		{"", TMeRef{}, false},
		// A host that merely ends in "t.me" must not pass as one.
		{"https://nott.me/chan/1", TMeRef{}, false},
	}
	for _, c := range cases {
		got, ok := ParseTMeMessage(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseTMeMessage(%q) = %+v,%v want %+v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTMeURLCanonical(t *testing.T) {
	cases := []struct{ in, want string }{
		{"t.me/chan/1", "https://t.me/chan/1"},
		{"http://t.me/chan/1/", "https://t.me/chan/1"},
		{"https://t.me/s/chan/1", "https://t.me/chan/1"},
		{"t.me/c/3748897192/55", "https://t.me/c/3748897192/55"},
	}
	for _, c := range cases {
		ref, ok := ParseTMeMessage(c.in)
		if !ok {
			t.Fatalf("ParseTMeMessage(%q) failed", c.in)
		}
		if got := ref.TMeURL(); got != c.want {
			t.Errorf("%q canonical = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLinkMediasTMe pins the behaviour that made this kind necessary: a post
// whose words quote another chat's message must produce a queue entry, while a
// bare channel link must not (following one would silently ingest a whole
// channel, which is far more than the sentence asked for).
func TestLinkMediasTMe(t *testing.T) {
	m := &tg.Message{Message: "看这个 t.me/other/77 和 https://t.me/s/other/77/ 还有 https://t.me/other"}
	got := LinkMedias(m)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].Kind != KindTMe || got[0].URL != "https://t.me/other/77" {
		t.Errorf("entry = %+v, want kind %q url https://t.me/other/77", got[0], KindTMe)
	}
}

// TestExtractLinksBareTMe guards the extractor: the bare spelling has no "://"
// and was invisible before, which is exactly the case a channel uses.
func TestExtractLinksBareTMe(t *testing.T) {
	got := ExtractLinks("转发 t.me/a/1，还有 https://t.me/b/2 和 notme/1")
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 links", got)
	}
	if got[0] != "t.me/a/1" {
		t.Errorf("first = %q, want t.me/a/1", got[0])
	}
}
