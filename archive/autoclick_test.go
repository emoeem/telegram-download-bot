package archive

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
)

func sampleMarkupMessage() *tg.Message {
	return &tg.Message{
		ID:      5,
		PeerID:  &tg.PeerChannel{ChannelID: 100},
		Message: "点击领取 https://t.me/durov/12",
		ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "领取", Data: []byte("cb-data")},
			&tg.KeyboardButtonURL{Text: "打开频道", URL: "https://t.me/foo?start=bar"},
			&tg.KeyboardButtonSwitchInline{Text: "分享", Query: "share"},
		}}}},
	}
}

func TestClickablesExtraction(t *testing.T) {
	got := clickables(sampleMarkupMessage())
	if len(got) != 3 {
		t.Fatalf("want 3 clickables (2 buttons + 1 body link), got %d: %+v", len(got), got)
	}
	if got[0].Kind != "callback" || got[0].Text != "领取" || string(got[0].Data) != "cb-data" {
		t.Fatalf("callback extraction wrong: %+v", got[0])
	}
	if got[1].Kind != "url" || got[1].URL != "https://t.me/foo?start=bar" {
		t.Fatalf("url button extraction wrong: %+v", got[1])
	}
	if got[2].Kind != "link" || got[2].URL != "https://t.me/durov/12" {
		t.Fatalf("body link extraction wrong: %+v", got[2])
	}

	// No markup, plain text, no t.me links → nothing clickable.
	if n := len(clickables(&tg.Message{ID: 1, Message: "hello https://example.com/x"})); n != 0 {
		t.Fatalf("external-only text should yield nothing, got %d", n)
	}
}

func TestMatchesKeywords(t *testing.T) {
	if !matchesKeywords(nil, "任意文字") {
		t.Fatal("empty keywords must match everything")
	}
	if !matchesKeywords([]string{"领取"}, "点击「领取」") {
		t.Fatal("substring should match")
	}
	if matchesKeywords([]string{"福利"}, "点击「领取」") {
		t.Fatal("unrelated keyword must not match")
	}
	if !matchesKeywords([]string{"WORLD"}, "hello world") {
		t.Fatal("match should be case-insensitive")
	}
}

func TestFilterClickables(t *testing.T) {
	btns := clickables(sampleMarkupMessage())
	x := store.Message{Text: "点击领取", Caption: ""}

	// The callback button matches on its own label.
	kept := filterClickables(btns, []string{"领取"}, x)
	if len(kept) != 2 || kept[0].Kind != "callback" || kept[1].Kind != "link" {
		t.Fatalf("keyword should keep the callback button and the body link, got %+v", kept)
	}
	kept = filterClickables(btns, []string{"打开频道"}, x)
	if len(kept) != 1 || kept[0].Kind != "url" {
		t.Fatalf("keyword should keep the URL button, got %+v", kept)
	}
	kept = filterClickables(btns, []string{"t.me/durov"}, x)
	if len(kept) != 1 || kept[0].Kind != "link" {
		t.Fatalf("keyword should keep the body link, got %+v", kept)
	}
	// Body links also match the message text, unlike buttons: the keyword
	// "点击领取" appears in the body but not in the button label.
	kept = filterClickables(btns, []string{"点击领取"}, x)
	if len(kept) != 1 || kept[0].Kind != "link" {
		t.Fatalf("body text should qualify the link, got %+v", kept)
	}
	kept = filterClickables(btns, []string{"不存在的词"}, x)
	if len(kept) != 0 {
		t.Fatalf("no match should keep nothing, got %d", len(kept))
	}
	kept = filterClickables(btns, nil, x)
	if len(kept) != 3 {
		t.Fatalf("empty keywords keep everything, got %d", len(kept))
	}
}

func TestAutoClickerObserveCapturesResponse(t *testing.T) {
	s := openAutoClickStore(t)
	ctx := t.Context()
	// Outlet configured + a Bot API transport stub: delivery succeeds and the
	// capture status is recorded.
	if err := s.SaveAutoClickOutlet(ctx, store.AutoClickOutlet{BotToken: "123456789:AA-secret-token", BotChatID: "-100200"}); err != nil {
		t.Fatal(err)
	}
	n := &Notifier{HTTP: &http.Client{Transport: stubTelegramTransport{}}}
	a := NewAutoClicker(s, n, nil)

	x := store.Message{ChatID: 77, MessageID: 9, Text: "恭喜，已领取"}
	// No window open: the message is not a captured response.
	if a.Observe(ctx, x) {
		t.Fatal("observe must not capture without an armed window")
	}

	a.armResponse(77)
	if !a.Observe(ctx, x) {
		t.Fatal("observe should capture inside the window")
	}
	// The window is single-shot.
	if a.Observe(ctx, x) {
		t.Fatal("capture window should be consumed once")
	}

	log, err := s.ListAutoClickLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Status != "captured" {
		t.Fatalf("captured response should be logged, got %+v", log)
	}
}

