package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
	tutil "github.com/iyear/tdl/core/util/tutil"
)

// Auto-click: when a monitored channel posts a message carrying inline buttons
// or t.me links, the user account "clicks" them on the user's behalf and the
// outcome (bot alert, /start reply, fetched content) is pushed to a dedicated
// Telegram Bot outlet. Everything is best-effort and asynchronous: a failing
// click must never slow down archiving.
const (
	autoclickTimeout    = 30 * time.Second
	responseWindow      = 25 * time.Second
	autoclickMaxButtons = 3

	// autoclickRateMax bounds clicks per target per window; inline-button
	// farming is exactly the kind of behavior that draws flood limits.
	autoclickRateMax    = 20
	autoclickRateWindow = time.Minute

	autoclickMaxConcurrent = 4

	// autoclickLogKeep bounds the autoclick_log table.
	autoclickLogKeep = 200

	// echoWindow suppresses auto-clicking inside a chat we just sent a click
	// into. A reward bot answers /start with a fresh message that carries the
	// same button: message-id deduplication is blind to it (new id, same
	// payload), so without this window the clicker clicks its own reply and the
	// bot answers again — one loop every ~20s, indefinitely. 90s is longer than
	// any observed bot burst (a full reply set lands in 1-2s) and shorter than a
	// human's attention span when the window is what delays a real message.
	echoWindow = 90 * time.Second

	// autoclickTextLimit keeps Bot API sendMessage payloads inside Telegram's
	// 4096 character message limit with headroom for the header line, mirroring
	// the notify pipeline.
	autoclickTextLimit = 3500
)

// clickable is one thing worth clicking on an archived message.
type clickable struct {
	Kind string // callback | url | link
	Text string
	URL  string // url and link kinds
	Data []byte // callback payload
}

// AutoClicker reacts to newly archived messages from auto-click enabled
// targets. Runtime access is injected as a function so the clicker survives
// supervisor reconnects without holding a stale client.
type AutoClicker struct {
	Store  *store.Store
	Notify *Notifier

	runtime  func() *Runtime
	sem      chan struct{}
	rate     rateWindow
	rateMu   sync.Mutex
	seen     map[dupKey]time.Time
	seenMu   sync.Mutex
	pending  map[int64]time.Time
	pendMu   sync.Mutex
	echo     map[int64]time.Time
	echoMu   sync.Mutex
	echoLog  map[int64]bool
	pruned   bool
	pruneMu  sync.Mutex
	prunedAt time.Time

	// lastClick paces the queue worker per chat; queueOnce keeps a single
	// worker alive across supervisor reconnects.
	lastClick   map[int64]time.Time
	lastClickMu sync.Mutex
	queueOnce   sync.Once
}

type dupKey struct {
	chatID int64
	msgID  int
}

func NewAutoClicker(s *store.Store, n *Notifier, rt func() *Runtime) *AutoClicker {
	return &AutoClicker{
		Store: s, Notify: n, runtime: rt,
		sem:       make(chan struct{}, autoclickMaxConcurrent),
		seen:      map[dupKey]time.Time{},
		pending:   map[int64]time.Time{},
		lastClick: map[int64]time.Time{},
		echo:      map[int64]time.Time{},
		echoLog:   map[int64]bool{},
	}
}

// markEcho opens a suppression window for chatID after a click was sent into
// it, so the reply the bot posts back is never treated as a fresh instruction.
func (a *AutoClicker) markEcho(chatID int64) {
	if chatID == 0 {
		return
	}
	a.echoMu.Lock()
	defer a.echoMu.Unlock()
	a.echo[chatID] = time.Now().Add(echoWindow)
	delete(a.echoLog, chatID)
}

// isEcho reports whether chatID is inside a suppression window. first is true
// for the first suppressed message of a window, which is the one worth logging:
// logging all eleven lines of a bot reply would bury the log table.
func (a *AutoClicker) isEcho(chatID int64) (echo bool, first bool) {
	a.echoMu.Lock()
	defer a.echoMu.Unlock()
	deadline, ok := a.echo[chatID]
	if !ok {
		return false, false
	}
	if time.Now().After(deadline) {
		delete(a.echo, chatID)
		delete(a.echoLog, chatID)
		return false, false
	}
	if !a.echoLog[chatID] {
		a.echoLog[chatID] = true
		return true, true
	}
	return true, false
}

