package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Destination struct {
	ID         int64
	Name, Kind string
	ChatID     sql.NullInt64
	Enabled    bool
	Mode       string
}

func (s *Store) AddDestination(ctx context.Context, name, kind string, chatID *int64) (int64, error) {
	var id int64
	var cid any
	if chatID != nil {
		cid = *chatID
	}
	e := s.db.QueryRowContext(ctx, "INSERT INTO archive_destinations(name,kind,chat_id,created_at,updated_at) VALUES(?,?,?,?,?) RETURNING id", name, kind, cid, time.Now().Unix(), time.Now().Unix()).Scan(&id)
	return id, e
}
func (s *Store) Destinations(ctx context.Context) ([]Destination, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT id,name,kind,chat_id,enabled,mode FROM archive_destinations ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		var d Destination
		if e = rows.Scan(&d.ID, &d.Name, &d.Kind, &d.ChatID, &d.Enabled, &d.Mode); e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) SetDestinationEnabled(ctx context.Context, id int64, on bool) error {
	_, e := s.db.ExecContext(ctx, "UPDATE archive_destinations SET enabled=?,updated_at=? WHERE id=?", on, time.Now().Unix(), id)
	return e
}
func (s *Store) Exported(ctx context.Context, row, dest int64) (int64, error) {
	var id int64
	e := s.db.QueryRowContext(ctx, "SELECT destination_message_id FROM archive_exports WHERE message_rowid=? AND destination_id=? AND status='sent'", row, dest).Scan(&id)
	return id, e
}
func (s *Store) MarkExport(ctx context.Context, row, dest, msg int64, err error) error {
	return s.MarkExportGrouped(ctx, row, dest, msg, 0, err)
}

type ExportMapping struct {
	RowID, DestinationID, DestinationMessageID, GroupedID int64
	Err                                                   error
}

func (s *Store) MarkExportGrouped(ctx context.Context, row, dest, msg, groupedID int64, err error) error {
	status := "sent"
	var ev any
	if err != nil {
		status = "failed"
		ev = err.Error()
	}
	_, e := s.db.ExecContext(ctx, `INSERT INTO archive_exports(message_rowid,destination_id,destination_message_id,grouped_id,status,error,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(message_rowid,destination_id) DO UPDATE SET destination_message_id=excluded.destination_message_id,grouped_id=excluded.grouped_id,status=excluded.status,error=excluded.error,updated_at=excluded.updated_at`,
		row, dest, msg, nullGroupedID(groupedID), status, ev, time.Now().Unix(), time.Now().Unix())
	return e
}

func nullGroupedID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) MarkExportsBatch(ctx context.Context, mappings []ExportMapping) error {
	if len(mappings) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO archive_exports(message_rowid,destination_id,destination_message_id,grouped_id,status,error,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(message_rowid,destination_id) DO UPDATE SET destination_message_id=excluded.destination_message_id,grouped_id=excluded.grouped_id,status=excluded.status,error=excluded.error,updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, m := range mappings {
		status := "sent"
		var ev any
		if m.Err != nil {
			status = "failed"
			ev = m.Err.Error()
		}
		if _, err = stmt.ExecContext(ctx, m.RowID, m.DestinationID, m.DestinationMessageID, nullGroupedID(m.GroupedID), status, ev, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DestinationByChat(ctx context.Context, chatID int64) (Destination, error) {
	var d Destination
	err := s.db.QueryRowContext(ctx, "SELECT id,name,kind,chat_id,enabled,mode FROM archive_destinations WHERE chat_id=? LIMIT 1", chatID).Scan(&d.ID, &d.Name, &d.Kind, &d.ChatID, &d.Enabled, &d.Mode)
	return d, err
}
func (s *Store) Destination(ctx context.Context, id int64) (Destination, error) {
	var d Destination
	err := s.db.QueryRowContext(ctx, "SELECT id,name,kind,chat_id,enabled,mode FROM archive_destinations WHERE id=?", id).Scan(&d.ID, &d.Name, &d.Kind, &d.ChatID, &d.Enabled, &d.Mode)
	return d, err
}
func (s *Store) SetDestinationMode(ctx context.Context, id int64, mode string) error {
	if mode != "all" && mode != "text" && mode != "media" {
		return fmt.Errorf("invalid destination mode %q", mode)
	}
	_, err := s.db.ExecContext(ctx, "UPDATE archive_destinations SET mode=?,updated_at=? WHERE id=?", mode, time.Now().Unix(), id)
	return err
}

func (s *Store) EnsureDestination(ctx context.Context, name, kind string, chatID int64) (int64, error) {
	if d, err := s.DestinationByChat(ctx, chatID); err == nil {
		return d.ID, nil
	}
	return s.AddDestination(ctx, name, kind, &chatID)
}

func (s *Store) TargetDestinations(ctx context.Context, chatID int64) ([]Destination, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.name,d.kind,d.chat_id,d.enabled,d.mode
FROM archive_destinations d
JOIN monitor_destinations md ON md.destination_id=d.id
WHERE md.target_chat_id=? AND d.enabled=1
UNION
SELECT d.id,d.name,d.kind,d.chat_id,d.enabled,d.mode
FROM archive_destinations d JOIN monitor_targets t ON t.destination_id=d.id
WHERE t.chat_id=? AND d.enabled=1
ORDER BY id`, chatID, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		var d Destination
		if err := rows.Scan(&d.ID, &d.Name, &d.Kind, &d.ChatID, &d.Enabled, &d.Mode); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) AttachDestination(ctx context.Context, targetChatID, destinationID int64) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO monitor_destinations(target_chat_id,destination_id,created_at) VALUES(?,?,?)`, targetChatID, destinationID, time.Now().Unix()); err != nil {
		return err
	}
	// A locally-archiving target with an attached destination must mirror:
	// without the mode promotion the exporter would silently skip it.
	_, err := s.db.ExecContext(ctx, `UPDATE monitor_targets SET archive_mode='both',updated_at=? WHERE chat_id=? AND archive_mode='local'`, time.Now().Unix(), targetChatID)
	return err
}

type ExportRecord struct {
	Destination          Destination
	DestinationMessageID int64
}

func (s *Store) ExportRecords(ctx context.Context, rowID int64) ([]ExportRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.name,d.kind,d.chat_id,d.enabled,d.mode,e.destination_message_id
FROM archive_exports e JOIN archive_destinations d ON d.id=e.destination_id
WHERE e.message_rowid=? AND e.status='sent'`, rowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportRecord
	for rows.Next() {
		var r ExportRecord
		if err := rows.Scan(&r.Destination.ID, &r.Destination.Name, &r.Destination.Kind, &r.Destination.ChatID, &r.Destination.Enabled, &r.Destination.Mode, &r.DestinationMessageID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetExportFallback(ctx context.Context, row, dest int64, reason string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE archive_exports SET fallback_reason=?,updated_at=? WHERE message_rowid=? AND destination_id=?", reason, time.Now().Unix(), row, dest)
	return err
}
