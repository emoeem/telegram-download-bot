package store

import "context"

type EditedFilter struct {
	Query    string
	ChatID   int64
	Page     int
	PageSize int
}

type EditedVersion struct {
	Version   int
	Text      string
	Caption   string
	EditedAt  int64
	IsCurrent bool
}

type EditedItem struct {
	MessageView
	EditCount int
}

type EditedPage struct {
	Items    []EditedItem
	Total    int
	Page     int
	PageSize int
}

// ListEditedMessages reuses SearchAdvanced's existing parser and only adds EditedOnly/pagination.
func (s *Store) ListEditedMessages(ctx context.Context, f EditedFilter) (EditedPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	offset := (f.Page - 1) * f.PageSize
	results, err := s.SearchAdvanced(ctx, f.Query, SearchOptions{EditedOnly: true, ChatID: f.ChatID, Limit: f.PageSize, Offset: offset})
	if err != nil {
		return EditedPage{}, err
	}
	total, err := s.CountSearch(ctx, f.Query, SearchOptions{EditedOnly: true, ChatID: f.ChatID})
	if err != nil {
		return EditedPage{}, err
	}
	ids := make([]int64, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.RowID)
	}
	views, err := s.MessageViewsByRowIDs(ctx, ids)
	if err != nil {
		return EditedPage{}, err
	}
	views = orderMessageViews(views, ids)
	counts, err := s.editCounts(ctx, ids)
	if err != nil {
		return EditedPage{}, err
	}
	items := make([]EditedItem, 0, len(views))
	for _, view := range views {
		items = append(items, EditedItem{MessageView: view, EditCount: counts[view.RowID]})
	}
	return EditedPage{Items: items, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}

// editCounts returns the per-message edit-version count for a whole page in one
// query instead of one COUNT per row.
func (s *Store) editCounts(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT message_rowid,COUNT(*) FROM message_edits WHERE message_rowid IN ("+string(placeholders)+") GROUP BY message_rowid", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// GetMessageEditTimeline returns archived versions in edit-time order followed by the current message.
func (s *Store) GetMessageEditTimeline(ctx context.Context, chatID int64, messageID int) ([]EditedVersion, error) {
	view, err := s.MessageView(ctx, chatID, messageID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT version,COALESCE(text,''),COALESCE(caption,''),edit_date FROM message_edits WHERE message_rowid=? ORDER BY edit_date ASC, version ASC, id ASC", view.RowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EditedVersion
	for rows.Next() {
		var v EditedVersion
		if err := rows.Scan(&v.Version, &v.Text, &v.Caption, &v.EditedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	out = append(out, EditedVersion{Version: len(out) + 1, Text: view.Text, Caption: view.Caption, EditedAt: view.EditDate, IsCurrent: true})
	return out, nil
}
