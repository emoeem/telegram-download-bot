package archive

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
)

// Web surface for the receive ledger. Three endpoints, each a thin projection of
// a store primitive so the CLI and the UI cannot disagree:
//
//	GET  /api/serials        the numbered ledger
//	GET  /api/serial         one number, resolved and rendered
//	POST /api/purge          delete by number / chat / date (confirm to execute)

func webArchiveRoot() string { return filepath.Join(consts.DataDir, "archive") }

// root resolves the data root for this server: explicit when configured,
// otherwise the default archive location.
func (w *WebServer) root() string {
	if w.Root != "" {
		return w.Root
	}
	return webArchiveRoot()
}

func (w *WebServer) serials(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	kind := strings.TrimSpace(q.Get("kind"))
	status := strings.TrimSpace(q.Get("status"))
	all := q.Get("all") == "1" || q.Get("all") == "true"
	maxSerial, _ := strconv.ParseInt(q.Get("max_serial"), 10, 64)
	var since *time.Time
	if v := strings.TrimSpace(q.Get("since")); v != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				since = &t
				break
			}
		}
	}
	items, err := w.store().Serials(r.Context(), store.SerialFilter{
		Limit: limit, Kind: kind, Status: status, InboundOnly: !all, MaxSerial: maxSerial, Since: since,
	})
	if err != nil {
		w.serverError(rw, err)
		return
	}
	sum, err := w.serialSummary(r.Context())
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, map[string]any{
		"summary": sum,
		"items":   items,
		"view":    SerialViewRoot(w.root()),
	})
}

func (w *WebServer) serialDetail(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	serial, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Query().Get("serial"), "#"), 10, 64)
	if serial <= 0 {
		http.Error(rw, "serial is required", http.StatusBadRequest)
		return
	}
	p, ok, err := LoadSerialPost(r.Context(), w.store(), w.root(), serial)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	if !ok {
		http.Error(rw, "serial not found", http.StatusNotFound)
		return
	}
	rendered, err := RenderSerialPost(p, r.URL.Query().Get("template"))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(rw, map[string]any{"item": p, "post": rendered})
}

// purgeRequest is the body of POST /api/purge. confirm=true is the only way to
// delete; without it the same request returns the dry-run report, which makes
// the endpoint safe to call from a form preview.
type purgeRequest struct {
	Serial       int64  `json:"serial"`
	Chat         int64  `json:"chat"`
	Until        string `json:"until"`
	Kind         string `json:"kind"`
	KeepMessages bool   `json:"keep_messages"`
	Confirm      bool   `json:"confirm"`
	// All wipes the whole archive. It is a separate field from the selectors
	// because "delete everything" must never be what happens when a selector
	// is left blank by mistake.
	All bool `json:"all"`
	// ConfirmText must read exactly "清空" when All is set. A boolean alone is
	// one stray click away from a wipe; typing the word is not.
	ConfirmText string `json:"confirm_text"`
}

// purgeConfirmWord is the literal a caller has to type to authorise a full
// wipe. It is a constant so the CLI and the UI cannot drift apart.
const purgeConfirmWord = "清空"

func (w *WebServer) purge(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Require JSON explicitly. Without this, a cross-site form can post a
	// text/plain body containing JSON — a simple request, so no CORS preflight
	// runs — and delete content. Demanding application/json forces a preflight
	// that a same-origin-only server never answers.
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		http.Error(rw, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req purgeRequest
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(rw, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sel := store.PurgeSelector{
		Serial:   req.Serial,
		ChatID:   req.Chat,
		Kind:     strings.TrimSpace(req.Kind),
		Messages: !req.KeepMessages,
		DryRun:   !req.Confirm,
	}
	if req.All {
		// The word is only demanded for the destructive call. A dry run must
		// stay open, otherwise the UI cannot tell the operator how much it is
		// about to delete before they decide to type it.
		if req.Confirm && strings.TrimSpace(req.ConfirmText) != purgeConfirmWord {
			http.Error(rw, `full wipe requires "confirm_text":"`+purgeConfirmWord+`"`, http.StatusBadRequest)
			return
		}
		sel.All = true
		sel.Messages = true
	}
	if v := strings.TrimSpace(req.Until); v != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				sel.Until = t
				break
			}
		}
	}
	rep, err := w.store().Purge(r.Context(), sel)
	if err != nil {
		// An empty selector is a client mistake, not a server fault.
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if !sel.DryRun {
		removed, freed := ApplyPurge(w.root(), rep)
		rep.Files = nil
		writeJSON(rw, map[string]any{
			"report":        rep,
			"files_removed": removed,
			"bytes_freed":   freed,
			"executed":      true,
		})
		return
	}
	writeJSON(rw, map[string]any{"report": rep, "executed": false})
}
