package archive

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// messageDelete removes archived messages one row at a time.
//
// Three modes, because "delete" means three different things to an operator and
// conflating them is how archives lose data by accident:
//
//	soft    hide it locally, Telegram and the bytes stay
//	hard    drop the row, its media and the object files nobody else references
//	restore undo a soft delete
//
// The row id is the archive's own identity. Accepting chat_id+message_id would
// force every caller to hold a pair it may have already thrown away.
func (w *WebServer) messageDelete(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	// Same reasoning as purge: a cross-site form can post text/plain without a
	// preflight, so the JSON content type is the thing that forces one.
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		http.Error(rw, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req struct {
		RowID int64  `json:"rowid"`
		Mode  string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(rw, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RowID <= 0 {
		http.Error(rw, "rowid is required", http.StatusBadRequest)
		return
	}
	mode := strings.TrimSpace(strings.ToLower(req.Mode))
	if mode == "" {
		mode = "soft"
	}

	switch mode {
	case "soft":
		n, err := w.store().SoftDeleteRowIDs(r.Context(), []int64{req.RowID})
		if err != nil {
			w.serverError(rw, err)
			return
		}
		if n == 0 {
			writeJSONErr(rw, http.StatusNotFound, "这条消息不在库里")
			return
		}
		writeJSON(rw, map[string]any{"status": "deleted", "mode": "soft", "rows": n})
	case "restore":
		n, err := w.store().RestoreRowIDs(r.Context(), []int64{req.RowID})
		if err != nil {
			w.serverError(rw, err)
			return
		}
		if n == 0 {
			writeJSONErr(rw, http.StatusNotFound, "这条消息不在库里")
			return
		}
		writeJSON(rw, map[string]any{"status": "restored", "rows": n})
	case "hard":
		rep, err := w.store().DeleteMessageHard(r.Context(), req.RowID)
		if err != nil {
			w.serverError(rw, err)
			return
		}
		removed, freed := ApplyPurge(w.root(), rep)
		rep.Files = nil
		writeJSON(rw, map[string]any{
			"status":        "deleted",
			"mode":          "hard",
			"report":        rep,
			"files_removed": removed,
			"bytes_freed":   freed,
		})
	default:
		http.Error(rw, "mode must be soft, hard or restore", http.StatusBadRequest)
	}
}

// autoClickScan queues the buttoned messages already sitting in the archive.
//
// Realtime clicking handles everything arriving after a source is enabled; this
// is the catch-up for history archived before. It only fills the persisted
// queue, so the worker keeps its per-chat pacing and a restart does not lose the
// backlog.
func (w *WebServer) autoClickScan(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !w.requireCtl(rw) {
		return
	}
	var req struct {
		ChatID  int64 `json:"chat_id"`
		Limit   int   `json:"limit"`
		Recent  bool  `json:"include_recent"`
		AllChat bool  `json:"all_chats"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<14)).Decode(&req); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rt, col := w.ctl().runtimeForTask()
	if rt == nil || col == nil || col.AutoClick == nil {
		writeJSONErr(rw, http.StatusServiceUnavailable, "会话未连接或未登录：先去「登录」页登录")
		return
	}
	if req.Limit <= 0 || req.Limit > 500 {
		req.Limit = 50
	}
	targets, err := w.store().Targets(r.Context())
	if err != nil {
		w.serverError(rw, err)
		return
	}
	queued, chats := 0, 0
	for _, t := range targets {
		if req.ChatID != 0 && t.ChatID != req.ChatID {
			continue
		}
		n, err := col.AutoClick.ScanChat(r.Context(), t.ChatID, req.Limit, req.Recent)
		if err != nil {
			slog.Default().Warn("autoclick scan failed", slog.Int64("chat", t.ChatID), slog.String("error", err.Error()))
			continue
		}
		queued += n
		chats++
	}
	writeJSON(rw, map[string]any{"status": "queued", "queued": queued, "chats": chats})
}
