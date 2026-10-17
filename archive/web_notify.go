package archive

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// notifyRuleJSON is the API view of a notify rule. The bot token is write-only:
// the browser never receives it back, only a masked form for display.
type notifyRuleJSON struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Keywords        []string `json:"keywords"`
	ExcludeKeywords []string `json:"exclude_keywords"`
	ChatIDs         []int64  `json:"chat_ids"`
	WebhookURL      string   `json:"webhook_url"`
	BotChatID       string   `json:"bot_chat_id"`
	BotTokenMasked  string   `json:"bot_token_masked,omitempty"`
	Enabled         bool     `json:"enabled"`
}

func maskBotToken(t string) string {
	if t == "" {
		return ""
	}
	// Bot tokens read like "<bot-id>:<secret>"; show the bot id and the last
	// three characters so two rules can be told apart without leaking secrets.
	if i := strings.IndexByte(t, ':'); i > 0 && len(t) > i+4 {
		return t[:i] + ":***" + t[len(t)-3:]
	}
	return "***"
}

func toNotifyRuleJSON(r store.NotifyRule) notifyRuleJSON {
	return notifyRuleJSON{
		ID: r.ID, Name: r.Name,
		Keywords: orEmpty(r.Keywords), ExcludeKeywords: orEmpty(r.ExcludeKeywords), ChatIDs: orEmptyInt64(r.ChatIDs),
		WebhookURL: r.WebhookURL, BotChatID: r.BotChatID,
		BotTokenMasked: maskBotToken(r.BotToken), Enabled: r.Enabled,
	}
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func orEmptyInt64(v []int64) []int64 {
	if v == nil {
		return []int64{}
	}
	return v
}

// notifyRules handles GET (list rules + recent deliveries), POST (create),
// PATCH (update mutable fields) and DELETE (?id=) for notification rules.
func (w *WebServer) notifyRules(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rs, err := w.Store.NotifyRules(r.Context())
		if err != nil {
			w.serverError(rw, err)
			return
		}
		out := make([]notifyRuleJSON, 0, len(rs))
		for _, r := range rs {
			out = append(out, toNotifyRuleJSON(r))
		}
		logs, _ := w.Store.RecentNotifyDeliveries(r.Context(), 30)
		if logs == nil {
			logs = []store.NotifyDelivery{}
		}
		writeJSON(rw, map[string]any{"rules": out, "log": logs})
	case http.MethodPost:
		if !w.requireCtl(rw) {
			return
		}
		var req struct {
			Name            string   `json:"name"`
			Keywords        []string `json:"keywords"`
			ExcludeKeywords []string `json:"exclude_keywords"`
			ChatIDs         []int64  `json:"chat_ids"`
			WebhookURL      string   `json:"webhook_url"`
			BotToken        string   `json:"bot_token"`
			BotChatID       string   `json:"bot_chat_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		req.WebhookURL = strings.TrimSpace(req.WebhookURL)
		req.BotToken = strings.TrimSpace(req.BotToken)
		req.BotChatID = strings.TrimSpace(req.BotChatID)
		if req.Name == "" {
			http.Error(rw, "name is required", http.StatusBadRequest)
			return
		}
		if req.WebhookURL == "" && (req.BotToken == "" || req.BotChatID == "") {
			http.Error(rw, "webhook_url or bot_token+bot_chat_id is required", http.StatusBadRequest)
			return
		}
		if !validNotifyWebhook(req.WebhookURL) {
			http.Error(rw, "webhook_url must be http(s)", http.StatusBadRequest)
			return
		}
		id, err := w.Store.AddNotifyRule(r.Context(), store.NotifyRule{
			Name: req.Name, Keywords: req.Keywords, ExcludeKeywords: req.ExcludeKeywords, ChatIDs: req.ChatIDs,
			WebhookURL: req.WebhookURL, BotToken: req.BotToken, BotChatID: req.BotChatID, Enabled: true,
		})
		if err != nil {
			w.serverError(rw, err)
			return
		}
		writeJSON(rw, map[string]int64{"id": id})
	case http.MethodPatch:
		if !w.requireCtl(rw) {
			return
		}
		var req struct {
			ID              int64     `json:"id"`
			Name            string    `json:"name"`
			Keywords        *[]string `json:"keywords"`
			ExcludeKeywords *[]string `json:"exclude_keywords"`
			ChatIDs         *[]int64  `json:"chat_ids"`
			Enabled         *bool     `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == 0 {
			http.Error(rw, "id is required", http.StatusBadRequest)
			return
		}
		// The enabled flag participates in every update, so only honor it when
		// explicitly sent.
		nr := store.NotifyRule{Name: req.Name}
		if req.Enabled != nil {
			nr.Enabled = *req.Enabled
		} else {
			cur, err := w.Store.NotifyRule(r.Context(), req.ID)
			if err != nil {
				http.Error(rw, err.Error(), http.StatusNotFound)
				return
			}
			nr.Enabled = cur.Enabled
		}
		if req.Keywords != nil {
			nr.Keywords = *req.Keywords
		}
		if req.ExcludeKeywords != nil {
			nr.ExcludeKeywords = *req.ExcludeKeywords
		}
		if req.ChatIDs != nil {
			nr.ChatIDs = *req.ChatIDs
		}
		if err := w.Store.UpdateNotifyRule(r.Context(), req.ID, nr); err != nil {
			http.Error(rw, err.Error(), http.StatusNotFound)
			return
		}
		cur, err := w.Store.NotifyRule(r.Context(), req.ID)
		if err != nil {
			w.serverError(rw, err)
			return
		}
		writeJSON(rw, toNotifyRuleJSON(cur))
	case http.MethodDelete:
		if !w.requireCtl(rw) {
			return
		}
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id == 0 {
			http.Error(rw, "id is required", http.StatusBadRequest)
			return
		}
		if err := w.Store.RemoveNotifyRule(r.Context(), id); err != nil {
			w.serverError(rw, err)
			return
		}
		writeJSON(rw, map[string]bool{"ok": true})
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// validNotifyWebhook restricts webhooks to http(s) so a crafted rule cannot
// make the server POST into exotic schemes (file:, gopher:, ...).
func validNotifyWebhook(u string) bool {
	if u == "" {
		return true
	}
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// notifyTest delivers a synthetic message through one rule and reports the
// outcome, so the UI can validate outlets without waiting for a real match.
func (w *WebServer) notifyTest(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	var req struct {
		ID   int64  `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == 0 {
		http.Error(rw, "id is required", http.StatusBadRequest)
		return
	}
	rule, err := w.Store.NotifyRule(r.Context(), req.ID)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusNotFound)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		req.Text = "tdl archive notify test: 如果收到这条消息，说明规则 " + rule.Name + " 的通知链路是通的。"
	}
	m := store.Message{MessageID: 0, Date: time.Now(), Text: req.Text}
	d := NewNotifier(w.Store).DispatchTo(r.Context(), rule, m)
	writeJSON(rw, map[string]any{"status": d.Status, "channel": d.Channel, "error": d.Error})
}
