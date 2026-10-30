package store

import "fmt"

// mediaColumns is every column of `media` in a fixed order, used by the rebuild
// below to copy rows without depending on SELECT * ordering.
const mediaColumns = `id,message_rowid,serial,type,file_id_unique,file_name,mime_type,
size,width,height,duration,sha256,original_path,thumbnail_path,preview_path,
source_url,ref_count,status,error,attempts,next_retry_at,downloaded_at,
created_at,updated_at,error_kind,error_detail,last_error_at`

// migrateMediaLinkUniqueness widens media's uniqueness from "one file per
// message" to "one file per (message, source_url)".
//
// The original constraint was a correct reading of the Telegram payload — a
// message carries at most one attached file — but it silently became a data
// loss bug once external links are archived: a post naming three videos in its
// text fit into one row, so two of them were overwritten on every edit and
// could only be recovered by re-fetching the channel. SQLite cannot ALTER a
// constraint, so the table is rebuilt.
//
// The rebuild reports whether it ran, because dropping the table also drops its
// indexes; the caller re-applies the schema when it did. Idempotent: an
// existing unique index over exactly (message_rowid, source_url) means the
// database is already in the new shape.
func (s *Store) migrateMediaLinkUniqueness() (bool, error) {
	ok, err := s.mediaHasLinkUniqueness()
	if err != nil || ok {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE media_migrate (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 message_rowid INTEGER NOT NULL,
 serial INTEGER,
 type TEXT NOT NULL, file_id_unique TEXT, file_name TEXT, mime_type TEXT,
 size INTEGER, width INTEGER, height INTEGER, duration INTEGER,
 sha256 TEXT, original_path TEXT, thumbnail_path TEXT, preview_path TEXT,
 source_url TEXT NOT NULL DEFAULT '',
 ref_count INTEGER NOT NULL DEFAULT 1,
 status TEXT NOT NULL DEFAULT 'pending', error TEXT, attempts INTEGER NOT NULL DEFAULT 0,
 next_retry_at INTEGER, downloaded_at INTEGER, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 error_kind TEXT, error_detail TEXT, last_error_at INTEGER,
 UNIQUE(message_rowid, source_url),
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE
 )`,
		`INSERT INTO media_migrate (` + mediaColumns + `) SELECT ` + mediaColumns + ` FROM media`,
		`DROP TABLE media`,
		`ALTER TABLE media_migrate RENAME TO media`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return false, fmt.Errorf("rebuild media for link uniqueness: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// mediaHasLinkUniqueness reports whether media's unique index already covers
// (message_rowid, source_url). Asking the index rather than matching the schema
// text keeps this working after a future column reorder.
func (s *Store) mediaHasLinkUniqueness() (bool, error) {
	rows, err := s.db.Query("PRAGMA index_list('media')")
	if err != nil {
		return false, err
	}
	var names []string
	for rows.Next() {
		// seq|name|unique|origin|partial
		var seq, unique, partial int
		var name, origin string
		if err = rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return false, err
		}
		if unique == 1 {
			names = append(names, name)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	for _, name := range names {
		cols, err := s.indexColumns(name)
		if err != nil {
			return false, err
		}
		if len(cols) == 2 && cols[0] == "message_rowid" && cols[1] == "source_url" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) indexColumns(name string) ([]string, error) {
	rows, err := s.db.Query("PRAGMA index_info('" + name + "')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		// seqno|cid|name
		var seq, cid int
		var col string
		if err := rows.Scan(&seq, &cid, &col); err != nil {
			return nil, err
		}
		cols = append(cols, col)
	}
	return cols, rows.Err()
}
