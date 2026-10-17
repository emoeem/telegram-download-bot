package store

import (
	"context"
)

type DeletedFilter struct {
	Query    string
	ChatID   int64
	Page     int
	PageSize int
}

type DeletedPage struct {
	Items    []MessageView
	Total    int
	Page     int
	PageSize int
}

// ListDeletedMessages reuses SearchAdvanced's existing parser and only adds DeletedOnly/pagination.
func (s *Store) ListDeletedMessages(ctx context.Context, f DeletedFilter) (DeletedPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	offset := (f.Page - 1) * f.PageSize

	search := SearchOptions{
		DeletedOnly: true,
		ChatID:      f.ChatID,
		Limit:       f.PageSize,
		Offset:      offset,
	}
	results, err := s.SearchAdvanced(ctx, f.Query, search)
	if err != nil {
		return DeletedPage{}, err
	}

	// Same shared filter parser as the page query, as a single COUNT.
	total, err := s.CountSearch(ctx, f.Query, SearchOptions{DeletedOnly: true, ChatID: f.ChatID})
	if err != nil {
		return DeletedPage{}, err
	}

	ids := make([]int64, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.RowID)
	}
	views, err := s.MessageViewsByRowIDs(ctx, ids)
	if err != nil {
		return DeletedPage{}, err
	}
	return DeletedPage{Items: orderMessageViews(views, ids), Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}