var (
	// t.me deep links carrying a bot start payload: t.me/<bot>?start=<param>
	tgStartLinkRE = regexp.MustCompile(`(?i)^https?://(?:t\.me|telegram\.me)/([A-Za-z0-9_]{4,64})\?(?:.*&)?start=([^&\s]+)`)
	// any t.me link with a message id: t.me/<name>/<id> or t.me/c/<id>/<msg>
	tgMessageLinkRE = regexp.MustCompile(`(?i)^https?://(?:t\.me|telegram\.me)/((?:c/\d+/\d+)|([A-Za-z0-9_]{4,64}/\d+))`)
	tgHostRE        = regexp.MustCompile(`(?i)^https?://(?:t\.me|telegram\.me)/`)
)

// clickables extracts everything clickable from the raw Telegram message:
// inline keyboard buttons (callback + URL) and t.me links in the body.
func clickables(m *tg.Message) []clickable {
	var out []clickable
	// Direct field access: GetReplyMarkup() keys off the wire flags, which are
	// absent on values rebuilt in tests, and the field itself is authoritative.
	if inline, ok := m.ReplyMarkup.(*tg.ReplyInlineMarkup); ok && inline != nil {
		for _, row := range inline.Rows {
			for _, b := range row.Buttons {
				switch btn := b.(type) {
				case *tg.KeyboardButtonCallback:
					out = append(out, clickable{Kind: "callback", Text: btn.Text, Data: btn.Data})
				case *tg.KeyboardButtonURL:
					out = append(out, clickable{Kind: "url", Text: btn.Text, URL: btn.URL})
				case *tg.KeyboardButtonURLAuth:
					out = append(out, clickable{Kind: "url", Text: btn.Text, URL: btn.URL})
				}
			}
		}
	}
	body := m.Message
	seen := map[string]struct{}{}
	for _, u := range store.ExtractLinks(body) {
		if tgHostRE.MatchString(u) {
			seen[u] = struct{}{}
			out = append(out, clickable{Kind: "link", Text: "正文链接", URL: u})
		}
	}
	// A pill rendered as a text link carries its destination in the entity, not
	// in the body: the visible label is whatever the author typed, so scanning
	// the text for URLs finds nothing. These are exactly the buttons an operator
	// sees in Telegram and expects a click to work, so read them from the
	// entities as well as from the keyboard.
	for _, e := range m.Entities {
		u, ok := e.(*tg.MessageEntityTextURL)
		if !ok || u.URL == "" || !tgHostRE.MatchString(u.URL) {
			continue
		}
		if _, dup := seen[u.URL]; dup {
			continue
		}
		seen[u.URL] = struct{}{}
		out = append(out, clickable{Kind: "link", Text: "文内按钮", URL: u.URL})
	}
	if len(out) > autoclickMaxButtons {
		out = out[:autoclickMaxButtons]
	}
	return out
}

// matchesKeywords reports whether any keyword appears in the button text, its
// URL or the message body. Empty keyword list means click everything.
func matchesKeywords(keywords []string, texts ...string) bool {
	if len(keywords) == 0 {
		return true
	}
	hay := acLower(strings.Join(texts, " "))
	for _, k := range keywords {
		if k != "" && strings.Contains(hay, acLower(k)) {
			return true
		}
	}
	return false
}

func acLower(s string) string {
	return strings.ToLower(s)
}

