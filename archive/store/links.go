package store

import (
	"context"
	"strings"
)

// linkDomainLimit caps the domain facet query.
const linkDomainLimit = 50

// linkSelect is the projection behind the Links view.
const linkSelect = `SELECT ml.url,COALESCE(ml.domain,''),m.id,m.chat_id,COALESCE(c.title,c.username,''),m.message_id,COALESCE(u.first_name||' '||u.last_name,u.username,''),COALESCE(u.username,''),m.date,COALESCE(NULLIF(m.text,''),NULLIF(m.caption,''),'') ` +
	`FROM message_links ml JOIN messages m ON m.id=ml.message_rowid ` +
	`LEFT JOIN users u ON u.id=m.sender_id LEFT JOIN chats c ON c.id=m.chat_id`

type LinkFilter struct {
	Query    string
	Domain   string
	Page     int
	PageSize int
}

type LinkItem struct {
	URL            string
	Domain         string
	MessageRowID   int64
	ChatID         int64
	ChatTitle      string
	MessageID      int
	SenderName     string
	SenderUsername string
	Date           int64
	Text           string
}

type LinkDomainCount struct {
	Domain string
	Count  int
}

type LinkPage struct {
	Items    []LinkItem
	Total    int
	Page     int
	PageSize int
}

func (s *Store) ListLinks(ctx context.Context, f LinkFilter) (LinkPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 50
	}
	domain := strings.TrimSpace(strings.ToLower(f.Domain))

	where, args, _, err := s.buildSearchFilter(ctx, f.Query, SearchOptions{}, false)
	if err != nil {
		return LinkPage{}, err
	}
	// The outer join on message_links already proves the message has a link, so
	// HasLink here would add a redundant correlated EXISTS. The domain predicate
	// is appended only when set: "?='' OR ..." defeats the index planner.
	conds := append([]string{}, where...)
	filterArgs := append([]any{}, args...)
	if domain != "" {
		conds = append(conds, "lower(COALESCE(ml.domain,''))=?")
		filterArgs = append(filterArgs, domain)
	}
	filter := " WHERE " + strings.Join(conds, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_links ml JOIN messages m ON m.id=ml.message_rowid"+filter, filterArgs...).Scan(&total); err != nil {
		return LinkPage{}, err
	}
	pageArgs := append(append([]any{}, filterArgs...), f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := s.db.QueryContext(ctx, linkSelect+filter+" ORDER BY m.date DESC, ml.id ASC LIMIT ? OFFSET ?", pageArgs...)
	if err != nil {
		return LinkPage{}, err
	}
	defer rows.Close()
	items := []LinkItem{}
	for rows.Next() {
		var item LinkItem
		if err := rows.Scan(&item.URL, &item.Domain, &item.MessageRowID, &item.ChatID, &item.ChatTitle, &item.MessageID, &item.SenderName, &item.SenderUsername, &item.Date, &item.Text); err != nil {
			return LinkPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return LinkPage{}, err
	}
	return LinkPage{Items: items, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}

func (s *Store) ListLinkDomains(ctx context.Context, query string) ([]LinkDomainCount, error) {
	// One aggregate query for every input: the search filter is pushed into the
	// same WHERE clause instead of walking all link rows and counting in Go.
	where, args, _, err := s.buildSearchFilter(ctx, query, SearchOptions{}, false)
	if err != nil {
		return nil, err
	}
	q := `SELECT LOWER(TRIM(COALESCE(ml.domain,''))) AS d, COUNT(DISTINCT ml.message_rowid) AS n
FROM message_links ml JOIN messages m ON m.id=ml.message_rowid
WHERE COALESCE(ml.domain,'')!='' AND ` + strings.Join(where, " AND ") + `
GROUP BY d ORDER BY n DESC, d LIMIT ?`
	args = append(args, linkDomainLimit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LinkDomainCount{}
	for rows.Next() {
		var d LinkDomainCount
		if err := rows.Scan(&d.Domain, &d.Count); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
