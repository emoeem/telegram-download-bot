package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// notifyDispatchTimeout bounds one outbound notification so a dead webhook
// cannot stall the archiving loop.
const notifyDispatchTimeout = 10 * time.Second

// notifyRateMax / notifyRateWindow bound one rule to a fixed number of
// notifications per window. A keyword that matches a high-traffic channel must
// not turn the outlet (or the receiving chat) into a firehose.
const (
	notifyRateMax    = 20
	notifyRateWindow = time.Minute

	// notifyMaxConcurrent caps concurrent outbound deliveries.
	notifyMaxConcurrent = 8

	// notifyTextLimit keeps Telegram sendMessage payloads inside the 4096
	// character message limit with headroom for the header line.
	notifyTextLimit = 3500
)

// Notifier evaluates newly archived realtime messages against notify_rules and
// pushes matches to the configured outlets. Dispatch is asynchronous and
// bounded: notifications are best-effort and must never slow down archiving.
type Notifier struct {
	Store *store.Store
	HTTP  *http.Client

	sem     chan struct{}
	rate    rateWindow
	rateMu  sync.Mutex
	pruned  bool
	pruneMu sync.Mutex
}

func NewNotifier(s *store.Store) *Notifier {
	return &Notifier{
		Store: s,
		HTTP:  &http.Client{Timeout: notifyDispatchTimeout},
		sem:   make(chan struct{}, notifyMaxConcurrent),
	}
}

// DispatchAsync evaluates the message against the enabled rules and delivers
// matches in the background. Errors are logged and recorded, never returned:
// notification failure must not fail message archiving.
func (n *Notifier) DispatchAsync(m store.Message) {
	rules, err := n.Store.NotifyRules(context.Background())
	if err != nil {
		return
	}
	var matched []store.NotifyRule
	for _, r := range rules {
		if r.Matches(m) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return
	}
	select {
	case n.sem <- struct{}{}:
		go func() {
			defer func() { <-n.sem }()
			ctx, cancel := context.WithTimeout(context.Background(), notifyDispatchTimeout)
			defer cancel()
			for _, r := range matched {
				if !n.allow(r.ID) {
					_ = n.Store.RecordNotifyDelivery(ctx, store.NotifyDelivery{RuleID: r.ID, ChatID: m.ChatID, MessageID: m.MessageID, Channel: "rate_limited", Status: "skipped"})
					continue
				}
				n.deliver(ctx, r, m)
			}
			n.pruneLog(ctx)
		}()
	default:
		// Delivery lane saturated: drop instead of queueing unboundedly. The
		// message itself is already archived; only the ping is lost.
		slog.Default().Warn("notify: dispatch lane saturated, dropping", slog.Int64("chat_id", m.ChatID), slog.Int("message_id", m.MessageID))
	}
}

// allow enforces the per-rule rate limit.
func (n *Notifier) allow(ruleID int64) bool {
	n.rateMu.Lock()
	defer n.rateMu.Unlock()
	return n.rate.allow(ruleID, notifyRateMax, notifyRateWindow)
}

// Dispatch evaluates and delivers synchronously. Used by the CLI test command.
func (n *Notifier) Dispatch(ctx context.Context, m store.Message) error {
	rules, err := n.Store.NotifyRules(ctx)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.Matches(m) {
			n.deliver(ctx, r, m)
		}
	}
	return nil
}

// DispatchTo delivers one message through one specific rule synchronously,
// bypassing the rate limit, and reports the outcome. Used by the CLI and web
// test commands to validate outlets.
func (n *Notifier) DispatchTo(ctx context.Context, r store.NotifyRule, m store.Message) store.NotifyDelivery {
	return n.deliver(ctx, r, m)
}

// OutletNames lists the delivery outlets configured on a rule.
func OutletNames(r store.NotifyRule) []string { return outletNames(r) }

