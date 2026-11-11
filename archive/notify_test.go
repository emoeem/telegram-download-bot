package archive

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iyear/tdl/archive/store"
)

func newNotifyTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNotifyRuleMatches(t *testing.T) {
	r := store.NotifyRule{Enabled: true, Keywords: []string{"告警", "down"}, ExcludeKeywords: []string{"已恢复"}}
	msg := store.Message{ChatID: 7, Text: "server down! 服务器告警了"}
	if !r.Matches(msg) {
		t.Fatal("keyword hit must match")
	}
	if r.Matches(store.Message{ChatID: 7, Text: "一切正常"}) {
		t.Fatal("no keyword hit must not match")
	}
	if r.Matches(store.Message{ChatID: 7, Text: "告警 已恢复"}) {
		t.Fatal("exclude word must suppress the match")
	}
	// Chat scoping.
	scoped := store.NotifyRule{Enabled: true, ChatIDs: []int64{1, 2}}
	if !scoped.Matches(store.Message{ChatID: 2, Text: "x"}) {
		t.Fatal("chat in scope must match")
	}
	if scoped.Matches(store.Message{ChatID: 3, Text: "x"}) {
		t.Fatal("chat out of scope must not match")
	}
	// Disabled rules never match; keyword-less rules match everything in scope.
	if (store.NotifyRule{Enabled: false}).Matches(msg) {
		t.Fatal("disabled rule must not match")
	}
	if !(store.NotifyRule{Enabled: true}).Matches(store.Message{ChatID: 9, Text: ""}) {
		t.Fatal("keyword-less rule must match any message")
	}
}

