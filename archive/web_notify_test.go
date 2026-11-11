package archive

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyear/tdl/archive/store"
)

func newNotifyWebServer(t *testing.T, managed bool) (*store.Store, *httptest.Server) {
	t.Helper()
	var ctl *Controller
	if managed {
		ctl = &Controller{jobs: map[int64]*Job{}}
	}
	s, ts := newControlTestServer(t, ctl, WebAuthConfig{})
	return s, ts
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNotifyRulesCRUD(t *testing.T) {
	s, ts := newNotifyWebServer(t, true)

	// Create.
	resp := postJSON(t, ts.URL+"/api/notify/rules", map[string]any{
		"name": "ops", "keywords": []string{"down"}, "webhook_url": "https://example.com/hook",
		"bot_token": "12345:AA-secret-token", "bot_chat_id": "-100200",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("create status %d", resp.StatusCode)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == 0 {
		t.Fatal("rule id missing")
	}

	// List must mask the bot token.
	got, err := http.Get(ts.URL + "/api/notify/rules")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Rules []notifyRuleJSON `json:"rules"`
	}
	_ = json.NewDecoder(got.Body).Decode(&list)
	got.Body.Close()
	if len(list.Rules) != 1 {
		t.Fatalf("rules = %+v", list.Rules)
	}
	r := list.Rules[0]
	if strings.Contains(r.BotTokenMasked, "AA-secret") {
		t.Fatalf("bot token leaked: %q", r.BotTokenMasked)
	}
	if r.BotTokenMasked == "" {
		t.Fatal("masked token missing")
	}

	// Update keywords.
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/notify/rules",
		strings.NewReader(`{"id":1,"keywords":["down","panic"],"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	up, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if up.StatusCode != 200 {
		t.Fatalf("patch status %d", up.StatusCode)
	}
	up.Body.Close()
	rules, _ := s.NotifyRules(t.Context())
	if len(rules) != 1 || len(rules[0].Keywords) != 2 || rules[0].Enabled {
		t.Fatalf("rules after patch = %+v", rules)
	}
	// Credentials must survive an edit that does not mention them.
	if rules[0].BotToken != "12345:AA-secret-token" || rules[0].WebhookURL != "https://example.com/hook" {
		t.Fatalf("credentials were wiped: %+v", rules[0])
	}

	// Delete.
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/notify/rules?id=1", nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	rules, _ = s.NotifyRules(t.Context())
	if len(rules) != 0 {
		t.Fatalf("rules after delete = %+v", rules)
	}
}

func TestNotifyRulesUnmanaged(t *testing.T) {
	_, ts := newNotifyWebServer(t, false)
	resp := postJSON(t, ts.URL+"/api/notify/rules", map[string]any{"name": "x", "webhook_url": "https://a.example"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unmanaged create status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestNotifyRulesValidation(t *testing.T) {
	_, ts := newNotifyWebServer(t, true)
	cases := []map[string]any{
		{"name": "x"},                            // no outlet
		{"name": "", "webhook_url": "https://a"}, // no name
		{"name": "x", "webhook_url": "ftp://a"},  // bad scheme
		{"name": "x", "bot_token": "1:a"},        // token without chat id
	}
	for i, c := range cases {
		resp := postJSON(t, ts.URL+"/api/notify/rules", c)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("case %d: expected 400, got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestNotifyTestEndpoint(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits++
	}))
	defer ts.Close()

	// The webhook target is on loopback, so the server needs the test client:
	// the default one refuses private destinations by design (SSRF guard).
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	ws := NewManagedWebServer(s, &Controller{jobs: map[int64]*Job{}}, WebAuthConfig{})
	ws.NotifyHTTP = ts.Client()
	web := httptest.NewServer(ws.Handler())
	t.Cleanup(func() { web.Close(); s.Close() })

	if _, err := s.AddNotifyRule(t.Context(), store.NotifyRule{Name: "ops", WebhookURL: ts.URL, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	resp := postJSON(t, web.URL+"/api/notify/test", map[string]any{"id": 1})
	var out struct {
		Status  string `json:"status"`
		Channel string `json:"channel"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Status != "sent" || out.Channel != "webhook" {
		t.Fatalf("test delivery = %+v", out)
	}
	if hits != 1 {
		t.Fatalf("webhook hit %d times", hits)
	}
	// Unknown rule id -> 404.
	resp = postJSON(t, web.URL+"/api/notify/test", map[string]any{"id": 99})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing rule status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Webhooks are a server-side request primitive: a rule pointing at loopback,
// RFC1918, link-local metadata or a non-http scheme must be rejected at write
// time so it can never be stored (and later delivered by a background loop).
func TestValidNotifyWebhookRejectsInternalTargets(t *testing.T) {
	bad := []string{
		"http://127.0.0.1/hook",
		"http://127.0.0.1:8080/hook",
		"http://localhost/hook",
		"http://LOCALHOST:9000/hook",
		"http://[::1]/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.100.100.200/latest/meta-data/",
		"http://10.1.2.3/hook",
		"http://172.16.0.1/hook",
		"http://192.168.1.10/hook",
		"http://0.0.0.0/hook",
		"http://[::ffff:127.0.0.1]/hook",
		"ftp://example.com/hook",
		"file:///etc/passwd",
		"https://user:pass@example.com/hook",
		"http:///no-host",
	}
	for _, u := range bad {
		if ValidNotifyWebhook(u) {
			t.Errorf("ValidNotifyWebhook(%q) = true, want false", u)
		}
	}
	good := []string{
		"",
		"https://example.com/hook",
		"https://hooks.slack.com/services/T000/B000/XXXX",
		"http://203.0.113.10/hook",
	}
	for _, u := range good {
		if !ValidNotifyWebhook(u) {
			t.Errorf("ValidNotifyWebhook(%q) = false, want true", u)
		}
	}
}