// OnNewMessage evaluates one freshly archived realtime message. Called from
// the collector after the message row is persisted; never blocks archiving.
func (a *AutoClicker) OnNewMessage(ctx context.Context, t store.Target, x store.Message, m *tg.Message) {
	if !t.AutoClick {
		return
	}
	// A click was just sent into this chat: its reply is an echo, not a new
	// instruction. Reward bots repost the same button, so clicking the echo is
	// what turned a single click into an 11-minute self-feeding loop.
	if echo, first := a.isEcho(x.ChatID); echo {
		if first {
			a.logClick(ctx, x, "echo_skipped", "刚向该会话发过点击，回复不再作为新指令处理", "")
		}
		return
	}
	// Edits and album siblings re-deliver the same message row; clicking twice
	// is both wasteful and user-visible. The in-process map covers replays, the
	// persisted log covers restarts and gap recovery.
	if a.alreadySeen(x.ChatID, x.MessageID) {
		return
	}
	if done, err := a.Store.HasAutoClickLog(ctx, x.ChatID, x.MessageID); err == nil && done {
		return
	}
	btns := clickables(m)
	btns = filterClickables(btns, t.AutoClickKeywords, x)
	if len(btns) == 0 {
		return
	}
	if !a.allowRate(x.ChatID) {
		// Out of flood budget for this minute: queue it instead of dropping.
		// A bot that posts a burst of buttoned messages is exactly the case
		// where "skip" used to lose most of the payload.
		if n, err := a.Enqueue(ctx, x.ChatID, []int{x.MessageID}); err == nil && n > 0 {
			a.logClick(ctx, x, "queued", "rate_limited", "")
		} else {
			a.logClick(ctx, x, "skip", "rate_limited", "")
		}
		return
	}
	viaBot, _ := m.GetViaBotID()
	select {
	case a.sem <- struct{}{}:
		go func() {
			defer func() { <-a.sem }()
			runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoclickTimeout)
			defer cancel()
			a.clickAll(runCtx, x, btns, viaBot)
		}()
	default:
		if n, err := a.Enqueue(ctx, x.ChatID, []int{x.MessageID}); err == nil && n > 0 {
			a.logClick(ctx, x, "queued", "lane_saturated", "")
		} else {
			slog.Default().Warn("autoclick: lane saturated, dropping", slog.Int64("chat_id", x.ChatID), slog.Int("message_id", x.MessageID))
		}
	}
}

// filterClickables keeps only the clickables that match a keyword. Buttons
// match on their own text/URL; body links additionally match the message text,
// because their label is a fixed placeholder. Message-level gating is the
// target's Keywords filter; the per-button match here stays deliberate so one
// word in the body cannot make every button fire.
func filterClickables(btns []clickable, keywords []string, x store.Message) []clickable {
	if len(keywords) == 0 {
		return btns
	}
	body := x.Text + " " + x.Caption
	var out []clickable
	for _, b := range btns {
		hay := b.Text + " " + b.URL
		if b.Kind == "link" {
			hay += " " + body
		}
		if matchesKeywords(keywords, hay) {
			out = append(out, b)
		}
	}
	return out
}

// clickAll runs the queued clicks for one message. The response-capture window
// is opened by the individual click handlers only after the interaction
// actually succeeded, so a failed click cannot swallow an unrelated message.
// rawEntity is the subset of a serialized Telegram entity this package needs.
//
// Decoding a stored message back into tg.Message does not work: the envelope
// carries interface-typed peer fields that the JSON codec cannot round-trip, so
// the whole value fails to unmarshal. Reading the entity array on its own
// sidesteps that — and the URL lives there, which is the only field a text-link
// pill needs.
type rawEntity struct {
	Offset int    `json:"Offset"`
	Length int    `json:"Length"`
	URL    string `json:"URL"`
}

type rawMessageShape struct {
	ID       int         `json:"ID"`
	Message  string      `json:"Message"`
	Entities []rawEntity `json:"Entities"`
}

// utf16Slice returns the label a Telegram entity covers. Offsets are counted in
// UTF-16 code units, not bytes and not runes, so a Chinese caption pairs every
// character with a two-unit slot and a byte slice would land mid-character.
func utf16Slice(s string, offset, length int) string {
	u := utf16.Encode([]rune(s))
	if offset < 0 || length <= 0 || offset+length > len(u) {
		return ""
	}
	return string(utf16.Decode(u[offset : offset+length]))
}

