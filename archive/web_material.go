package archive

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/iyear/tdl/archive/store"
)

// Web surface for the material layer — the "tidied archive".
//
// The layer itself already existed (album folding plus cross-post dedupe), but
// it was reachable only from `tdl archive material` and the export endpoint, so
// from the browser the archive looked like an unsorted pile of raw messages.
// These two endpoints are the missing front door:
//
//	GET  /api/materials        the material ledger: one row per post or album,
//	                           with duplicates flagged and pointed at the original
//	POST /api/material/refresh  run a build pass (incremental by default)

type materialsAPIResponse struct {
	Items    []store.MaterialView `json:"items"`
	Total    int                  `json:"total"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
	Stats    store.MaterialStats  `json:"stats"`
	Kinds    []string             `json:"kinds"`
}

// materialKinds is the fixed vocabulary the UI renders as filter chips. Sorted
// so the chips never reshuffle between requests.
var materialKinds = []string{"photo", "video", "album", "animation", "sticker", "audio", "document", "text", "link"}

func (w *WebServer) materials(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	if page < 1 {
		page = 1
	}
	chatID, _ := strconv.ParseInt(strings.TrimSpace(q.Get("chat")), 10, 64)

	f := store.MaterialFilter{
		ChatID:   chatID,
		Kind:     strings.TrimSpace(q.Get("kind")),
		Query:    strings.TrimSpace(q.Get("q")),
		Status:   strings.TrimSpace(q.Get("status")),
		Page:     page,
		PageSize: pageSize,
	}
	// The default view is the tidied one: repeated posts are hidden behind their
	// original. "all" brings them back, "duplicate" shows only the repeats.
	if f.Status == "" {
		f.Status = "active"
	}

	items, err := w.store().ListMaterials(r.Context(), f)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	total, err := w.store().CountMaterials(r.Context(), f)
	if err != nil {
		w.serverError(rw, err)
		return
	}
	stats, err := w.store().MaterialStats(r.Context())
	if err != nil {
		w.serverError(rw, err)
		return
	}
	if items == nil {
		items = []store.MaterialView{}
	}
	writeJSON(rw, materialsAPIResponse{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
		Stats:    stats,
		Kinds:    materialKinds,
	})
}

type materialRefreshRequest struct {
	Rebuild bool  `json:"rebuild"`
	ChatID  int64 `json:"chat_id"`
	Dense   bool  `json:"dense"`
}

// materialRefresh rebuilds the layer. It touches only derived data, so it is
// safe to run while the viewer is serving, and safe to run twice.
func (w *WebServer) materialRefresh(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Same gate as the other mutating endpoints: a cross-site form can post
	// text/plain without a preflight, and that must not reach a rebuild.
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		http.Error(rw, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var req materialRefreshRequest
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<14)).Decode(&req); err != nil {
		http.Error(rw, "invalid JSON body", http.StatusBadRequest)
		return
	}

	mz := NewMaterializer(w.store())
	rep, err := mz.Build(r.Context(), BuildOptions{
		ChatID:  req.ChatID,
		Rebuild: req.Rebuild,
		// A rebuild must always recompute the duplicate pass; an incremental
		// refresh does it too, because a single new copy can turn a post that
		// used to be unique into somebody else's duplicate.
		Dense: req.Rebuild || req.Dense,
	})
	if err != nil {
		w.serverError(rw, err)
		return
	}
	writeJSON(rw, rep)
}
