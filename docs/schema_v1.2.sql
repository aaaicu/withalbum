-- TARGET SCHEMA (not implemented by the bundled stdlib-only Go proof of concept).
-- SQLite 3.38+ expected. Run PRAGMA foreign_keys=ON on every connection.
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS users (
  user_id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS devices (
  device_id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  platform TEXT NOT NULL CHECK(platform IN ('ios','android','macos','web')),
  device_name TEXT NOT NULL,
  revoked_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS folders (
  folder_id TEXT PRIMARY KEY,
  folder_name TEXT NOT NULL,
  owner_user_id TEXT NOT NULL REFERENCES users(user_id),
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS folder_members (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  role TEXT NOT NULL CHECK(role IN ('owner','editor','viewer')),
  PRIMARY KEY(folder_id, user_id)
);
CREATE TABLE IF NOT EXISTS blobs (
  sha256 TEXT PRIMARY KEY CHECK(length(sha256) = 64),
  bytes INTEGER NOT NULL CHECK(bytes > 0),
  mime_type TEXT NOT NULL,
  relative_path TEXT NOT NULL UNIQUE,
  verified_at TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS assets (
  asset_id TEXT PRIMARY KEY,
  original_sha256 TEXT NOT NULL REFERENCES blobs(sha256),
  original_filename TEXT NOT NULL,
  taken_at TEXT,
  media_kind TEXT NOT NULL CHECK(media_kind IN ('image','video','live_photo','raw','other')),
  width INTEGER,
  height INTEGER,
  duration_ms INTEGER,
  preview_state TEXT NOT NULL DEFAULT 'pending' CHECK(preview_state IN ('pending','ready','error')),
  preview_sha256 TEXT REFERENCES blobs(sha256),
  uploader_user_id TEXT REFERENCES users(user_id),
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_assets_blob ON assets(original_sha256);
CREATE TABLE IF NOT EXISTS folder_assets (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  asset_id TEXT NOT NULL REFERENCES assets(asset_id),
  added_by_device_id TEXT REFERENCES devices(device_id),
  added_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  removed_at TEXT,
  PRIMARY KEY(folder_id, asset_id)
);
CREATE TABLE IF NOT EXISTS folder_sequences (
  folder_id TEXT PRIMARY KEY REFERENCES folders(folder_id) ON DELETE CASCADE,
  last_seq INTEGER NOT NULL DEFAULT 0 CHECK(last_seq >= 0)
);
CREATE TABLE IF NOT EXISTS folder_changes (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  seq INTEGER NOT NULL CHECK(seq > 0),
  event_type TEXT NOT NULL CHECK(event_type IN ('asset_added','asset_removed','asset_updated')),
  asset_id TEXT NOT NULL,
  source_device_id TEXT,
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY(folder_id,seq)
);
CREATE INDEX IF NOT EXISTS idx_folder_changes_asset ON folder_changes(folder_id,asset_id);
CREATE TABLE IF NOT EXISTS device_sync_cursors (
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  applied_seq INTEGER NOT NULL DEFAULT 0 CHECK(applied_seq >= 0),
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY(device_id,folder_id)
);
CREATE TABLE IF NOT EXISTS idempotency_keys (
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  response_json TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY(device_id,key)
);