// clickablesFromRaw recovers clickable destinations from the archived raw JSON
// with no network call at all. This is what makes a backfill over thousands of
// messages practical: the queue only fetches a message when the local copy
// genuinely has nothing to click.
func clickablesFromRaw(x store.Message) []clickable {
	if len(x.Raw) == 0 {
		return nil
	}
	var r rawMessageShape
	if err := json.Unmarshal(x.Raw, &r); err != nil {
		return nil
	}
	body := r.Message
	if body == "" {
		body = x.Text
	}
	if body == "" {
		body = x.Caption
	}
	var out []clickable
	seen := map[string]struct{}{}
	for _, e := range r.Entities {
		if e.URL == "" || !tgHostRE.MatchString(e.URL) {
			continue
		}
		if _, dup := seen[e.URL]; dup {
			continue
		}
		seen[e.URL] = struct{}{}
		label := strings.TrimSpace(utf16Slice(body, e.Offset, e.Length))
		if label == "" {
			label = "文内按钮"
		}
		out = append(out, clickable{Kind: "link", Text: label, URL: e.URL})
	}
	for _, u := range store.ExtractLinks(body) {
		if !tgHostRE.MatchString(u) {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, clickable{Kind: "link", Text: "正文链接", URL: u})
	}
	return out
}

func (a *AutoClicker) clickAll(ctx context.Context, x store.Message, btns []clickable, viaBot int64) int {
	rt := a.runtime()
	if rt == nil {
		a.logClick(ctx, x, "no_connection", "会话未连接或未登录", "")
		return 0
	}
	peer, err := ResolveChatID(ctx, rt, x.ChatID)
	if err != nil {
		a.logClick(ctx, x, "failed", "解析聊天失败: "+err.Error(), "")
		return 0
	}
	ok := 0
	for _, b := range btns {
		switch b.Kind {
		case "callback":
			if a.clickCallback(ctx, rt, peer, x, b, viaBot) {
				ok++
			}
		case "url", "link":
			if a.clickURL(ctx, rt, x, b) {
				ok++
			}
		}
	}
	return ok
}

// clickCallback presses an inline callback button with the user account.
func (a *AutoClicker) clickCallback(ctx context.Context, rt *Runtime, peer peers.Peer, x store.Message, b clickable, viaBot int64) bool {
	ans, err := rt.API.MessagesGetBotCallbackAnswer(ctx, &tg.MessagesGetBotCallbackAnswerRequest{
		Peer:  peer.InputPeer(),
		MsgID: x.MessageID,
		Data:  b.Data,
	})
	if err != nil {
		a.logClick(ctx, x, "failed", "callback 点击失败: "+err.Error(), b.Text)
		a.deliver(ctx, x, fmt.Sprintf("🔘 自动点击「%s」失败：%v", b.Text, err), "failed", b)
		return false
	}
	// The bot may answer in the source chat or, reward-bot style, in a private
	// chat; arm both windows so either reply is captured. A private-chat reply
	// only reaches the archive if that chat is a monitor target, so the target
	// is registered before the window opens rather than after the reply is lost.
	a.ensureResponseTarget(ctx, x, viaBot, b)
	a.armResponse(x.ChatID)
	a.markEcho(x.ChatID)
	if viaBot > 0 && viaBot != x.ChatID {
		a.armResponse(viaBot)
		a.markEcho(viaBot)
	}
	text := fmt.Sprintf("🔘 已自动点击「%s」（来自 %s）", b.Text, a.chatLabel(ctx, x))
	if msg := strings.TrimSpace(ans.Message); msg != "" {
		if ans.Alert {
			text += "\n\n❗ " + msg
		} else {
			text += "\n\n" + msg
		}
	}
	a.logClick(ctx, x, "clicked", "", b.Text)
	a.deliver(ctx, x, text, "clicked", b)
	return true
}

// clickURL handles URL buttons and body links. t.me deep links are executed
// (bot /start payload or message fetch); plain web links are only forwarded —
// the archive account must never open arbitrary external URLs.
// ensureResponseTarget registers a bot that answers in a private chat as a
// monitor target before its reply arrives. The collector drops any message
// whose chat has no target row, so without this the payoff of a click — the
// file the bot sends back — is received and then thrown away. An existing
// target is never rewritten: its keyword filters and size caps stay the user's.
func (a *AutoClicker) ensureResponseTarget(ctx context.Context, x store.Message, viaBot int64, b clickable) {
	a.ensureTarget(ctx, viaBot, x, b)
}