// stubTelegramTransport answers every request with a successful Bot API body
// so delivery tests never touch the network.
type stubTelegramTransport struct{}

func (stubTelegramTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}

// captureTransport records the request body and answers success.
type captureTransport struct{ body string }

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	c.body = string(b)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}

func TestAutoClickDeliverTruncates(t *testing.T) {
	s := openAutoClickStore(t)
	ctx := t.Context()
	if err := s.SaveAutoClickOutlet(ctx, store.AutoClickOutlet{BotToken: "123456789:AA-secret-token", BotChatID: "-100200"}); err != nil {
		t.Fatal(err)
	}
	tr := &captureTransport{}
	a := NewAutoClicker(s, &Notifier{HTTP: &http.Client{Transport: tr}}, nil)

	a.deliver(ctx, store.Message{ChatID: 1, MessageID: 2}, "前缀 "+strings.Repeat("测", 5000), "clicked", clickable{Text: "领取"})

	form, err := url.ParseQuery(tr.body)
	if err != nil {
		t.Fatal(err)
	}
	text := form.Get("text")
	if n := utf8.RuneCountInString(text); n > autoclickTextLimit+1 {
		t.Fatalf("delivered text must stay inside the Bot API limit, got %d runes", n)
	}
	if !strings.HasSuffix(text, "…") {
		t.Fatal("truncated text should end with an ellipsis")
	}
}

func TestAutoClickerDedup(t *testing.T) {
	s := openAutoClickStore(t)
	a := NewAutoClicker(s, nil, nil)
	if a.alreadySeen(1, 2) {
		t.Fatal("first sighting must not be a dup")
	}
	if !a.alreadySeen(1, 2) {
		t.Fatal("second sighting must be a dup")
	}
}

func TestHasAutoClickLogDedup(t *testing.T) {
	s := openAutoClickStore(t)
	ctx := t.Context()
	if done, err := s.HasAutoClickLog(ctx, 10, 20); err != nil || done {
		t.Fatalf("absent entry must be false, got %v %v", done, err)
	}
	if err := s.AddAutoClickLog(ctx, store.AutoClickLogEntry{ChatID: 10, MessageID: 20, Kind: "callback", Status: "clicked"}); err != nil {
		t.Fatal(err)
	}
	if done, err := s.HasAutoClickLog(ctx, 10, 20); err != nil || !done {
		t.Fatalf("recorded entry must be true, got %v %v", done, err)
	}
	// A different message is unaffected.
	if done, err := s.HasAutoClickLog(ctx, 10, 21); err != nil || done {
		t.Fatalf("other message must stay false, got %v %v", done, err)
	}
}

func openAutoClickStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/archive.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// A click pays out as a bare media message far more often than as text: the
// "一键查询验证视频" style button answers with a video and no caption. Observe
// used to return false for those, which is exactly how a successful click lost
// its file.
func TestAutoClickerObserveCapturesMediaOnlyResponse(t *testing.T) {
	s := openAutoClickStore(t)
	ctx := t.Context()
	if err := s.SaveAutoClickOutlet(ctx, store.AutoClickOutlet{BotToken: "123456789:AA-secret-token", BotChatID: "-100200"}); err != nil {
		t.Fatal(err)
	}
	a := NewAutoClicker(s, &Notifier{HTTP: &http.Client{Transport: stubTelegramTransport{}}}, nil)

	media := store.Message{ChatID: 77, MessageID: 11, HasMedia: true, MediaMime: "video/mp4"}
	if a.Observe(ctx, media) {
		t.Fatal("no window armed yet: must not capture")
	}
	a.armResponse(77)
	if !a.Observe(ctx, media) {
		t.Fatal("a media-only reply inside the window must be captured")
	}
	// A textless, medialess message is still nothing worth forwarding.
	a.armResponse(77)
	if a.Observe(ctx, store.Message{ChatID: 77, MessageID: 12}) {
		t.Fatal("empty non-media message must not count as a capture")
	}
	log, err := s.ListAutoClickLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Status != "captured" {
		t.Fatalf("media capture must be logged once, got %+v", log)
	}
}

