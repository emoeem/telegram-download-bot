package archive

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/iyear/tdl/archive/store"
)

// autoClickSettings serves the dedicated delivery outlet for auto-click
// results. The Bot token is write-only: GET returns the masked form, POST
// with an empty token keeps the stored one, mirroring notify rule handling.
func (w *WebServer) autoClickSettings(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		o, err := w.store().AutoClickOutlet(r.Context())
		if err != nil {
			w.serverError(rw, err)
			return
		}
		writeJSON(rw, map[string]string{
			"bot_token_masked": maskBotToken(o.BotToken),
			"bot_chat_id":      o.BotChatID,
			"configured":       strconv.FormatBool(o.BotToken != "" && o.BotChatID != ""),
		})
	case http.MethodPost:
		if !w.requireCtl(rw) {
			return
		}
		var req struct {
			BotToken  string `json:"bot_token"`
			BotChatID string `json:"bot_chat_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		req.BotChatID = strings.TrimSpace(req.BotChatID)
		if req.BotChatID != "" {
			if _, err := strconv.ParseInt(req.BotChatID, 10, 64); err != nil {
				http.Error(rw, "bot_chat_id 必须是数字 chat id（频道为 -100 开头，私聊为正数）", http.StatusBadRequest)
				return
			}
		}
		if req.BotToken == "" {
			// keep the stored token so the UI can resubmit the masked form
			o, err := w.store().AutoClickOutlet(r.Context())
			if err == nil {
				req.BotToken = o.BotToken
			}
		}
		if req.BotToken != "" && !validBotTokenShape(req.BotToken) {
			http.Error(rw, "bot token 形如 123456:ABC-DEF…，请检查后重试", http.StatusBadRequest)
			return
		}
		if req.BotChatID == "" {
			// clearing the chat id clears the outlet entirely
			req.BotToken = ""
		}
		if err := w.store().SaveAutoClickOutlet(r.Context(), store.AutoClickOutlet{BotToken: req.BotToken, BotChatID: req.BotChatID}); err != nil {
			w.serverError(rw, err)
			return
		}
		writeJSON(rw, map[string]string{"status": "saved"})
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// newOutletNotifier is a test seam: outlet delivery runs through a Notifier so
// tests can stub the Bot API transport.
var newOutletNotifier = NewNotifier

// autoClickTest sends one synthetic message through the outlet so the user can
// verify the Bot Token / chat_id without waiting for a real click. Values in
// the request body override the stored outlet without saving them.
func (w *WebServer) autoClickTest(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	var req struct {
		BotToken  string `json:"bot_token"`
		BotChatID string `json:"bot_chat_id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	o, err := w.store().AutoClickOutlet(r.Context())
	if err != nil {
		w.serverError(rw, err)
		return
	}
	if req.BotToken == "" {
		req.BotToken = o.BotToken
	}
	if req.BotChatID == "" {
		req.BotChatID = o.BotChatID
	}
	if req.BotToken == "" || req.BotChatID == "" {
		http.Error(rw, "请先配置 Bot Token 和 chat_id", http.StatusBadRequest)
		return
	}
	n := newOutletNotifier(w.store())
	ctx, cancel := context.WithTimeout(r.Context(), notifyDispatchTimeout)
	defer cancel()
	err = n.sendTelegram(ctx, store.NotifyRule{BotToken: req.BotToken, BotChatID: req.BotChatID},
		"🔔 自动点击出口测试：收到这条消息说明 Bot Token 与 chat_id 配置正确。")
	if err != nil {
		writeJSON(rw, map[string]string{"status": "failed", "error": err.Error()})
		return
	}
	writeJSON(rw, map[string]string{"status": "sent"})
}

// autoClickLog lists the most recent click attempts and outcomes.
func (w *WebServer) autoClickLog(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	log, err := w.store().ListAutoClickLog(r.Context(), limit)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	if log == nil {
		log = []store.AutoClickLogEntry{}
	}
	writeJSON(rw, map[string][]store.AutoClickLogEntry{"items": log})
}

// validBotTokenShape rejects obviously malformed tokens early; the real check
// happens when the Bot API answers a delivery.
func validBotTokenShape(t string) bool {
	colon := -1
	for i, c := range t {
		if c == ':' {
			colon = i
			break
		}
	}
	return colon > 0 && colon < len(t)-1
}

// autoClickClick presses one inline callback button from the web UI. This is
// the gap between "I can see the button" and "clicking it does something": the
// archived button is display-only, so the click has to be replayed by the user
// account, and the bot's answer is archived like any other message.
func (w *WebServer) autoClickClick(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	var req struct {
		RowID int64 `json:"rowid"`
		Index int   `json:"index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if req.RowID <= 0 {
		http.Error(rw, "rowid is required", http.StatusBadRequest)
		return
	}
	v, err := w.store().GetMessageByRowID(r.Context(), req.RowID)
	if err != nil {
		writeJSONErr(rw, http.StatusNotFound, "这条消息不在库里")
		return
	}
	rt, col := w.ctl().runtimeForTask()
	if rt == nil || col == nil || col.AutoClick == nil {
		writeJSONErr(rw, http.StatusServiceUnavailable, "会话未连接或未登录：先去「登录」页登录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), autoclickTimeout)
	defer cancel()
	out, err := col.AutoClick.ClickOne(ctx, store.Message{
		ChatID: v.ChatID, MessageID: v.MessageID,
		Text: v.Text, Caption: v.Caption,
		HasMedia: v.MediaSHA256 != "", MediaMime: v.MediaMime,
	}, req.Index)
	if err != nil {
		writeJSONErr(rw, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(rw, map[string]string{"status": "clicked", "message": out})
}

// writeJSONErr returns a JSON error so the page can show the reason instead of
// an opaque "failed".
func writeJSONErr(rw http.ResponseWriter, code int, msg string) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(map[string]string{"error": msg})
}
