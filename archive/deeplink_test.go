package archive

import "testing"

func TestStartPayloadRoundTrip(t *testing.T) {
	for _, n := range []int64{1, 2, 41, 42, 999, 1337, 1 << 20, 1 << 40} {
		p := StartPayload(n)
		if p == "" {
			t.Fatalf("serial %d produced an empty payload", n)
		}
		got, ok := ParseStartPayload(p)
		if !ok || got != n {
			t.Fatalf("round trip %d -> %q -> %d ok=%v", n, p, got, ok)
		}
	}
	if StartPayload(0) != "" {
		t.Fatal("serial 0 must not produce a payload")
	}
	// Not ours.
	// "rZZZ" is deliberately absent: ZZZ is valid base36, so it *is* one of
	// our payloads (serial 46655). Only genuinely malformed input is rejected.
	for _, p := range []string{"", "x12", "r", "payload", "r!!!", "r-"} {
		if n, ok := ParseStartPayload(p); ok {
			t.Fatalf("%q parsed as serial %d", p, n)
		}
	}
}

func TestBuildSerialLink(t *testing.T) {
	cases := map[string]string{
		"@some_bot":             "https://t.me/some_bot",
		"some_bot":              "https://t.me/some_bot",
		"t.me/some_bot":         "https://t.me/some_bot",
		"https://t.me/some_bot": "https://t.me/some_bot",
	}
	for in, want := range cases {
		if got := BuildStartLink(in, ""); got != want {
			t.Fatalf("BuildStartLink(%q)=%q want %q", in, got, want)
		}
	}
	link := BuildSerialLink("@some_bot", 1337)
	want := "https://t.me/some_bot?start=" + StartPayload(1337)
	if link != want {
		t.Fatalf("link=%q want %q", link, want)
	}
}

func TestParseStartLink(t *testing.T) {
	bot, payload, ok := ParseStartLink("https://t.me/some_bot?start=r16")
	if !ok || bot != "some_bot" || payload != "r16" {
		t.Fatalf("bot=%q payload=%q ok=%v", bot, payload, ok)
	}
	if n, ok := ParseStartPayload(payload); !ok || n != 42 {
		t.Fatalf("payload did not decode to 42: %d %v", n, ok)
	}
	if _, _, ok := ParseStartLink("https://example.com/x"); ok {
		t.Fatal("non-telegram link accepted")
	}
}

func TestBotUsernameNormalization(t *testing.T) {
	cases := map[string]string{
		"@a_bot":                      "a_bot",
		"https://t.me/a_bot?start=r1": "a_bot",
		"telegram.me/a_bot":           "a_bot",
		"t.me/a_bot/":                 "a_bot",
	}
	for in, want := range cases {
		if got := BotUsername(in); got != want {
			t.Fatalf("BotUsername(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSerialRefPadsToSixDigits(t *testing.T) {
	if got := SerialRef(1337); got != "#001337" {
		t.Fatalf("got %q", got)
	}
	if got := SerialRef(0); got != "#—" {
		t.Fatalf("got %q", got)
	}
}