func (n *Notifier) deliver(ctx context.Context, r store.NotifyRule, m store.Message) store.NotifyDelivery {
	payload := n.buildPayload(ctx, r, m)
	var errs []string
	if r.WebhookURL != "" {
		if err := n.sendWebhook(ctx, r, payload); err != nil {
			errs = append(errs, "webhook: "+err.Error())
		}
	}
	if r.BotToken != "" && r.BotChatID != "" {
		if err := n.sendTelegram(ctx, r, payload.preview()); err != nil {
			errs = append(errs, "telegram: "+err.Error())
		}
	}
	d := store.NotifyDelivery{RuleID: r.ID, ChatID: m.ChatID, MessageID: m.MessageID}
	d.Channel = strings.Join(outletNames(r), "+")
	if len(errs) == 0 {
		d.Status = "sent"
	} else {
		d.Status, d.Error = "failed", strings.Join(errs, "; ")
		slog.Default().Warn("notify: delivery failed", slog.Int64("rule_id", r.ID), slog.String("error", d.Error))
	}
	if err := n.Store.RecordNotifyDelivery(ctx, d); err != nil {
		slog.Default().Warn("notify: record delivery", slog.String("error", err.Error()))
	}
	return d
}

// outletNames lists the outlets a rule is configured to use.
func outletNames(r store.NotifyRule) []string {
	var out []string
	if r.WebhookURL != "" {
		out = append(out, "webhook")
	}
	if r.BotToken != "" && r.BotChatID != "" {
		out = append(out, "telegram")
	}
	if len(out) == 0 {
		out = append(out, "none")
	}
	return out
}

// notifyPayload is the webhook body; the Telegram path renders its text form.
type notifyPayload struct {
	Text      string `json:"text"`
	Chat      string `json:"chat"`
	ChatID    int64  `json:"chat_id"`
	MessageID int    `json:"message_id"`
	Date      int64  `json:"date"`
	Rule      string `json:"rule"`
}

// preview renders the human-readable text sent to Telegram Bot API chats.
func (p notifyPayload) preview() string {
	head := p.Text
	if len(head) > notifyTextLimit {
		head = head[:notifyTextLimit] + "…"
	}
	return head
}

func (n *Notifier) buildPayload(ctx context.Context, r store.NotifyRule, m store.Message) notifyPayload {
	p := notifyPayload{
		Text:      firstNonEmpty(m.Text, m.Caption),
		ChatID:    m.ChatID,
		MessageID: m.MessageID,
		Date:      m.Date.Unix(),
		Rule:      r.Name,
	}
	p.Chat, _ = n.Store.ChatTitle(ctx, m.ChatID)
	if p.Chat != "" {
		p.Text = "[" + p.Chat + "] " + p.Text
	}
	return p
}

func (n *Notifier) sendWebhook(ctx context.Context, r store.NotifyRule, payload notifyPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tdl-archive-notify/1.0")
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) sendTelegram(ctx context.Context, r store.NotifyRule, text string) error {
	endpoint := "https://api.telegram.org/bot" + r.BotToken + "/sendMessage"
	form := url.Values{}
	form.Set("chat_id", r.BotChatID)
	form.Set("text", text)
	form.Set("disable_web_page_preview", "true")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("bot api returned status %d", resp.StatusCode)
	}
	return nil
}

func (n *Notifier) pruneLog(ctx context.Context) {
	n.pruneMu.Lock()
	defer n.pruneMu.Unlock()
	if n.pruned {
		return
	}
	n.pruned = true
	if err := n.Store.PruneNotifyLog(ctx, 500); err != nil {
		slog.Default().Debug("notify: prune log", slog.String("error", err.Error()))
	}
}

// rateWindow is a sliding-window per-key counter.
type rateWindow struct {
	entries map[int64][]time.Time
}

func (w *rateWindow) allow(key int64, max int, window time.Duration) bool {
	if w.entries == nil {
		w.entries = map[int64][]time.Time{}
	}
	now := time.Now()
	keep := w.entries[key][:0]
	for _, t := range w.entries[key] {
		if now.Sub(t) < window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= max {
		w.entries[key] = keep
		return false
	}
	w.entries[key] = append(keep, now)
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
