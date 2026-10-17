package store

import (
	"context"
	"fmt"
	"strings"
)

type MediaKind string

const (
	MediaKindImage MediaKind = "image"
	MediaKindVideo MediaKind = "video"
	MediaKindFile  MediaKind = "file"
)

type MediaFilter struct {
	Kind     MediaKind
	Query    string
	ChatID   int64
	Topic    int64
	Page     int
	PageSize int
}

type MediaItem struct {
	MessageView
}

type MediaPage struct {
	Items    []MediaItem
	Total    int
	Page     int
	PageSize int
}

func (s *Store) ListMediaMessages(ctx context.Context, f MediaFilter) (MediaPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	if f.Kind != MediaKindImage && f.Kind != MediaKindVideo && f.Kind != MediaKindFile {
		return MediaPage{}, fmt.Errorf("invalid media kind %q", f.Kind)
	}
	offset := (f.Page - 1) * f.PageSize
	query := strings.TrimSpace(f.Query)
	if query != "" {
		query += " "
	}
	query += "has:" + string(f.Kind)

	results, err := s.SearchAdvanced(ctx, query, SearchOptions{
		MediaReadyOnly: true,
		ChatID:         f.ChatID,
		Topic:          f.Topic,
		Limit:          f.PageSize,
		Offset:         offset,
	})
	if err != nil {
		return MediaPage{}, err
	}
	total, err := s.CountSearch(ctx, query, SearchOptions{MediaReadyOnly: true, ChatID: f.ChatID, Topic: f.Topic})
	if err != nil {
		return MediaPage{}, err
	}
	ids := make([]int64, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.RowID)
	}
	views, err := s.MessageViewsByRowIDs(ctx, ids)
	if err != nil {
		return MediaPage{}, err
	}
	views = orderMessageViews(views, ids)
	items := make([]MediaItem, 0, len(views))
	for _, view := range views {
		items = append(items, MediaItem{MessageView: view})
	}
	return MediaPage{Items: items, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}
