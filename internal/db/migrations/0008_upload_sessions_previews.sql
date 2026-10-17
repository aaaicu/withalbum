-- Migration 0008: resumable upload sessions and preview states.
--
-- An upload session receives one original in chunks (internal/httpapi
-- uploads.go). Its bytes are appended to .system/uploads/<upload_id>.part;
-- this row is the durable record of how many of them are complete
-- (received_bytes; the file may be longer after a crash and is cut back to
-- it). A session belongs to the account, the device of the session that
-- created it and the folder, and is keyed like upload_receipts by the
-- client's Idempotency-Key, so that the same key names one request whether
-- it is sent whole or in chunks. Completing a session commits through the
-- normal upload commit, which marks the row completed in the same
-- transaction; completed rows are kept until expires_at so that a retried
-- completion gets the stored result.
CREATE TABLE upload_sessions (
  upload_id TEXT PRIMARY KEY CHECK(length(upload_id) = 32),
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,
  sha256 TEXT NOT NULL CHECK(length(sha256) = 64),
  total_bytes INTEGER NOT NULL CHECK(total_bytes > 0),
  received_bytes INTEGER NOT NULL DEFAULT 0 CHECK(received_bytes >= 0 AND received_bytes <= total_bytes),
  filename TEXT NOT NULL,
  device_label TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','completed')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  UNIQUE(user_id, device_id, folder_id, idempotency_key)
);
CREATE INDEX idx_upload_sessions_expires ON upload_sessions(expires_at);
CREATE INDEX idx_upload_sessions_user ON upload_sessions(user_id, status);

-- Previews that are not ready yet, or not possible. NULL: as preview_ready
-- says (ready, or for rows written before this migration and by the legacy
-- import without a preview, unsupported). 'pending': waiting for the
-- preview worker (HEIC and video previews are made by external tools after
-- the commit); 'failed': the converter could not make one; 'unsupported':
-- no converter for this type on this server.
ALTER TABLE assets ADD COLUMN preview_state TEXT CHECK(preview_state IN ('pending','failed','unsupported'));
CREATE INDEX idx_assets_preview_pending ON assets(original_sha256) WHERE preview_state = 'pending';