// ensureTarget registers a bot chat as a monitor target. The collector drops
// every message whose chat has no target row, so a reply from an unregistered
// bot is received and then thrown away — the click "works" and the payoff
// vanishes. Existing targets are never rewritten.
func (a *AutoClicker) ensureTarget(ctx context.Context, chatID int64, x store.Message, b clickable) {
	if chatID <= 0 || chatID == x.ChatID {
		return
	}
	if _, err := a.Store.Target(ctx, chatID); err == nil {
		return
	}
	viaBot := chatID
	t := store.Target{
		ChatID: viaBot, Enabled: true,
		ArchiveText: true, ArchiveMedia: true, ArchiveFiles: true,
		DownloadMedia: true, ArchiveMode: "local",
	}
	if err := a.Store.AddTarget(ctx, t); err != nil {
		slog.Default().Warn("autoclick: cannot register reply source",
			slog.Int64("chat_id", viaBot), slog.String("error", err.Error()))
		return
	}
	a.logClick(ctx, x, "auto_target", fmt.Sprintf("已自动加入监控，用于接收「%s」的回复", b.Text), b.Text)
}

// ClickOne presses one inline callback button on an already archived message.
// The web UI calls this so a click on the page performs the same interaction as
// a click inside Telegram: the user account asks the bot, the bot answers in a
// monitored chat, and the collector archives that answer with a serial. The
// message is re-read from the server because only the live tg.Message still
// carries the callback payload — the archived copy keeps labels only.
func (a *AutoClicker) ClickOne(ctx context.Context, x store.Message, index int) (string, error) {
	rt := a.runtime()
	if rt == nil {
		return "", fmt.Errorf("会话未连接或未登录")
	}
	peer, err := ResolveChatID(ctx, rt, x.ChatID)
	if err != nil {
		return "", fmt.Errorf("解析聊天失败: %w", err)
	}
	m, err := a.liveMessage(ctx, rt, peer, x)
	if err != nil {
		return "", err
	}
	btns := clickables(m)
	if index < 0 || index >= len(btns) {
		return "", fmt.Errorf("按钮序号 %d 超出范围（该消息有 %d 个可点按钮）", index, len(btns))
	}
	b := btns[index]
	if b.Kind != "callback" {
		// A pill is a link, not a callback: pressing it means opening the bot
		// deep link or fetching the linked message, which is exactly what the
		// automatic path already does. Refusing here made the page's click a
		// dead end for every text-link button — the ones that look most like
		// buttons in Telegram.
		if !a.allowRate(x.ChatID) {
			return "", fmt.Errorf("点击过于频繁，请稍后再点")
		}
		if !a.clickURL(ctx, rt, x, b) {
			return "", fmt.Errorf("打开「%s」失败，详情见点击日志", b.Text)
		}
		return "已替你打开「" + b.Text + "」，机器人回复会自动归档并编号", nil
	}
	if !a.allowRate(x.ChatID) {
		return "", fmt.Errorf("点击过于频繁，请稍后再点")
	}
	viaBot, _ := m.GetViaBotID()
	ans, err := rt.API.MessagesGetBotCallbackAnswer(ctx, &tg.MessagesGetBotCallbackAnswerRequest{
		Peer:  peer.InputPeer(),
		MsgID: x.MessageID,
		Data:  b.Data,
	})
	if err != nil {
		a.logClick(ctx, x, "failed", "网页点击失败: "+err.Error(), b.Text)
		return "", err
	}
	a.ensureResponseTarget(ctx, x, viaBot, b)
	a.armResponse(x.ChatID)
	a.markEcho(x.ChatID)
	if viaBot > 0 && viaBot != x.ChatID {
		a.armResponse(viaBot)
		a.markEcho(viaBot)
	}
	a.logClick(ctx, x, "clicked", "网页端点击", b.Text)
	msg := strings.TrimSpace(ans.Message)
	if msg == "" {
		msg = "已替你点击，等机器人把内容发到会话里（会自动归档编号）"
	}
	return msg, nil
}

