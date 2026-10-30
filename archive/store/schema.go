package store

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS chats (
 id INTEGER PRIMARY KEY, type TEXT NOT NULL, username TEXT, title TEXT,
 access_hash INTEGER, photo_id INTEGER, avatar_path TEXT, last_msg_id INTEGER NOT NULL DEFAULT 0,
 is_forum INTEGER NOT NULL DEFAULT 0, raw_json BLOB,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY, username TEXT, first_name TEXT, last_name TEXT, phone TEXT,
 is_bot INTEGER NOT NULL DEFAULT 0, access_hash INTEGER, photo_id INTEGER, avatar_path TEXT, raw_json BLOB,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, sender_id INTEGER,
 date INTEGER NOT NULL, edit_date INTEGER, text TEXT, caption TEXT,
 reply_to_message_id INTEGER, reply_to_top_id INTEGER, reply_to_peer_id INTEGER,
 thread_id INTEGER, grouped_id INTEGER, forward_from_id INTEGER,
 forward_from_chat_id INTEGER, forward_from_message_id INTEGER,
 forward_from_name TEXT, forward_post_author TEXT,
 views INTEGER, forwards INTEGER, reply_count INTEGER,
 has_media INTEGER NOT NULL DEFAULT 0, has_entities INTEGER NOT NULL DEFAULT 0,
 -- out=1 means the logged-in account sent it, out=0 means it arrived from the
 -- peer. The receive ledger only numbers inbound media, so this distinction is
 -- load-bearing rather than cosmetic.
 out INTEGER NOT NULL DEFAULT 0,
 pinned INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0,
 deleted_at INTEGER, raw_json BLOB, fts_text TEXT, buttons_json TEXT,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 UNIQUE(chat_id,message_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_chat_msg ON messages(chat_id,message_id);
CREATE INDEX IF NOT EXISTS idx_messages_chat_date ON messages(chat_id,date);
CREATE INDEX IF NOT EXISTS idx_messages_grouped ON messages(grouped_id);
CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender_id);
-- Forum topics and reply threads filter on these columns; without the indexes a
-- topic view scans and sorts the whole table.
CREATE INDEX IF NOT EXISTS idx_messages_chat_thread ON messages(chat_id,thread_id);
CREATE INDEX IF NOT EXISTS idx_messages_chat_reply_top ON messages(chat_id,reply_to_top_id);

CREATE TABLE IF NOT EXISTS message_edits (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_rowid INTEGER NOT NULL,
 version INTEGER NOT NULL, text TEXT, caption TEXT, raw_json BLOB,
 edit_date INTEGER NOT NULL, archived_at INTEGER NOT NULL,
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_message_edits_message_rowid ON message_edits(message_rowid);
CREATE INDEX IF NOT EXISTS idx_message_edits_message_edit_date ON message_edits(message_rowid, edit_date);

CREATE TABLE IF NOT EXISTS message_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 message_rowid INTEGER NOT NULL,
 chat_id INTEGER NOT NULL,
 message_id INTEGER NOT NULL,
 event_type TEXT NOT NULL,
 actor_id INTEGER,
 subject_id INTEGER,
 payload_json TEXT,
 date INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_message_events_chat_date ON message_events(chat_id, date);
CREATE INDEX IF NOT EXISTS idx_message_events_type ON message_events(event_type);
CREATE INDEX IF NOT EXISTS idx_message_events_message ON message_events(message_rowid);
CREATE TABLE IF NOT EXISTS message_links (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_rowid INTEGER NOT NULL,
 url TEXT NOT NULL, domain TEXT, expanded_url TEXT,
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_message_links_message_rowid ON message_links(message_rowid);
CREATE INDEX IF NOT EXISTS idx_message_links_domain ON message_links(domain);

CREATE TABLE IF NOT EXISTS message_entities (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_rowid INTEGER NOT NULL,
 type TEXT NOT NULL, offset INTEGER NOT NULL, length INTEGER NOT NULL, value TEXT,
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS message_reactions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_rowid INTEGER NOT NULL,
 emoji TEXT NOT NULL, count INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 UNIQUE(message_rowid, emoji),
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_message_reactions_message_rowid ON message_reactions(message_rowid);
CREATE TABLE IF NOT EXISTS media (
 id INTEGER PRIMARY KEY AUTOINCREMENT, message_rowid INTEGER NOT NULL,
 -- serial is the human-facing receive number: assigned once, at ingest, and
 -- never reused. NULL means "not assigned yet" (legacy rows before the column
 -- existed); the unique index tolerates many NULLs, which is exactly the
 -- backfill window, and forbids every duplicate number after it.
 serial INTEGER,
 type TEXT NOT NULL, file_id_unique TEXT, file_name TEXT, mime_type TEXT,
 size INTEGER, width INTEGER, height INTEGER, duration INTEGER,
 sha256 TEXT, original_path TEXT, thumbnail_path TEXT, preview_path TEXT,
 source_url TEXT NOT NULL DEFAULT '',
 ref_count INTEGER NOT NULL DEFAULT 1,
 status TEXT NOT NULL DEFAULT 'pending', error TEXT, attempts INTEGER NOT NULL DEFAULT 0,
 next_retry_at INTEGER, downloaded_at INTEGER, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE,
 UNIQUE(message_rowid, source_url)
);
CREATE INDEX IF NOT EXISTS idx_media_sha ON media(sha256);
CREATE INDEX IF NOT EXISTS idx_media_status ON media(status);
CREATE INDEX IF NOT EXISTS idx_media_status_retry_id ON media(status,next_retry_at,id);
CREATE INDEX IF NOT EXISTS idx_media_file_id_unique ON media(file_id_unique);
CREATE TABLE IF NOT EXISTS files (
 sha256 TEXT PRIMARY KEY, size INTEGER NOT NULL, mime_type TEXT,
 state TEXT NOT NULL DEFAULT 'ready', ref_count INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_cursors (
 account_id INTEGER NOT NULL, chat_id INTEGER NOT NULL,
 channel_pts INTEGER NOT NULL DEFAULT 0, pts INTEGER NOT NULL DEFAULT 0,
 backlog_state TEXT NOT NULL DEFAULT 'pending', backlog_oldest_msg_id INTEGER,
 last_msg_id INTEGER NOT NULL DEFAULT 0, last_seen_at INTEGER NOT NULL DEFAULT 0,
 updated_at INTEGER NOT NULL, PRIMARY KEY(account_id,chat_id)
);
CREATE TABLE IF NOT EXISTS monitor_targets (
 chat_id INTEGER PRIMARY KEY, username TEXT, enabled INTEGER NOT NULL DEFAULT 1,
 archive_text INTEGER NOT NULL DEFAULT 1, archive_media INTEGER NOT NULL DEFAULT 1,
 archive_files INTEGER NOT NULL DEFAULT 1, media_max_size INTEGER NOT NULL DEFAULT 0,
 download_media INTEGER NOT NULL DEFAULT 0, archive_mode TEXT NOT NULL DEFAULT 'local',
 destination_id INTEGER, keywords_json TEXT, exclude_keywords_json TEXT, since_date INTEGER,
 autoclick_enabled INTEGER NOT NULL DEFAULT 0, autoclick_keywords_json TEXT,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS monitor_destinations (
 target_chat_id INTEGER NOT NULL,
 destination_id INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 PRIMARY KEY(target_chat_id,destination_id),
 FOREIGN KEY(destination_id) REFERENCES archive_destinations(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_monitor_destinations_destination ON monitor_destinations(destination_id);
CREATE TABLE IF NOT EXISTS archive_settings (
 key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notify_rules (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL,
 keywords_json TEXT NOT NULL DEFAULT '[]',
 exclude_keywords_json TEXT NOT NULL DEFAULT '[]',
 chat_ids_json TEXT NOT NULL DEFAULT '[]',
 webhook_url TEXT NOT NULL DEFAULT '',
 bot_token TEXT NOT NULL DEFAULT '',
 bot_chat_id TEXT NOT NULL DEFAULT '',
 enabled INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notify_log (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 rule_id INTEGER NOT NULL,
 chat_id INTEGER, message_id INTEGER,
 channel TEXT NOT NULL,
 status TEXT NOT NULL,
 error TEXT,
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notify_log_created ON notify_log(created_at DESC);
CREATE TABLE IF NOT EXISTS jobs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 kind TEXT NOT NULL,
 target_id INTEGER,
 chat_id INTEGER,
 status TEXT NOT NULL,
 progress_done INTEGER NOT NULL DEFAULT 0,
 progress_total INTEGER,
 error TEXT,
 started_at INTEGER,
 finished_at INTEGER,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_status_created ON jobs(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_jobs_target ON jobs(target_id, created_at DESC);
CREATE TABLE IF NOT EXISTS autoclick_log (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 chat_id INTEGER, message_id INTEGER,
 button_text TEXT, button_kind TEXT NOT NULL,
 status TEXT NOT NULL, detail TEXT,
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_autoclick_log_created ON autoclick_log(created_at DESC);
-- HasAutoClickLog runs on every incoming message and every queued click; without
-- this index it scans a log that grows until pruned.
CREATE INDEX IF NOT EXISTS idx_autoclick_log_chat_msg ON autoclick_log(chat_id,message_id);
-- autoclick_targets remembers every click destination that already fired. A
-- reward bot answers a /start by posting a message that carries the very same
-- /start again, so without a memory of what was already clicked the clicker
-- feeds its own replies back in and loops forever. Keyed by bot+payload, not by
-- message id: the echo always arrives under a fresh message id.
CREATE TABLE IF NOT EXISTS autoclick_targets (
 key TEXT PRIMARY KEY,
 first_clicked_at INTEGER NOT NULL,
 last_clicked_at INTEGER NOT NULL,
 hits INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_autoclick_targets_last ON autoclick_targets(last_clicked_at DESC);
CREATE TABLE IF NOT EXISTS autoclick_queue (
 chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending',
 attempts INTEGER NOT NULL DEFAULT 0,
 next_at INTEGER NOT NULL DEFAULT 0,
 last_error TEXT,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 PRIMARY KEY(chat_id,message_id)
);
CREATE INDEX IF NOT EXISTS idx_autoclick_queue_due ON autoclick_queue(state,next_at);
CREATE TABLE IF NOT EXISTS archive_destinations (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL UNIQUE,
 kind TEXT NOT NULL,
 chat_id INTEGER,
 enabled INTEGER NOT NULL DEFAULT 1,
 mode TEXT NOT NULL DEFAULT 'all',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS archive_exports (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 message_rowid INTEGER NOT NULL,
 destination_id INTEGER NOT NULL,
 destination_message_id INTEGER NOT NULL,
 grouped_id INTEGER,
 status TEXT NOT NULL DEFAULT 'sent',
 error TEXT, fallback_reason TEXT,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 UNIQUE(message_rowid,destination_id),
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE,
 FOREIGN KEY(destination_id) REFERENCES archive_destinations(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_exports_destination ON archive_exports(destination_id,destination_message_id);

-- materials is the normalized "asset" layer derived from raw messages: one row
-- per post (a single message) or per album (every message of one grouped_id).
-- Raw messages stay immutable; a material is always rebuildable from them, so
-- nothing here is a source of truth except content_hash/status/duplicate_of.
CREATE TABLE IF NOT EXISTS materials (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 chat_id INTEGER NOT NULL,
 anchor_rowid INTEGER NOT NULL,
 anchor_message_id INTEGER NOT NULL,
 grouped_id INTEGER,
 kind TEXT NOT NULL DEFAULT 'text',
 title TEXT NOT NULL DEFAULT '',
 body TEXT NOT NULL DEFAULT '',
 caption TEXT NOT NULL DEFAULT '',
 content_hash TEXT NOT NULL DEFAULT '',
 text_hash TEXT NOT NULL DEFAULT '',
 media_count INTEGER NOT NULL DEFAULT 0,
 media_kind TEXT NOT NULL DEFAULT '',
 char_count INTEGER NOT NULL DEFAULT 0,
 source_url TEXT NOT NULL DEFAULT '',
 source_name TEXT NOT NULL DEFAULT '',
 date INTEGER NOT NULL,
 tags TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'active',
 duplicate_of INTEGER,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 UNIQUE(chat_id,anchor_message_id)
);
CREATE INDEX IF NOT EXISTS idx_materials_content_hash ON materials(content_hash);
CREATE INDEX IF NOT EXISTS idx_materials_text_hash ON materials(text_hash);
CREATE INDEX IF NOT EXISTS idx_materials_chat_date ON materials(chat_id,date DESC);
CREATE INDEX IF NOT EXISTS idx_materials_kind ON materials(kind);
CREATE INDEX IF NOT EXISTS idx_materials_status ON materials(status);
-- The default library view sorts by date with no chat filter; without this the
-- whole table is sorted on every page.
CREATE INDEX IF NOT EXISTS idx_materials_date ON materials(date DESC,id DESC);

-- material_messages records which raw messages compose one material, so an
-- album can be re-expanded (or an edit re-applied) without guessing by grouped_id.
CREATE TABLE IF NOT EXISTS material_messages (
 material_id INTEGER NOT NULL,
 message_rowid INTEGER NOT NULL,
 position INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(material_id,message_rowid),
 FOREIGN KEY(material_id) REFERENCES materials(id) ON DELETE CASCADE,
 FOREIGN KEY(message_rowid) REFERENCES messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_material_messages_rowid ON material_messages(message_rowid);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
 fts_text, content='messages', content_rowid='id', tokenize='unicode61'
);
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
 INSERT INTO messages_fts(rowid,fts_text) VALUES(new.id,new.fts_text);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
 INSERT INTO messages_fts(messages_fts,rowid,fts_text) VALUES('delete',old.id,old.fts_text);
END;
-- Dropped first so an upgraded database replaces the old trigger: IF NOT EXISTS
-- alone would keep the version that rewrites the FTS row on every update.
DROP TRIGGER IF EXISTS messages_au;
CREATE TRIGGER messages_au AFTER UPDATE OF fts_text ON messages
WHEN old.fts_text IS NOT new.fts_text
BEGIN
 INSERT INTO messages_fts(messages_fts,rowid,fts_text) VALUES('delete',old.id,old.fts_text);
 INSERT INTO messages_fts(rowid,fts_text) VALUES(new.id,new.fts_text);
END;
`
