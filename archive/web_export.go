package archive

import (
	"context"
	"encoding/csv"
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

// exportSource describes one exportable view: its CSV header and a paged fetch
// that returns rows aligned with that header.
type exportSource struct {
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
	chat, _ := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
	topic, _ := strconv.ParseInt(r.URL.Query().Get("topic"), 10, 64)

	src, err := w.exportSource(r.Context(), kind, query, mediaKind, chat, topic)
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
		w.exportCSV(rw, src)
	case "json":
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.exportJSON(rw, src)
	default:
		http.Error(rw, "format must be csv or json", http.StatusBadRequest)
	}
}

// exportSource resolves the requested view into a header plus paged fetcher.
func (w *WebServer) exportSource(ctx context.Context, kind, query, mediaKind string, chat, topic int64) (exportSource, error) {
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
		return exportSource{header: messageHeader, fetch: func(page int) ([][]string, error) {
			p, err := w.Store.ListMessages(ctx, store.MessageFilter{Query: query, ChatID: chat, Topic: topic, Page: page, PageSize: exportPageSize})
			if err != nil {
				return nil, err
			}
			return viewRows(p.Items), nil
		}}, nil
	case "deleted":
		return exportSource{header: messageHeader, fetch: func(page int) ([][]string, error) {
			p, err := w.Store.ListDeletedMessages(ctx, store.DeletedFilter{Query: query, ChatID: chat, Page: page, PageSize: exportPageSize})
			if err != nil {
				return nil, err
			}
			return viewRows(p.Items), nil
		}}, nil
	case "edited":
		header := append(append([]string{}, messageHeader...), "edit_count")
		return exportSource{header: header, fetch: func(page int) ([][]string, error) {
			p, err := w.Store.ListEditedMessages(ctx, store.EditedFilter{Query: query, ChatID: chat, Page: page, PageSize: exportPageSize})
			if err != nil {
				return nil, err
			}
			rows := viewRows(make([]store.MessageView, 0, len(p.Items)))
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
		return exportSource{header: header, fetch: func(page int) ([][]string, error) {
			p, err := w.Store.ListMediaMessages(ctx, store.MediaFilter{Kind: mk, Page: page, PageSize: exportPageSize})
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
		return exportSource{header: header, fetch: func(page int) ([][]string, error) {
			p, err := w.Store.ListLinks(ctx, store.LinkFilter{Query: query, Page: page, PageSize: exportPageSize})
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
	}
	return exportSource{}, fmt.Errorf("kind must be one of messages, media, links, deleted, edited")
}

func (w *WebServer) exportCSV(rw http.ResponseWriter, src exportSource) {
	cw := csv.NewWriter(rw)
	defer cw.Flush()
	_ = cw.Write(src.header)
	_ = w.streamExport(src, func(row []string) bool { return cw.Write(row) == nil })
}

func (w *WebServer) exportJSON(rw http.ResponseWriter, src exportSource) {
	_, _ = rw.Write([]byte("["))
	first := true
	_ = w.streamExport(src, func(row []string) bool {
		obj := make(map[string]string, len(row))
		for i, h := range src.header {
			if i < len(row) {
				obj[h] = row[i]
			}
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return false
		}
		if !first {
			_, _ = rw.Write([]byte(","))
		}
		first = false
		_, _ = rw.Write(b)
		return true
	})
	_, _ = rw.Write([]byte("]\n"))
}

// streamExport walks pages until the source is exhausted or the row cap is hit.
func (w *WebServer) streamExport(src exportSource, emit func([]string) bool) error {
	for page, total := 1, 0; total < exportRowLimit; page++ {
		rows, err := src.fetch(page)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if total >= exportRowLimit {
				return nil
			}
			total++
			if !emit(row) {
				return nil
			}
		}
		if len(rows) < exportPageSize {
			return nil
		}
	}
	return nil
}
