package archive

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/iyear/tdl/archive/store"
)

func TestAutoClickSettingsAPI(t *testing.T) {
	// Unmanaged server: the outlet is read-only, writes must be refused.
	s, ts := newNotifyWebServer(t, false)
	resp, err := http.Get(ts.URL + "/api/autoclick")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Configured string `json:"configured"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Configured != "false" {
		t.Fatalf("fresh outlet should be unconfigured, got %q", st.Configured)
	}
	if resp = postJSON(t, ts.URL+"/api/autoclick", map[string]any{"bot_token": "1:x", "bot_chat_id": "-100"}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("write without controller should be 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Managed server: save, mask, and keep the stored token on empty submit.
	s2, ts2 := newNotifyWebServer(t, true)
	_ = s
	resp = postJSON(t, ts2.URL+"/api/autoclick", map[string]any{"bot_token": "123456789:AA-secret-token", "bot_chat_id": "-100200"})
	if resp.StatusCode != 200 {
		t.Fatalf("save status %d", resp.StatusCode)
	}
	resp.Body.Close()

	got, err := http.Get(ts2.URL + "/api/autoclick")
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		BotTokenMasked string `json:"bot_token_masked"`
		BotChatID      string `json:"bot_chat_id"`
		Configured     string `json:"configured"`
	}
	_ = json.NewDecoder(got.Body).Decode(&view)
	got.Body.Close()
	if view.BotTokenMasked != "123456789:***ken" {
		t.Fatalf("token must be masked, got %q", view.BotTokenMasked)
	}
	if view.BotChatID != "-100200" || view.Configured != "true" {
		t.Fatalf("unexpected view: %+v", view)
	}

	// An empty token resubmission keeps the stored one.
	resp = postJSON(t, ts2.URL+"/api/autoclick", map[string]any{"bot_token": "", "bot_chat_id": "-100200"})
	resp.Body.Close()
	o, _ := s2.AutoClickOutlet(t.Context())
	if o.BotToken != "123456789:AA-secret-token" {
		t.Fatalf("empty token submit should keep stored token, got %q", o.BotToken)
	}

	// Malformed chat id is rejected; a malformed token too.
	if resp = postJSON(t, ts2.URL+"/api/autoclick", map[string]any{"bot_token": "t", "bot_chat_id": "abc"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad chat id should be 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if resp = postJSON(t, ts2.URL+"/api/autoclick", map[string]any{"bot_token": "no-colon", "bot_chat_id": "-100200"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad token shape should be 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAutoClickTestEndpoint(t *testing.T) {
	s, ts := newNotifyWebServer(t, true)

	// Nothing configured and no overrides: the endpoint refuses instead of
	// firing into the void.
	if resp := postJSON(t, ts.URL+"/api/autoclick/test", map[string]any{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfigured outlet should be 400, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Body overrides let the user test values before saving them.
	orig := newOutletNotifier
	newOutletNotifier = func(*store.Store) *Notifier {
		return &Notifier{HTTP: &http.Client{Transport: stubTelegramTransport{}}}
	}
	t.Cleanup(func() { newOutletNotifier = orig })

	resp := postJSON(t, ts.URL+"/api/autoclick/test", map[string]any{
		"bot_token": "123456789:AA-secret-token", "bot_chat_id": "-100200",
	})
	var d struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&d)
	resp.Body.Close()
	if resp.StatusCode != 200 || d.Status != "sent" {
		t.Fatalf("override test should be sent, got %d %+v", resp.StatusCode, d)
	}

	// With the outlet stored, an empty body tests the saved values.
	if err := s.SaveAutoClickOutlet(t.Context(), store.AutoClickOutlet{BotToken: "123456789:AA-secret-token", BotChatID: "-100200"}); err != nil {
		t.Fatal(err)
	}
	resp = postJSON(t, ts.URL+"/api/autoclick/test", map[string]any{})
	_ = json.NewDecoder(resp.Body).Decode(&d)
	resp.Body.Close()
	if resp.StatusCode != 200 || d.Status != "sent" {
		t.Fatalf("stored outlet test should be sent, got %d %+v", resp.StatusCode, d)
	}
}

func TestAutoClickLogAPI(t *testing.T) {
	s, ts := newNotifyWebServer(t, false)
	ctx := t.Context()
	if err := s.AddAutoClickLog(ctx, store.AutoClickLogEntry{
		ChatID: 42, MessageID: 7, ButtonText: "领取", Kind: "callback", Status: "clicked",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := http.Get(ts.URL + "/api/autoclick/log?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if got.StatusCode != 200 {
		t.Fatalf("log status %d", got.StatusCode)
	}
	var d struct {
		Items []store.AutoClickLogEntry `json:"items"`
	}
	_ = json.NewDecoder(got.Body).Decode(&d)
	if len(d.Items) != 1 || d.Items[0].ChatID != 42 || d.Items[0].MessageID != 7 {
		t.Fatalf("unexpected log payload: %+v", d.Items)
	}
	if d.Items[0].ButtonText != "领取" || d.Items[0].Status != "clicked" {
		t.Fatalf("entry fields lost: %+v", d.Items[0])
	}
}
