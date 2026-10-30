package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// exportRowLimit caps a single export so a huge archive cannot be streamed into
// a browser tab by accident.
const exportRowLimit = 50000

// exportPageSize is the page size used to walk the store while exporting.
const exportPageSize = 500

// ExportSource describes one exportable view: its CSV header and a paged fetch
// that returns rows aligned with that header.
type ExportSource struct {
	header []string
	fetch  func(page int) ([][]string, error)
}

// export streams the current view as CSV or JSON. JSON rows use the same field
// names as the CSV header, so both formats are interchangeable for scripts.
func (w *WebServer) export(rw http.ResponseWriter, r *http.Request) {
	if !getOnly(rw, r) {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}
	query := r.URL.Query().Get("q")
	mediaKind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("media_kind")))
	domain := r.URL.Query().Get("domain")
	chat, _ := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	topic, _ := strconv.ParseInt(r.URL.Query().Get("topic"), 10, 64)

	// Column reshaping is applied before streaming so an unknown field name
	// fails the request instead of producing a file the importer accepts with
	// every value in the wrong column.
	spec, err := ParseFieldSpec(r.URL.Query().Get("fields"))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if rawMap := r.URL.Query().Get("map"); rawMap != "" {
		var m map[string]string
		if err := json.Unmarshal([]byte(rawMap), &m); err != nil {
			http.Error(rw, "map must be a JSON object of {source: output}: "+err.Error(), http.StatusBadRequest)
			return
		}
		spec = MergeRenames(spec, m)
	}

	src, err := BuildExportSource(r.Context(), w.store(), kind, query, mediaKind, domain, chat, topic)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	src, err = ReshapeSource(src, spec)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	name := fmt.Sprintf("archive-%s-%s.%s", kind, time.Now().Format("20060102-150405"), format)
	rw.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	switch format {
	case "csv":
		rw.Header().Set("Content-Type", "text/csv; charset=utf-8")
		_ = WriteCSV(rw, src)
	case "json":
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = WriteJSON(rw, src)
	case "jsonl", "ndjson":
		rw.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		_ = WriteJSONL(rw, src)
	default:
		http.Error(rw, "format must be csv, json or jsonl", http.StatusBadRequest)
	}
}

