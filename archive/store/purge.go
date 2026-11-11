package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Deletion is the one operation in this tool that destroys the archive, so it
// runs through a single explicit primitive with a dry-run mode rather than
// scattered DELETE statements.
//
// The rule that keeps it honest: rows and files are separate. Purge removes
// database rows and reports which content-addressed files lost their last
// reference; deleting those files is the caller's job. That split means a
// dry run is exact (it computes the same file list without touching anything),
// and a failed unlink leaves an orphan file rather than a dangling reference.

// PurgeSelector chooses what to delete. Every field is optional; an empty
// selector deletes nothing (guarded by RequireSelector).
type PurgeSelector struct {
	// Serial deletes exactly one numbered media item.
	Serial int64
	// ChatID restricts the purge to one chat.
	ChatID int64
	// Until restricts the purge to messages older than this instant.
	Until time.Time
	// Kind restricts to one media kind (photo, video, document, ...).
	Kind string
	// Messages controls whether the message row goes too. When false the
	// message text stays and only its media is dropped — the "keep the record,
	// free the bytes" mode.
	Messages bool
	// DryRun computes the report without deleting anything.
	DryRun bool
	// All selects every archived message and media object. It exists because
	// the empty-selector guard below is there to stop a typo from wiping the
	// archive — so wiping it has to be something a caller asks for by name,
	// not something that happens when a flag is forgotten.
	//
	// Configuration is deliberately out of scope: monitored targets,
	// notification rules, destinations and settings survive, because clearing
	// the archive should not silently stop watching.
	All bool
}

func (s PurgeSelector) empty() bool {
	if s.All {
		return false
	}
	return s.Serial == 0 && s.ChatID == 0 && s.Until.IsZero() && s.Kind == ""
}

// PurgeReport is what a purge did (or, in dry-run mode, would do).
type PurgeReport struct {
	Messages int     `json:"messages"`
	Media    int     `json:"media"`
	Bytes    int64   `json:"bytes"`
	Serials  []int64 `json:"serials"`
	// Files are content-addressed object paths whose last reference this purge
	// removed. The caller unlinks them.
	Files  []string `json:"files"`
	DryRun bool     `json:"dry_run"`
	// Everything reports that no selector narrowed the purge, so the caller
	// should drop derived views wholesale rather than one serial at a time.
	Everything bool `json:"everything"`
}

type purgeRow struct {
	mediaID int64
	rowID   int64
	serial  int64
	sha     string
	path    string
	size    int64
	kind    string
	mime    string
	chatID  int64
	msgID   int
}

