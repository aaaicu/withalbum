-- Migration 0011: Live Photos (a still and its paired video as one asset).
--
-- An asset keeps exactly one primary original, assets.original_sha256 (the
-- still), so previews, /original, duplicate checks and older clients see
-- it as one photo. The paired video is an extra component in
-- asset_components; its bytes are an ordinary blobs row, so pool
-- reconciliation, verify, backup and restore cover it unchanged.
--
-- Both originals are uploaded through ordinary upload sessions (chunks,
-- resume, hash check) that belong to one upload bundle; completing the
-- bundle commits both in one transaction, so no asset, event or receipt
-- exists before both are verified and in the pool.

-- An upload bundle: the request of a client (Idempotency-Key, scoped like
-- upload_receipts by account, session device and folder) to store several
-- originals as one asset. Its components are the upload_sessions rows with
-- its bundle_id; deleting the bundle deletes them.
CREATE TABLE upload_bundles (
  bundle_id TEXT PRIMARY KEY CHECK(length(bundle_id) = 32),
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('live_photo')),
  status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','completed')),
  device_label TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  UNIQUE(user_id, device_id, folder_id, idempotency_key)
);
CREATE INDEX idx_upload_bundles_expires ON upload_bundles(expires_at);

-- A component session carries its bundle and role; its idempotency_key is
-- an internal "#b/<bundle_id>/<role>", outside the client key syntax, so it
-- never meets a client's key. One session per role and bundle.
ALTER TABLE upload_sessions ADD COLUMN bundle_id TEXT REFERENCES upload_bundles(bundle_id) ON DELETE CASCADE;
ALTER TABLE upload_sessions ADD COLUMN role TEXT CHECK(role IS NULL OR role IN ('still','paired_video'));
CREATE UNIQUE INDEX idx_upload_sessions_bundle_role ON upload_sessions(bundle_id, role) WHERE bundle_id IS NOT NULL;

-- A bundle and its component sessions expire together: a chunk appended to
-- one component (which moves that session's expires_at) extends the bundle
-- and the other components to the same time, so a slow second component
-- never finds the first one pruned. The inner UPDATE does not fire this
-- trigger again (recursive triggers are off, and it only raises earlier
-- times).
CREATE TRIGGER upload_sessions_extend_bundle
AFTER UPDATE OF expires_at ON upload_sessions
WHEN NEW.bundle_id IS NOT NULL
BEGIN
  UPDATE upload_bundles SET expires_at = NEW.expires_at, updated_at = NEW.updated_at
    WHERE bundle_id = NEW.bundle_id AND expires_at < NEW.expires_at;
  UPDATE upload_sessions SET expires_at = NEW.expires_at
    WHERE bundle_id = NEW.bundle_id AND upload_id <> NEW.upload_id AND expires_at < NEW.expires_at;
END;

-- Extra originals of an asset. role 'paired_video': the movie of a Live
-- Photo. pairing_id is the Apple content identifier both originals carry
-- (NULL when it could not be read). The blob is never removed while a row
-- refers to it.
CREATE TABLE asset_components (
  asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE RESTRICT,
  role TEXT NOT NULL CHECK(role IN ('paired_video')),
  sha256 TEXT NOT NULL REFERENCES blobs(sha256),
  filename TEXT NOT NULL,
  pairing_id TEXT,
  added_at TEXT NOT NULL,
  PRIMARY KEY(asset_id, role)
);
CREATE INDEX idx_asset_components_blob ON asset_components(sha256);

-- How the still and the video of a Live Photo (media_kind 'live_photo')
-- were matched: 'verified' (both carry the same content identifier) or
-- 'unverified' (at least one carries none the server could read). NULL for
-- every other asset.
ALTER TABLE assets ADD COLUMN pairing_state TEXT CHECK(pairing_state IN ('verified','unverified'));

-- upload_receipts gains the result 'upgraded' (a bundle attached its video
-- to the folder's existing still-only asset) and paired_sha256, the video
-- of a bundle receipt (NULL for single originals). SQLite cannot change a
-- CHECK constraint in place, so the table is rebuilt as in 0009: renamed,
-- created again, every row copied unchanged, the old one dropped and its
-- index created again. No other table refers to upload_receipts.
ALTER TABLE upload_receipts RENAME TO upload_receipts_0010;

CREATE TABLE upload_receipts (
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE RESTRICT,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,
  sha256 TEXT NOT NULL CHECK(length(sha256) = 64),
  bytes INTEGER NOT NULL CHECK(bytes > 0),
  result TEXT NOT NULL CHECK(result IN ('created','already_present','upgraded')),
  asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE RESTRICT,
  created_at TEXT NOT NULL,
  paired_sha256 TEXT CHECK(paired_sha256 IS NULL OR length(paired_sha256) = 64),
  PRIMARY KEY(user_id, device_id, folder_id, idempotency_key)
);

INSERT INTO upload_receipts (user_id, device_id, folder_id, idempotency_key, sha256, bytes, result, asset_id, created_at)
  SELECT user_id, device_id, folder_id, idempotency_key, sha256, bytes, result, asset_id, created_at FROM upload_receipts_0010;

DROP TABLE upload_receipts_0010;

CREATE INDEX idx_upload_receipts_created ON upload_receipts(created_at);
