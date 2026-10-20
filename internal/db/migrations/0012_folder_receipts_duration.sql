-- Migration 0012: idempotent folder creation and the playing time of videos.
--
-- folder_create_receipts records POST /api/v1/folders requests that carried
-- an Idempotency-Key, keyed like upload_receipts by the account and the
-- device of the session (there is no folder yet). It is written in the
-- transaction that creates the folder, so a retried request finds either
-- nothing (no folder was created) or this row (the folder exists) and is
-- answered with that folder instead of creating a second one. The name is
-- kept to tell a retry (same name) from a reuse of the key for another
-- request. Rows are pruned with upload_receipts (meta.ReceiptTTL).
CREATE TABLE folder_create_receipts (
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,
  folder_name TEXT NOT NULL,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY(user_id, device_id, idempotency_key)
);
CREATE INDEX idx_folder_create_receipts_created ON folder_create_receipts(created_at);

-- The playing time of a movie original, read from its movie header
-- (internal/capturetime.Duration) by the capture-time worker after the
-- commit, like the capture time. It has its own state so that originals
-- whose capture time is already settled are read once more for their
-- duration (the backfill) without touching captured_at.
--   duration_state  'pending' until the worker has looked, then 'done'.
--   duration_ms     NULL when unknown or not a movie, else > 0.
ALTER TABLE blobs ADD COLUMN duration_state TEXT NOT NULL DEFAULT 'pending' CHECK(duration_state IN ('pending','done'));
ALTER TABLE blobs ADD COLUMN duration_ms INTEGER CHECK(duration_ms IS NULL OR (duration_ms > 0 AND duration_state = 'done'));
CREATE INDEX idx_blobs_duration_pending ON blobs(sha256) WHERE duration_state = 'pending';