// liveMessage re-reads one message so the callback payload is available.
// Channel history lives behind channels.getMessages: messages.getMessages
// answers CHANNEL_INVALID for it, which looks exactly like a deleted message.
func (a *AutoClicker) liveMessage(ctx context.Context, rt *Runtime, peer peers.Peer, x store.Message) (*tg.Message, error) {
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: x.MessageID}}
	var res tg.MessagesMessagesClass
	var err error
	if _, isChannel := rt.Peers.ResolveChannelID(ctx, x.ChatID); isChannel == nil {
		ch, ok := asInputChannel(peer.InputPeer())
		if !ok {
			return nil, fmt.Errorf("无法定位频道实体，读取消息失败")
		}
		res, err = rt.API.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: ch, ID: ids,
		})
	} else {
		res, err = rt.API.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return nil, fmt.Errorf("读取消息失败: %w", err)
	}
	for _, mc := range messagesOf(res) {
		if m, ok := mc.(*tg.Message); ok && m.ID == x.MessageID {
			return m, nil
		}
		if e, ok := mc.(*tg.MessageEmpty); ok && e.ID == x.MessageID {
			return nil, fmt.Errorf("这条消息已被删除，按钮无从点起")
		}
	}
	return nil, fmt.Errorf("消息已不在会话中（可能被删了）")
}

// asInputChannel narrows a resolved peer into the channel form that
// channels.getMessages requires.
func asInputChannel(p tg.InputPeerClass) (tg.InputChannelClass, bool) {
	switch v := p.(type) {
	case *tg.InputPeerChannel:
		return &tg.InputChannel{ChannelID: v.ChannelID, AccessHash: v.AccessHash}, true
	}
	return nil, false
}

// messagesOf flattens the several shapes MessagesGetMessages can return.
func messagesOf(res tg.MessagesMessagesClass) []tg.MessageClass {
	switch v := res.(type) {
	case *tg.MessagesMessages:
		return v.Messages
	case *tg.MessagesChannelMessages:
		return v.Messages
	case *tg.MessagesMessagesSlice:
		return v.Messages
	}
	return nil
}

func (a *AutoClicker) clickURL(ctx context.Context, rt *Runtime, x store.Message, b clickable) bool {
	u := b.URL
	if m := tgStartLinkRE.FindStringSubmatch(u); m != nil {
		return a.startBot(ctx, rt, x, b, m[1], m[2])
	}
	if tgMessageLinkRE.MatchString(u) {
		return a.fetchLinkedMessage(ctx, rt, x, b)
	}
	if tgHostRE.MatchString(u) {
		// profile or other non-executable t.me link: hand it to the user
		a.logClick(ctx, x, "sent", "t.me 链接转发", b.Text)
		a.deliver(ctx, x, fmt.Sprintf("🔗 %s 的消息里带了链接「%s」：\n%s", a.chatLabel(ctx, x), b.Text, u), "sent", b)
		return true
	}
	a.logClick(ctx, x, "sent", "外部链接转发", b.Text)
	a.deliver(ctx, x, fmt.Sprintf("🔗 %s 的消息里带了一个外部链接「%s」，需要你手动打开：\n%s", a.chatLabel(ctx, x), b.Text, u), "sent", b)
	return true
}

// startBot opens a t.me/<bot>?start=<param> deep link by sending the /start
// command from the user account; the bot's reply lands in the capture window.
func (a *AutoClicker) startBot(ctx context.Context, rt *Runtime, x store.Message, b clickable, username, param string) bool {
	bot, err := rt.Peers.Resolve(ctx, strings.TrimPrefix(username, "@"))
	if err != nil {
		a.logClick(ctx, x, "failed", "解析 bot 失败: "+err.Error(), b.Text)
		a.deliver(ctx, x, fmt.Sprintf("🔘 自动打开「%s」失败，无法解析 bot @%s：%v", b.Text, username, err), "failed", b)
		return false
	}
	payload := "/start"
	if param != "" {
		payload = "/start " + param
	}
	// Destination-level memory: /start <same payload> to the same bot carries no
	// new information, and repeating it is precisely how the echo loop fed
	// itself. Checked before sending so a repeat costs nothing.
	key := "bot:" + acLower(strings.TrimPrefix(username, "@")) + "|" + payload
	if seen, err := a.Store.AutoClickTargetSeen(ctx, key); err == nil && seen {
		a.logClick(ctx, x, "skip", "同一目标此前已点过: "+key, b.Text)
		return false
	}
	if _, err := rt.API.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     bot.InputPeer(),
		Message:  payload,
		RandomID: rand.Int63(),
	}); err != nil {
		a.logClick(ctx, x, "failed", "/start 发送失败: "+err.Error(), b.Text)
		a.deliver(ctx, x, fmt.Sprintf("🔘 自动打开「%s」失败：%v", b.Text, err), "failed", b)
		return false
	}
	if err := a.Store.RecordAutoClickTarget(ctx, key); err != nil {
		slog.Default().Warn("autoclick: record target", slog.String("error", err.Error()))
	}
	// The bot answers in its own private chat; register it first or the reply is
	// dropped by the collector before it can be archived.
	a.ensureTarget(ctx, bot.ID(), x, b)
	a.armResponse(bot.ID())
	a.markEcho(bot.ID())
	a.logClick(ctx, x, "clicked", "/start "+param, b.Text)
	a.deliver(ctx, x, fmt.Sprintf("🔘 已自动点击「%s」并向 @%s 发送 %s，等待其回复…", b.Text, username, payload), "clicked", b)
	return true
}

