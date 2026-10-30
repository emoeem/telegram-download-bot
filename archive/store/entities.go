package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gotd/td/tg"
)

// UpsertEntities stores the users/chats/channels a history page or update
// carries. One transaction and three prepared statements: a page arrives with
// dozens of entities, and one autocommit round trip each dominated the ingest
// path. The conditional DO UPDATE skips a row whose raw JSON did not change, so
// rescanning the same page is nearly free.
func (s *Store) UpsertEntities(ctx context.Context, e tg.Entities) error {
	if len(e.Users) == 0 && len(e.Chats) == 0 && len(e.Channels) == 0 {
		return nil
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	userStmt, err := tx.PrepareContext(ctx, `INSERT INTO users(id,username,first_name,last_name,phone,is_bot,access_hash,photo_id,raw_json,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET username=excluded.username,first_name=excluded.first_name,last_name=excluded.last_name,phone=excluded.phone,is_bot=excluded.is_bot,access_hash=excluded.access_hash,photo_id=excluded.photo_id,raw_json=excluded.raw_json,avatar_path=CASE WHEN users.photo_id IS NOT excluded.photo_id THEN NULL ELSE users.avatar_path END,updated_at=excluded.updated_at
WHERE users.raw_json IS NOT excluded.raw_json`)
	if err != nil {
		return err
	}
	defer userStmt.Close()
	for id, u := range e.Users {
		var photoID any
		if p, ok := u.GetPhoto(); ok {
			if x, ok := p.(*tg.UserProfilePhoto); ok {
				photoID = x.PhotoID
			}
		}
		raw, _ := json.Marshal(u)
		if _, err := userStmt.ExecContext(ctx, id, u.Username, u.FirstName, u.LastName, u.Phone, u.Bot, u.AccessHash, photoID, raw, now, now); err != nil {
			return err
		}
	}

	chatStmt, err := tx.PrepareContext(ctx, `INSERT INTO chats(id,type,username,title,access_hash,photo_id,raw_json,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET type=excluded.type,title=excluded.title,photo_id=excluded.photo_id,raw_json=excluded.raw_json,avatar_path=CASE WHEN chats.photo_id IS NOT excluded.photo_id THEN NULL ELSE chats.avatar_path END,updated_at=excluded.updated_at
WHERE chats.raw_json IS NOT excluded.raw_json`)
	if err != nil {
		return err
	}
	defer chatStmt.Close()
	for id, c := range e.Chats {
		var photoID any
		if p := c.GetPhoto(); p != nil {
			if x, ok := p.(*tg.ChatPhoto); ok {
				photoID = x.PhotoID
			}
		}
		raw, _ := json.Marshal(c)
		if _, err := chatStmt.ExecContext(ctx, id, "chat", "", c.Title, int64(0), photoID, raw, now, now); err != nil {
			return err
		}
	}

	channelStmt, err := tx.PrepareContext(ctx, `INSERT INTO chats(id,type,username,title,access_hash,photo_id,is_forum,raw_json,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET type=excluded.type,username=excluded.username,title=excluded.title,access_hash=excluded.access_hash,photo_id=excluded.photo_id,is_forum=excluded.is_forum,raw_json=excluded.raw_json,avatar_path=CASE WHEN chats.photo_id IS NOT excluded.photo_id THEN NULL ELSE chats.avatar_path END,updated_at=excluded.updated_at
WHERE chats.raw_json IS NOT excluded.raw_json`)
	if err != nil {
		return err
	}
	defer channelStmt.Close()
	for id, c := range e.Channels {
		var photoID any
		if p := c.GetPhoto(); p != nil {
			if x, ok := p.(*tg.ChatPhoto); ok {
				photoID = x.PhotoID
			}
		}
		forum := 0
		if c.Forum {
			forum = 1
		}
		raw, _ := json.Marshal(c)
		if _, err := channelStmt.ExecContext(ctx, id, "channel", c.Username, c.Title, c.AccessHash, photoID, forum, raw, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type AvatarJob struct {
	Kind        string
	ID, PhotoID int64
	AvatarPath  string
}

func (s *Store) PendingAvatars(ctx context.Context, limit int) ([]AvatarJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT 'user',id,photo_id,COALESCE(avatar_path,'') FROM users WHERE photo_id IS NOT NULL AND (avatar_path IS NULL OR avatar_path='') UNION ALL SELECT type,id,photo_id,COALESCE(avatar_path,'') FROM chats WHERE photo_id IS NOT NULL AND (avatar_path IS NULL OR avatar_path='') LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AvatarJob
	for rows.Next() {
		var j AvatarJob
		if err := rows.Scan(&j.Kind, &j.ID, &j.PhotoID, &j.AvatarPath); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Store) SetAvatarPath(ctx context.Context, kind string, id int64, path string) error {
	table := "users"
	if kind != "user" {
		table = "chats"
	}
	_, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET avatar_path=?,updated_at=? WHERE id=?", path, time.Now().Unix(), id)
	return err
}
func (s *Store) AvatarPath(ctx context.Context, kind string, id int64) (string, error) {
	table := "users"
	if kind != "user" {
		table = "chats"
	}
	var p string
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(avatar_path,'') FROM "+table+" WHERE id=?", id).Scan(&p)
	return p, err
}
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO archive_settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at", key, value, time.Now().Unix())
	return err
}
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM archive_settings WHERE key=?", key).Scan(&v)
	return v, err
}

// ChatTitle returns the stored display title/username of a chat, best effort.
func (s *Store) ChatTitle(ctx context.Context, id int64) (string, error) {
	var title string
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(title,''),NULLIF(username,''),CAST(id AS TEXT)) FROM chats WHERE id=?", id).Scan(&title)
	return title, err
}

// ChatIdentity returns the display title and the public username of a chat.
// The materializer needs both: the username builds a t.me permalink, the title
// is the human label a downstream CMS shows.
func (s *Store) ChatIdentity(ctx context.Context, id int64) (title, username string) {
	_ = s.db.QueryRowContext(ctx, "SELECT COALESCE(title,''),COALESCE(username,'') FROM chats WHERE id=?", id).Scan(&title, &username)
	return title, username
}