// The collector drops messages whose chat has no target row, so a bot answering
// in a private chat would be received and then discarded. The reply source has
// to be registered before the window opens.
func TestAutoClickerRegistersReplySource(t *testing.T) {
	s := openAutoClickStore(t)
	ctx := t.Context()
	a := NewAutoClicker(s, nil, nil)

	x := store.Message{ChatID: 100, MessageID: 5}
	a.ensureResponseTarget(ctx, x, 0, clickable{Text: "查询"})
	a.ensureResponseTarget(ctx, x, x.ChatID, clickable{Text: "查询"})

	const bot = 555
	if _, err := s.Target(ctx, bot); err == nil {
		t.Fatal("bot must not be a target before the click")
	}
	a.ensureResponseTarget(ctx, x, bot, clickable{Text: "查询"})
	got, err := s.Target(ctx, bot)
	if err != nil {
		t.Fatalf("reply source should be registered: %v", err)
	}
	if !got.Enabled || !got.DownloadMedia || !got.ArchiveMedia {
		t.Fatalf("registered target must archive and download: %+v", got)
	}

	// An existing target keeps the user's own filters.
	off := false
	if err := s.UpdateTargetRules(ctx, bot, store.TargetRules{Enabled: &off, Keywords: []string{"视频"}}); err != nil {
		t.Fatal(err)
	}
	a.ensureResponseTarget(ctx, x, bot, clickable{Text: "查询"})
	again, err := s.Target(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	if again.Enabled || len(again.Keywords) != 1 || again.Keywords[0] != "视频" {
		t.Fatalf("existing target rules must be preserved: %+v", again)
	}
}

// A text-link pill keeps its destination in the entity. Missing that means the
// button the operator sees in Telegram can never be pressed, which is precisely
// how "点一下、机器人回内容" used to fail.
func TestClickablesFromTextLinkEntity(t *testing.T) {
	m := &tg.Message{
		ID:      77,
		PeerID:  &tg.PeerChannel{ChannelID: 100},
		Message: "👉一键查询验证视频👈",
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityTextURL{Offset: 0, Length: 11, URL: "https://t.me/some_reward_bot?start=ABC123"},
			&tg.MessageEntityBold{Offset: 0, Length: 11},
		},
	}
	got := clickables(m)
	if len(got) != 1 {
		t.Fatalf("want 1 clickable from the text-link pill, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "link" || got[0].URL != "https://t.me/some_reward_bot?start=ABC123" {
		t.Fatalf("text-link extraction wrong: %+v", got[0])
	}

	// A non-t.me text link is not a Telegram interaction: it must not be picked
	// up (the archive account never opens arbitrary external URLs).
	ext := &tg.Message{ID: 78, Message: "广告", Entities: []tg.MessageEntityClass{
		&tg.MessageEntityTextURL{Offset: 0, Length: 2, URL: "https://example.com/x"},
	}}
	if n := len(clickables(ext)); n != 0 {
		t.Fatalf("external text link must not be clickable, got %d", n)
	}

	// The same destination as a plain-text URL must not be reported twice.
	dup := &tg.Message{ID: 79, Message: "https://t.me/some_reward_bot?start=ABC123",
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityTextURL{Offset: 0, Length: 43, URL: "https://t.me/some_reward_bot?start=ABC123"},
		}}
	if n := len(clickables(dup)); n != 1 {
		t.Fatalf("duplicate destination must collapse to 1, got %d", n)
	}
}

// TestEchoWindowStopsSelfFeedingLoop pins the guard for the loop that turned one
// click into eleven rounds: the bot answers /start with a message carrying the
// same button, the clicker treats the fresh message id as a new instruction and
// clicks again. Message-id deduplication cannot see it — the guard has to key on
// "we just sent something into this chat".
func TestEchoWindowStopsSelfFeedingLoop(t *testing.T) {
	a := NewAutoClicker(nil, nil, func() *Runtime { return nil })

	botChat := int64(8747293354)
	if echo, _ := a.isEcho(botChat); echo {
		t.Fatal("no click has been sent yet, nothing should be suppressed")
	}

	a.markEcho(botChat)
	echo, first := a.isEcho(botChat)
	if !echo {
		t.Fatal("a message arriving right after our click must be suppressed")
	}
	if !first {
		t.Fatal("the first suppressed message of a window is the one worth logging")
	}
	// The remaining ten lines of the bot reply must be suppressed without each
	// writing a log row, or the 200-row log fills from a single loop.
	if echo, first := a.isEcho(botChat); !echo || first {
		t.Fatalf("echo=%v first=%v, want echo=true first=false", echo, first)
	}

	// An unrelated chat is untouched: suppression is per destination.
	if echo, _ := a.isEcho(3940126558); echo {
		t.Fatal("suppression must not leak to other chats")
	}

	// Once the window lapses the chat is usable again, otherwise a target would
	// go permanently deaf after its first click.
	a.echoMu.Lock()
	a.echo[botChat] = time.Now().Add(-time.Second)
	a.echoMu.Unlock()
	if echo, _ := a.isEcho(botChat); echo {
		t.Fatal("an expired window must not suppress")
	}
}