// fetchLinkedMessage resolves a t.me message link, archives the target message
// (and its album) into the local store, and summarizes it to the outlet.
func (a *AutoClicker) fetchLinkedMessage(ctx context.Context, rt *Runtime, x store.Message, b clickable) bool {
	t, tErr := a.Store.Target(ctx, x.ChatID)
	peer, msgID, err := tutil.ParseMessageLink(ctx, rt.Peers, b.URL)
	if err != nil {
		a.logClick(ctx, x, "failed", "解析消息链接失败: "+err.Error(), b.Text)
		a.deliver(ctx, x, fmt.Sprintf("🔗 自动打开「%s」失败：%v", b.Text, err), "failed", b)
		return false
	}
	input := peer.InputPeer()
	msg, err := tutil.GetSingleMessage(ctx, rt.API, input, msgID)
	if err != nil {
		a.logClick(ctx, x, "failed", "拉取消息失败: "+err.Error(), b.Text)
		a.deliver(ctx, x, fmt.Sprintf("🔗 自动打开「%s」失败：%v", b.Text, err), "failed", b)
		return false
	}
	msgs := []*tg.Message{msg}
	if _, ok := msg.GetGroupedID(); ok {
		if group, err := tutil.GetGroupedMessages(ctx, rt.API, input, msg); err == nil && len(group) > 0 {
			msgs = group
		}
	}
	archived := 0
	var first store.Message
	for _, fm := range msgs {
		fx := store.FromTG(fm)
		if fx.ChatID == 0 {
			continue
		}
		if err := a.Store.UpsertMessage(ctx, fx); err == nil {
			archived++
			if first.MessageID == 0 {
				first = fx
			}
		}
		if tErr == nil && t.ArchiveMedia && (t.DownloadMedia || t.ArchiveMode == "both") {
			if meta, ok := store.MediaInfo(fm); ok {
				_ = a.Store.EnsureMedia(ctx, fx, meta)
			}
			for _, meta := range store.LinkMedias(fm) {
				_ = a.Store.EnsureMedia(ctx, fx, meta)
			}
		}
	}
	if archived == 0 {
		a.logClick(ctx, x, "failed", "拉取到的内容无法归档", b.Text)
		a.deliver(ctx, x, fmt.Sprintf("🔗 自动打开「%s」失败：拉取到的内容无法归档", b.Text), "failed", b)
		return false
	}
	a.logClick(ctx, x, "fetched", fmt.Sprintf("归档 %d 条", archived), b.Text)
	body := strings.TrimSpace(first.Text)
	if len(body) > 800 {
		body = body[:800] + "…"
	}
	text := fmt.Sprintf("🔗 已自动点击「%s」并归档 %s 的 %d 条内容：\n%s", b.Text, a.chatLabel(ctx, x), archived, body)
	a.deliver(ctx, x, text, "fetched", b)
	return true
}

// armResponse opens a capture window for messages arriving from chatID.
func (a *AutoClicker) armResponse(chatID int64) {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	a.pending[chatID] = time.Now().Add(responseWindow)
}