// ExportSource resolves the requested view into a header plus paged fetcher.
// buildExportSource resolves a view name into a header plus a paged fetcher.
//
// It takes the store rather than a WebServer so the CLI drives exactly the same
// sources: a web-only export would drift from the documented one the moment
// either side gains a column.
func BuildExportSource(ctx context.Context, st *store.Store, kind, query, mediaKind, domain string, chat, topic int64) (ExportSource, error) {
	messageHeader := []string{"row_id", "chat_id", "message_id", "date", "sender", "username", "text", "media_sha256", "media_mime", "deleted", "pinned"}
	viewRows := func(views []store.MessageView) [][]string {
		out := make([][]string, 0, len(views))
		for _, v := range views {
			out = append(out, []string{
				strconv.FormatInt(v.RowID, 10),
				strconv.FormatInt(v.ChatID, 10),
				strconv.Itoa(v.MessageID),
				time.Unix(v.Date, 0).Format(time.RFC3339),
				v.SenderName,
				v.SenderUsername,
				v.Text,
				v.MediaSHA256,
				v.MediaMime,
				strconv.FormatBool(v.Deleted),
				strconv.FormatBool(v.Pinned),
			})
		}
		return out
	}

	switch kind {
	case "messages":
		return ExportSource{header: messageHeader, fetch: func(page int) ([][]string, error) {
			p, err := st.ListMessages(ctx, store.MessageFilter{Query: query, ChatID: chat, Topic: topic, Page: page, PageSize: exportPageSize})
			if err != nil {
				return nil, err
			}
			return viewRows(p.Items), nil
		}}, nil
	case "deleted":
		return ExportSource{header: messageHeader, fetch: func(page int) ([][]string, error) {
			p, err := st.ListDeletedMessages(ctx, store.DeletedFilter{Query: query, ChatID: chat, Page: page, PageSize: exportPageSize})
			if err != nil {
				return nil, err
			}
			return viewRows(p.Items), nil
		}}, nil
	case "edited":
		header := append(append([]string{}, messageHeader...), "edit_count")
		return ExportSource{header: header, fetch: func(page int) ([][]string, error) {
			p, err := st.ListEditedMessages(ctx, store.EditedFilter{Query: query, ChatID: chat, Page: page, PageSize: exportPageSize})
			if err != nil {
				return nil, err
			}
			views := make([]store.MessageView, 0, len(p.Items))
			for _, it := range p.Items {
				views = append(views, it.MessageView)
			}
			rows := viewRows(views)
			for i, it := range p.Items {
				rows[i] = append(rows[i], strconv.Itoa(it.EditCount))
			}
			return rows, nil
		}}, nil
	case "media":
		mk := store.MediaKind(mediaKind)
		if mk == "" {
			mk = store.MediaKindImage
		}
		header := append(append([]string{}, messageHeader...), "media_status")
		return ExportSource{header: header, fetch: func(page int) ([][]string, error) {
			p, err := st.ListMediaMessages(ctx, store.MediaFilter{Kind: mk, Query: query, ChatID: chat, Topic: topic, Page: page, PageSize: exportPageSize})
			if err != nil {
				return nil, err
			}
			views := make([]store.MessageView, 0, len(p.Items))
			for _, it := range p.Items {
				views = append(views, it.MessageView)
			}
			rows := viewRows(views)
			for i, it := range p.Items {
				rows[i] = append(rows[i], it.MediaStatus)
			}
			return rows, nil
		}}, nil
	case "links":
		header := []string{"url", "domain", "chat_id", "chat_title", "message_id", "message_row_id", "date", "sender", "username", "text"}
		return ExportSource{header: header, fetch: func(page int) ([][]string, error) {
			p, err := st.ListLinks(ctx, store.LinkFilter{Query: query, Domain: domain, Page: page, PageSize: exportPageSize})
			if err != nil {
				return nil, err
			}
			out := make([][]string, 0, len(p.Items))
			for _, it := range p.Items {
				out = append(out, []string{
					it.URL, it.Domain,
					strconv.FormatInt(it.ChatID, 10), it.ChatTitle,
					strconv.Itoa(it.MessageID), strconv.FormatInt(it.MessageRowID, 10),
					time.Unix(it.Date, 0).Format(time.RFC3339),
					it.SenderName, it.SenderUsername, it.Text,
				})
			}
			return out, nil
		}}, nil
	case "materials":
		// The material layer is what a site importer actually wants: one row
		// per post (albums folded), carrying the derived identity that survives
		// a rebuild.
		header := []string{
			"id", "chat_id", "chat_title", "chat_username", "kind", "title",
			"body", "caption", "content_hash", "text_hash",
			"media_count", "media_kind", "char_count",
			"source_url", "source_name", "date",
			"tags", "status", "duplicate_of", "message_ids",
			"views", "forwards",
			"media_sha256", "media_mime", "media_size", "media_url", "width", "height", "duration",
			"media_path", "thumbnail_path", "preview_path",
		}
		return ExportSource{header: header, fetch: func(page int) ([][]string, error) {
			items, err := st.ListMaterials(ctx, store.MaterialFilter{
				ChatID:   chat,
				Query:    query,
				Status:   "all",
				Page:     page,
				PageSize: exportPageSize,
			})
			if err != nil {
				return nil, err
			}
			out := make([][]string, 0, len(items))
			for _, m := range items {
				out = append(out, []string{
					strconv.FormatInt(m.ID, 10),
					strconv.FormatInt(m.ChatID, 10),
					m.ChatTitle,
					m.ChatUsername,
					m.Kind,
					m.Title,
					m.Body,
					m.Caption,
					m.ContentHash,
					m.TextHash,
					strconv.Itoa(m.MediaCount),
					m.MediaKind,
					strconv.Itoa(m.CharCount),
					m.SourceURL,
					m.SourceName,
					time.Unix(m.Date, 0).Format(time.RFC3339),
					m.Tags,
					m.Status,
					strconv.FormatInt(m.DuplicateOf, 10),
					m.MessageIDs,
					strconv.Itoa(m.Views),
					strconv.Itoa(m.Forwards),
					m.MediaSHA,
					m.MediaMIME,
					strconv.FormatInt(m.MediaSize, 10),
					m.MediaURL,
					strconv.Itoa(m.Width),
					strconv.Itoa(m.Height),
					strconv.Itoa(m.Duration),
					m.MediaPath,
					m.ThumbnailPath,
					m.PreviewPath,
				})
			}
			return out, nil
		}}, nil
	case "serials":
		// The receive ledger as a file: one row per numbered media item, in
		// descending serial order. Paging walks backwards by serial, so the
		// export is stable even while new items arrive at the top.
		header := []string{
			"serial", "date", "direction", "kind", "mime", "size", "status",
			"file_name", "chat_id", "chat_label", "message_id", "path", "caption",
		}
		var nextMax int64
		// Serial 1 is the end of the walk. MaxSerial=0 means "unbounded" to the
		// store, so the walk must stop explicitly instead of restarting from the
		// newest row.
		done := false
		return ExportSource{header: header, fetch: func(_ int) ([][]string, error) {
			if done {
				return nil, nil
			}
			items, err := st.Serials(ctx, store.SerialFilter{Limit: exportPageSize, MaxSerial: nextMax})
			if err != nil {
				return nil, err
			}
			out := make([][]string, 0, len(items))
			for _, it := range items {
				dir := "in"
				if it.Out {
					dir = "out"
				}
				date := ""
				if !it.Date.IsZero() {
					date = it.Date.UTC().Format(time.RFC3339)
				}
				out = append(out, []string{
					strconv.FormatInt(it.Serial, 10),
					date,
					dir,
					it.Kind,
					it.MIME,
					strconv.FormatInt(it.Size, 10),
					it.Status,
					it.FileName,
					strconv.FormatInt(it.ChatID, 10),
					it.ChatLabel,
					strconv.Itoa(it.MessageID),
					it.Path,
					it.Caption,
				})
			}
			if len(items) > 0 {
				next := items[len(items)-1].Serial - 1
				if next < 1 {
					done = true
				} else {
					nextMax = next
				}
			}
			return out, nil
		}}, nil
	}
	return ExportSource{}, fmt.Errorf("kind must be one of messages, media, links, deleted, edited, materials, serials")
}
