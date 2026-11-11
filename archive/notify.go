package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// notifyDispatchTimeout bounds one outbound notification so a dead webhook
// cannot stall the archiving loop.
const notifyDispatchTimeout = 10 * time.Second

// Delivery is retried a bounded number of times so a transient network blip or
// a 5xx does not silently lose a notification. The whole asynchronous dispatch
// lane gets notifyDeliveryBudget, enough for all attempts plus backoff.
const (
	notifyDeliveryAttempts = 3
	notifyRetryBase        = time.Second
	notifyDeliveryBudget   = 3*notifyDispatchTimeout + 30*time.Second
)

var (
	// errNotifyRetryable marks a failure worth retrying (5xx / 429).
	errNotifyRetryable = errors.New("notify: retryable delivery failure")
	// errNotifyBlocked marks an SSRF-refused destination. Retrying it would
	// only repeat the same refusal, so it is treated as permanent.
	errNotifyBlocked = errors.New("notify: destination blocked")
)

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
	// WebhookHTTP is the client used for user-supplied webhook URLs. It is
	// separate from HTTP because its dialer refuses loopback/private/metadata
	// destinations (SSRF). HTTP stays on the default transport so Telegram
	// delivery keeps honouring HTTPS_PROXY.
	//
	// nil falls back to HTTP, which is only correct for embedders that already
	// enforce their own egress policy; NewNotifier always fills it in.
	WebhookHTTP *http.Client

	// RetryBackoff is the delay before the first retry, doubled on each
	// subsequent attempt. Zero uses notifyRetryBase; a negative value disables
	// retries (used by tests that exercise permanent failures).
	RetryBackoff time.Duration

	sem     chan struct{}
	rate    rateWindow
	rateMu  sync.Mutex
	pruned  bool
	pruneMu sync.Mutex
}

func NewNotifier(s *store.Store) *Notifier {
	return &Notifier{
		Store:       s,
		HTTP:        &http.Client{Timeout: notifyDispatchTimeout},
		WebhookHTTP: newWebhookHTTPClient(),
		sem:         make(chan struct{}, notifyMaxConcurrent),
	}
}

// blockedWebhookPrefixes are the non-public ranges a webhook must never reach.
// netip covers loopback/private/link-local/multicast via its helpers; these are
// the extras that matter for SSRF: 0.0.0.0/8 (often routed to localhost on
// Linux), CGNAT 100.64.0.0/10 (cloud metadata endpoints such as 100.100.100.200),
// 192.0.0.0/24 (IETF protocol assignments) and 198.18.0.0/15 (benchmarking).
var blockedWebhookPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64: can map any IPv4, including private
}

// blockedWebhookAddr reports whether ip is a destination a webhook must not
// reach. IPv4-mapped IPv6 is unwrapped first so ::ffff:127.0.0.1 is rejected.
func blockedWebhookAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedWebhookPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// blockedWebhookHost is the syntactic half of the check: it rejects literal
// private addresses and loopback names. Hostnames that merely *resolve* to a
// private address are caught at dial time, which is the only point that sees
// the real address.
func blockedWebhookHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return blockedWebhookAddr(ip)
	}
	return false
}

// newWebhookHTTPClient builds the egress-restricted client used for webhooks.
// The control hook runs after resolution and before connect, so a hostname
// pointing at a private address (including a rebound DNS answer) is refused
// rather than connected to. Redirects are re-validated and capped.
func newWebhookHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: notifyDispatchTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if blockedWebhookAddr(ip) {
				return fmt.Errorf("%w: webhook destination %s is in a blocked range", errNotifyBlocked, ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: notifyDispatchTimeout,
		Transport: &http.Transport{
			// Proxy: nil on purpose. An environment proxy would be the only
			// peer the address check sees, and a proxy can reach anything, so
			// honouring it here would silently disable the SSRF guard.
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   notifyDispatchTimeout,
			ResponseHeaderTimeout: notifyDispatchTimeout,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("webhook: too many redirects")
			}
			if !ValidNotifyWebhook(req.URL.String()) {
				return fmt.Errorf("webhook: redirect to %q is not an allowed destination", req.URL.Redacted())
			}
			return nil
		},
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
			ctx, cancel := context.WithTimeout(context.Background(), notifyDeliveryBudget)
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
		err := n.withRetry(ctx, func(attemptCtx context.Context) error {
			return n.sendWebhook(attemptCtx, r, payload)
		})
		if err != nil {
			errs = append(errs, "webhook: "+err.Error())
		}
	}
	if r.BotToken != "" && r.BotChatID != "" {
		err := n.withRetry(ctx, func(attemptCtx context.Context) error {
			return n.sendTelegram(attemptCtx, r, payload.preview())
		})
		if err != nil {
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

// withRetry runs send up to notifyDeliveryAttempts times with doubling
// backoff. Permanent failures (4xx, an SSRF refusal, a cancelled context) stop
// immediately; only network errors and 5xx/429 are retried.
func (n *Notifier) withRetry(ctx context.Context, send func(context.Context) error) error {
	attempts := notifyDeliveryAttempts
	base := n.RetryBackoff
	if base < 0 {
		attempts = 1
	} else if base == 0 {
		base = notifyRetryBase
	}
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(base << (attempt - 1))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		err = send(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !retryableNotifyError(err) {
			return err
		}
		slog.Default().Warn("notify: retrying delivery",
			slog.Int("attempt", attempt+1), slog.String("error", err.Error()))
	}
	return err
}

func retryableNotifyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNotifyBlocked) {
		return false
	}
	if errors.Is(err, errNotifyRetryable) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// notifyStatusError classifies an HTTP status: 429 and 5xx are transient,
// everything else (auth, bad payload, 404) would fail the same way again.
func notifyStatusError(outlet string, code int) error {
	if code == http.StatusTooManyRequests || code >= 500 {
		return fmt.Errorf("%w: %s returned status %d", errNotifyRetryable, outlet, code)
	}
	return fmt.Errorf("%s returned status %d", outlet, code)
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
// Truncation counts runes, not bytes: slicing a Chinese message at a byte
// offset can cut a UTF-8 sequence in half and the Bot API rejects the payload.
func (p notifyPayload) preview() string {
	return truncateRunes(p.Text, notifyTextLimit)
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
	// Rules already stored before write-time validation existed may still carry
	// a non-http(s) URL, so delivery refuses to build the request for them.
	// Private/loopback targets are refused by WebhookHTTP's dial guard rather
	// than here, so a deployment-supplied client stays in control of egress.
	if !notifyWebhookSchemeOK(r.WebhookURL) {
		return fmt.Errorf("webhook url %q is not http(s)", r.WebhookURL)
	}
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
	client := n.WebhookHTTP
	if client == nil {
		// Only reachable for embedders that constructed Notifier directly;
		// NewNotifier always supplies the guarded client.
		client = n.HTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return notifyStatusError("webhook", resp.StatusCode)
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
		return notifyStatusError("bot api", resp.StatusCode)
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