// Observe inspects an incoming message before normal archiving: when a capture
// window is open for its chat and this message is a click response, it is
// forwarded to the outlet and reported as handled.
func (a *AutoClicker) Observe(ctx context.Context, x store.Message) bool {
	a.pendMu.Lock()
	deadline, ok := a.pending[x.ChatID]
	if ok {
		if time.Now().After(deadline) {
			delete(a.pending, x.ChatID)
			ok = false
		} else {
			delete(a.pending, x.ChatID)
		}
	}
	a.pendMu.Unlock()
	if !ok {
		return false
	}
	text := strings.TrimSpace(x.Text)
	if text == "" {
		text = strings.TrimSpace(x.Caption)
	}
	if text == "" {
		// A click usually pays out as a bare media message: "一键查询验证视频"
		// answers with a video and no caption. Dropping those here was exactly
		// the gap between "clicked" and "got the file", so the media stands in
		// for the missing text instead of ending the capture.
		if !x.HasMedia {
			return false
		}
		text = "（媒体消息，无文字）"
		if x.MediaMime != "" {
			text = "（" + x.MediaMime + " 消息，无文字）"
		}
	}
	a.deliver(ctx, x, fmt.Sprintf("💬 %s 回复：\n%s", a.chatLabel(ctx, x), text), "captured", clickable{})
	return true
}

// deliver pushes the result text through the dedicated Bot outlet. Failures
// are logged, never propagated.
func (a *AutoClicker) deliver(ctx context.Context, x store.Message, text, status string, b clickable) {
	if a.Notify == nil {
		a.logClick(ctx, x, "no_outlet", "未配置通知器", b.Text)
		return
	}
	o, err := a.Store.AutoClickOutlet(ctx)
	if err != nil {
		a.logClick(ctx, x, "failed", "读取出口配置失败: "+err.Error(), b.Text)
		return
	}
	if o.BotToken == "" || o.BotChatID == "" {
		a.logClick(ctx, x, "no_outlet", "未配置 Bot Token / chat_id", b.Text)
		return
	}
	rule := store.NotifyRule{BotToken: o.BotToken, BotChatID: o.BotChatID}
	dctx, cancel := context.WithTimeout(ctx, notifyDispatchTimeout)
	defer cancel()
	if err := a.Notify.sendTelegram(dctx, rule, truncateRunes(text, autoclickTextLimit)); err != nil {
		a.logClick(ctx, x, "failed", "推送失败: "+err.Error(), b.Text)
		return
	}
	if status != "" {
		a.logClick(ctx, x, status, "已推送", b.Text)
	}
}

// truncateRunes shortens s to at most limit runes (never splitting a UTF-8
// sequence) and marks the cut with an ellipsis.
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

func (a *AutoClicker) logClick(ctx context.Context, x store.Message, status, detail, buttonText string) {
	if err := a.Store.AddAutoClickLog(ctx, store.AutoClickLogEntry{
		ChatID: x.ChatID, MessageID: x.MessageID, ButtonText: buttonText,
		Kind: "autoclick", Status: status, Detail: detail,
	}); err != nil {
		slog.Default().Warn("autoclick: log", slog.String("error", err.Error()))
	}
	a.pruneOnce(ctx)
}

// pruneOnce trims the log table at most once a minute.
func (a *AutoClicker) pruneOnce(ctx context.Context) {
	a.pruneMu.Lock()
	defer a.pruneMu.Unlock()
	if a.pruned && time.Since(a.prunedAt) < time.Minute {
		return
	}
	a.pruned, a.prunedAt = true, time.Now()
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.Store.PruneAutoClickLog(pctx, autoclickLogKeep); err != nil {
		slog.Default().Debug("autoclick: prune", slog.String("error", err.Error()))
	}
}

func (a *AutoClicker) allowRate(chatID int64) bool {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	return a.rate.allow(chatID, autoclickRateMax, autoclickRateWindow)
}

// alreadySeen deduplicates clicks within the process; the persisted log covers
// restarts. Entries expire lazily so the map cannot grow unbounded.
func (a *AutoClicker) alreadySeen(chatID int64, msgID int) bool {
	k := dupKey{chatID, msgID}
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	if _, ok := a.seen[k]; ok {
		return true
	}
	now := time.Now()
	if len(a.seen) > 1024 {
		for kk, t := range a.seen {
			if now.Sub(t) > time.Hour {
				delete(a.seen, kk)
			}
		}
	}
	a.seen[k] = now
	return false
}

// chatLabel resolves the display title of the source chat, best effort.
func (a *AutoClicker) chatLabel(ctx context.Context, x store.Message) string {
	title, err := a.Store.ChatTitle(ctx, x.ChatID)
	if err != nil || title == "" {
		return fmt.Sprintf("chat %d", x.ChatID)
	}
	return title
}