func TestNotifierWebhookDelivery(t *testing.T) {
	var hits int32
	var body atomic.Value // string
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		rw.WriteHeader(200)
	}))
	defer ts.Close()

	s := newNotifyTestStore(t)
	ctx := context.Background()
	if _, err := s.AddNotifyRule(ctx, store.NotifyRule{Name: "hook", Keywords: []string{"boom"}, WebhookURL: ts.URL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	n := NewNotifier(s)
	// The httptest server is on loopback, which the SSRF-guarded webhook
	// client refuses by design; inject the test client for delivery semantics.
	n.WebhookHTTP = ts.Client()
	if err := n.Dispatch(ctx, store.Message{ChatID: 5, MessageID: 11, Date: time.Unix(200, 0), Text: "boom happened"}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("webhook hit %d times", got)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body.Load().(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["text"] != "boom happened" || payload["message_id"] != float64(11) {
		t.Fatalf("payload = %v", payload)
	}
	logs, err := s.RecentNotifyDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Status != "sent" || logs[0].Channel != "webhook" {
		t.Fatalf("delivery log = %+v", logs)
	}
}

func TestNotifierWebhookFailureIsRecorded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(500)
	}))
	defer ts.Close()
	s := newNotifyTestStore(t)
	ctx := context.Background()
	if _, err := s.AddNotifyRule(ctx, store.NotifyRule{Name: "hook", WebhookURL: ts.URL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	n := NewNotifier(s)
	n.WebhookHTTP = ts.Client() // loopback test server, see above
	// A permanent 500 here would otherwise be retried with real backoff; the
	// retry path has its own test.
	n.RetryBackoff = -1
	if err := n.Dispatch(ctx, store.Message{ChatID: 5, MessageID: 1, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	logs, _ := s.RecentNotifyDeliveries(ctx, 10)
	if len(logs) != 1 || logs[0].Status != "failed" {
		t.Fatalf("delivery log = %+v", logs)
	}
}

// A rule stored before the write-time SSRF validation (or inserted by hand)
// must still not reach a private address: the production client refuses at
// dial time even though the scheme is valid.
func TestNotifierRefusesPrivateWebhookAtDial(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		rw.WriteHeader(200)
	}))
	defer ts.Close()
	s := newNotifyTestStore(t)
	ctx := context.Background()
	id, err := s.AddNotifyRule(ctx, store.NotifyRule{Name: "hook", WebhookURL: ts.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.NotifyRule(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	d := NewNotifier(s).DispatchTo(ctx, rule, store.Message{ChatID: 1, MessageID: 1, Text: "x"})
	if d.Status != "failed" {
		t.Fatalf("loopback webhook must be refused, got %+v", d)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("blocked webhook was reached %d times", hits)
	}
}

func TestNotifierRateLimitSkips(t *testing.T) {
	s := newNotifyTestStore(t)
	ctx := context.Background()
	if _, err := s.AddNotifyRule(ctx, store.NotifyRule{Name: "hook", WebhookURL: "http://127.0.0.1:1/hook", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	n := NewNotifier(s)
	if !n.allow(1) {
		t.Fatal("first delivery must be allowed")
	}
	for i := 0; i < notifyRateMax-1; i++ {
		if !n.allow(1) {
			t.Fatalf("hit %d must be allowed", i+2)
		}
	}
	if n.allow(1) {
		t.Fatal("delivery beyond the window budget must be denied")
	}
	if !n.allow(2) {
		t.Fatal("the budget is per rule")
	}
}

func TestDispatchAsyncSkipsSilentlyWhenNoRules(t *testing.T) {
	s := newNotifyTestStore(t)
	NewNotifier(s).DispatchAsync(store.Message{ChatID: 1, Text: "x"}) // must not panic or block
}

func TestDispatchToRecordsOutcome(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer ts.Close()
	s := newNotifyTestStore(t)
	ctx := context.Background()
	id, err := s.AddNotifyRule(ctx, store.NotifyRule{Name: "hook", WebhookURL: ts.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.NotifyRule(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	n := NewNotifier(s)
	n.WebhookHTTP = ts.Client() // loopback test server, see above
	d := n.DispatchTo(ctx, rule, store.Message{ChatID: 3, MessageID: 9, Text: "test"})
	if d.Status != "sent" || d.RuleID != id || d.MessageID != 9 {
		t.Fatalf("delivery = %+v", d)
	}
}

// Telegram counts characters, not bytes. A byte-truncated Chinese preview ends
// mid-rune and the Bot API rejects the whole message.
func TestNotifyPreviewTruncatesOnRuneBoundary(t *testing.T) {
	p := notifyPayload{Text: strings.Repeat("中", notifyTextLimit+50)}
	got := p.preview()
	if !utf8.ValidString(got) {
		t.Fatal("preview is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "…")); n != notifyTextLimit {
		t.Fatalf("kept %d runes, want %d", n, notifyTextLimit)
	}
}

// A network blip or a 5xx must not silently lose a notification: the same
// message is retried with backoff and the delivery log still records one row.
func TestNotifierRetriesTransientWebhookFailure(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rw.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := newNotifyTestStore(t)
	ctx := context.Background()
	if _, err := s.AddNotifyRule(ctx, store.NotifyRule{Name: "hook", WebhookURL: ts.URL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	n := NewNotifier(s)
	n.WebhookHTTP = ts.Client()
	n.RetryBackoff = time.Millisecond
	if err := n.Dispatch(ctx, store.Message{ChatID: 5, MessageID: 1, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("webhook hit %d times, want 3", got)
	}
	logs, _ := s.RecentNotifyDeliveries(ctx, 10)
	if len(logs) != 1 || logs[0].Status != "sent" {
		t.Fatalf("delivery log = %+v", logs)
	}
}

// A 4xx is not transient: retrying an auth failure or a bad payload only delays
// the failure and triples the requests.
func TestNotifierDoesNotRetryPermanentWebhookFailure(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		rw.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	s := newNotifyTestStore(t)
	ctx := context.Background()
	if _, err := s.AddNotifyRule(ctx, store.NotifyRule{Name: "hook", WebhookURL: ts.URL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	n := NewNotifier(s)
	n.WebhookHTTP = ts.Client()
	n.RetryBackoff = time.Millisecond
	_ = n.Dispatch(ctx, store.Message{ChatID: 5, MessageID: 1, Text: "x"})
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("permanent failure hit %d times, want 1", got)
	}
	logs, _ := s.RecentNotifyDeliveries(ctx, 10)
	if len(logs) != 1 || logs[0].Status != "failed" {
		t.Fatalf("delivery log = %+v", logs)
	}
}
