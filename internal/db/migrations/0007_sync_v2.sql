-- Migration 0007: sync API v2, idempotent uploads, refresh grace.
--
-- Sync cursors carry an epoch so that a cursor from another folder, or from
-- this database before a restore (internal/backup rotates the epoch), is
-- refused instead of silently skipping events. The epoch of a folder is
-- derived from the database epoch and the folder id (sqlitestore).

CREATE TABLE sync_state (
  singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
  epoch TEXT NOT NULL CHECK(length(epoch) = 32)
);
INSERT INTO sync_state (singleton, epoch) VALUES (1, lower(hex(randomblob(16))));

-- Events with seq <= pruned_seq may have been deleted (retention). Nothing
-- prunes events yet; a cursor below pruned_seq is answered 409.
ALTER TABLE folder_sequences ADD COLUMN pruned_seq INTEGER NOT NULL DEFAULT 0 CHECK(pruned_seq >= 0);
INSERT INTO folder_sequences (folder_id, last_seq)
  SELECT f.folder_id, COALESCE((SELECT MAX(c.seq) FROM folder_changes c WHERE c.folder_id = f.folder_id), 0)
  FROM folders f WHERE NOT EXISTS (SELECT 1 FROM folder_sequences s WHERE s.folder_id = f.folder_id);

-- upload_receipts (schema v1.4) was never written. It is replaced by one row
-- per committed upload request, keyed by the uploading account, its device
-- (session), the folder and the client's Idempotency-Key, so that a retry
-- after a lost response returns the same result and keys of different
-- accounts, devices or folders never meet.
DROP TABLE upload_receipts;
CREATE TABLE upload_receipts (
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE RESTRICT,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,
  sha256 TEXT NOT NULL CHECK(length(sha256) = 64),
  bytes INTEGER NOT NULL CHECK(bytes > 0),
  result TEXT NOT NULL CHECK(result IN ('created','already_present')),
  asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE RESTRICT,
  created_at TEXT NOT NULL,
  PRIMARY KEY(user_id, device_id, folder_id, idempotency_key)
);
CREATE INDEX idx_upload_receipts_created ON upload_receipts(created_at);

-- Refresh grace: when (used_at) a refresh token was exchanged, and whether a
-- second presentation shortly after may still rotate the session once
-- (grace = 1) instead of revoking it. Rows written before this migration
-- have no grace.
ALTER TABLE auth_refresh_used ADD COLUMN used_at TEXT;
ALTER TABLE auth_refresh_used ADD COLUMN grace INTEGER NOT NULL DEFAULT 0 CHECK(grace IN (0,1));