// Purge deletes archive rows selected by sel and reports the files that became
// unreferenced.
// purgeEverything wipes archived content: messages, media rows, the tables
// derived from them, and the run history that only means anything while those
// rows exist. Configuration is untouched on purpose — a wipe must not silently
// stop monitoring, and the operator who clears the archive usually wants the
// watcher to keep filling it again.
func (s *Store) purgeEverything(ctx context.Context, sel PurgeSelector) (PurgeReport, error) {
	rep := PurgeReport{DryRun: sel.DryRun, Everything: true}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&rep.Messages); err != nil {
		return rep, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(size),0) FROM media`).Scan(&rep.Media, &rep.Bytes); err != nil {
		return rep, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(original_path,'') FROM media WHERE COALESCE(original_path,'') <> ''`)
	if err != nil {
		return rep, err
	}
	seen := map[string]struct{}{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return rep, err
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		rep.Files = append(rep.Files, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	if sel.DryRun {
		return rep, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()

	// Order is dependency order, not alphabetical: anything pointing at a
	// message row goes before the row itself, so a later schema that grows a
	// real foreign key still runs this list cleanly.
	for _, stmt := range []string{
		`DELETE FROM message_reactions`,
		`DELETE FROM message_entities`,
		`DELETE FROM message_links`,
		`DELETE FROM message_events`,
		`DELETE FROM message_edits`,
		`DELETE FROM material_messages`,
		`DELETE FROM materials`,
		`DELETE FROM media`,
		`DELETE FROM files`,
		`DELETE FROM messages`,
		`DELETE FROM sync_cursors`,
		`DELETE FROM autoclick_queue`,
		`DELETE FROM autoclick_log`,
		`DELETE FROM autoclick_targets`,
		`DELETE FROM notify_log`,
		`DELETE FROM jobs`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return rep, err
		}
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	return rep, nil
}

func (s *Store) Purge(ctx context.Context, sel PurgeSelector) (PurgeReport, error) {
	rep := PurgeReport{DryRun: sel.DryRun}
	if sel.All {
		return s.purgeEverything(ctx, sel)
	}
	if sel.empty() {
		return rep, fmt.Errorf("purge selector is empty: pass --serial, --chat or --until so a typo cannot wipe the archive")
	}

	var (
		where []string
		args  []any
	)
	where = append(where, "1=1")
	if sel.Serial > 0 {
		where = append(where, "media.serial=?")
		args = append(args, sel.Serial)
	}
	if sel.ChatID != 0 {
		where = append(where, "m.chat_id=?")
		args = append(args, sel.ChatID)
	}
	if !sel.Until.IsZero() {
		where = append(where, "m.date<?")
		args = append(args, sel.Until.Unix())
	}
	if sel.Kind != "" {
		where = append(where, "media.type=?")
		args = append(args, sel.Kind)
	}
	q := `SELECT media.id, media.message_rowid, COALESCE(media.serial,0), COALESCE(media.sha256,''),
       COALESCE(media.original_path,''), COALESCE(media.size,0), media.type, COALESCE(media.mime_type,''),
       m.chat_id, m.message_id
 FROM media JOIN messages m ON m.id=media.message_rowid
 WHERE ` + strings.Join(where, " AND ")

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return rep, err
	}
	var items []purgeRow
	seenMsg := map[int64]struct{}{}
	for rows.Next() {
		var it purgeRow
		if err := rows.Scan(&it.mediaID, &it.rowID, &it.serial, &it.sha, &it.path, &it.size, &it.kind, &it.mime, &it.chatID, &it.msgID); err != nil {
			rows.Close()
			return rep, err
		}
		items = append(items, it)
		// Messages are only counted when they are actually going away; in
		// media-only mode the message row survives and must not be reported as
		// deleted.
		if sel.Messages {
			if _, ok := seenMsg[it.rowID]; !ok {
				seenMsg[it.rowID] = struct{}{}
				rep.Messages++
			}
		}
		rep.Media++
		rep.Bytes += it.size
		if it.serial > 0 {
			rep.Serials = append(rep.Serials, it.serial)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	if len(items) == 0 {
		return rep, nil
	}

	// Count how many rows point at each object, so a shared object is only
	// unlinked once every reference is gone.
	bySHA := map[string]int{}
	for _, it := range items {
		if it.sha != "" {
			bySHA[it.sha]++
		}
	}

	if sel.DryRun {
		// 与 purgeItemsTx 的 refcount 逻辑保持一致：内容去重共享的对象只有
		// 引用清零才真正删除。预览若不算 ref_count，会把"承诺删除但实际
		// 保留"的对象列进去——操作员核对要清空的内容时看到的就是这个数。
		for _, it := range items {
			if it.path == "" || bySHA[it.sha] <= 0 {
				continue
			}
			var left int
			_ = s.db.QueryRowContext(ctx,
				"SELECT ref_count FROM files WHERE sha256=?", it.sha).Scan(&left)
			// 无 files 行（旧版导入）：与 purgeItemsTx 同语义，视为归我们删。
			if left-bySHA[it.sha] > 0 {
				bySHA[it.sha] = 0 // 仍被选择之外引用：预览如实排除
				continue
			}
			rep.Files = append(rep.Files, it.path)
			bySHA[it.sha] = 0 // report each object once
		}
		return rep, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()

	files, err := purgeItemsTx(ctx, tx, items, msgRowIDs(seenMsg, sel.Messages))
	if err != nil {
		return rep, err
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	rep.Files = files
	return rep, nil
}

// msgRowIDs flattens the seen-message set in a stable order; nil means the
// caller keeps the message rows.
func msgRowIDs(seen map[int64]struct{}, withMessages bool) []int64 {
	if !withMessages {
		return nil
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

// purgeItemsTx deletes the selected media rows and, when msgIDs is non-nil, the
// named message rows as well, inside an open transaction. It releases
// content-addressed references and returns the object paths whose last
// reference just dropped. Callers unlink the files only after the commit: a
// rolled-back purge must not destroy bytes the surviving rows still reference.
func purgeItemsTx(ctx context.Context, tx *sql.Tx, items []purgeRow, msgIDs []int64) ([]string, error) {
	for _, rowID := range msgIDs {
		if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE id=?", rowID); err != nil {
			return nil, err
		}
		// materials.anchor_rowid has no foreign key, so the cascade above does
		// not reach the material layer; drop the anchored rows here or the
		// library keeps listing posts whose message is gone.
		if _, err := tx.ExecContext(ctx, "DELETE FROM materials WHERE anchor_rowid=?", rowID); err != nil {
			return nil, err
		}
	}
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, "DELETE FROM media WHERE id=?", it.mediaID); err != nil {
			return nil, err
		}
	}

	// Count how many rows point at each object, so a shared object is only
	// unlinked once every reference is gone.
	bySHA := map[string]int{}
	for _, it := range items {
		if it.sha != "" {
			bySHA[it.sha]++
		}
	}
	var files []string
	for sha, n := range bySHA {
		if _, err := tx.ExecContext(ctx, "UPDATE files SET ref_count=ref_count-? WHERE sha256=?", n, sha); err != nil {
			return nil, err
		}
		var left int
		err := tx.QueryRowContext(ctx, "SELECT ref_count FROM files WHERE sha256=?", sha).Scan(&left)
		if err != nil {
			// No files row (legacy import): the object is still ours to remove.
			left = 0
		}
		if left > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM files WHERE sha256=?", sha); err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.sha == sha && it.path != "" {
				files = append(files, it.path)
				break
			}
		}
	}
	return files, nil
}

// PurgeMediaRows is the narrow primitive for "drop one media row, keep the
// message": used when a single attachment should disappear from the archive.
//
// Rows are selected by id rather than routed through Purge's serial selector,
// so never-numbered rows work too, and the reported file list is exactly the
// one whose references hit zero: an object shared through content dedupe keeps
// its bytes until its last reference in this database is gone.
func (s *Store) PurgeMediaRows(ctx context.Context, mediaIDs []int64) (PurgeReport, error) {
	rep := PurgeReport{}
	if len(mediaIDs) == 0 {
		return rep, nil
	}
	var items []purgeRow
	for _, id := range mediaIDs {
		it, err := s.purgeRowByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return rep, err
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return rep, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()
	files, err := purgeItemsTx(ctx, tx, items, nil)
	if err != nil {
		return rep, err
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}
	for _, it := range items {
		rep.Media++
		rep.Bytes += it.size
		if it.serial > 0 {
			rep.Serials = append(rep.Serials, it.serial)
		}
	}
	rep.Files = files
	return rep, nil
}

// purgeRowByID loads one media row as a purge item. The trailing zeros stand in
// for the chat/message columns the selector-driven query fills; the row-wise
// purge path never reads them.
func (s *Store) purgeRowByID(ctx context.Context, id int64) (purgeRow, error) {
	var it purgeRow
	err := s.db.QueryRowContext(ctx, `SELECT id, message_rowid, COALESCE(serial,0), COALESCE(sha256,''),
			COALESCE(original_path,''), COALESCE(size,0), type, COALESCE(mime_type,''), 0, 0
		FROM media WHERE id=?`, id).
		Scan(&it.mediaID, &it.rowID, &it.serial, &it.sha, &it.path, &it.size, &it.kind, &it.mime, &it.chatID, &it.msgID)
	return it, err
}

// purgeRowsForMessage selects the purgeable media rows of one message.
func (s *Store) purgeRowsForMessage(ctx context.Context, rowID int64) ([]purgeRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, message_rowid, COALESCE(serial,0), COALESCE(sha256,''),
			COALESCE(original_path,''), COALESCE(size,0), type, COALESCE(mime_type,''), 0, 0
		FROM media WHERE message_rowid=?`, rowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []purgeRow
	for rows.Next() {
		var it purgeRow
		if err := rows.Scan(&it.mediaID, &it.rowID, &it.serial, &it.sha, &it.path, &it.size, &it.kind, &it.mime, &it.chatID, &it.msgID); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}
