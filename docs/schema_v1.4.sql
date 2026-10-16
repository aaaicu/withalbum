-- WithAlbum target schema v1.4: DESIGN ONLY, NOT yet wired into HTTP server.
-- New install target; NOT an automated migration for previous PoC state.json data.
-- SQLite 3.38+ and PRAGMA foreign_keys=ON on EVERY database connection.
PRAGMA foreign_keys=ON;
PRAGMA journal_mode=WAL;

CREATE TABLE users (
  user_id TEXT PRIMARY KEY,
  login_name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  display_name TEXT NOT NULL,
  password_hash TEXT NOT NULL,         -- Argon2id PHC encoded format, never plaintext
  server_role TEXT NOT NULL DEFAULT 'USER' CHECK(server_role IN ('SERVER_ADMIN','USER')),
  can_create_albums INTEGER NOT NULL DEFAULT 0 CHECK(can_create_albums IN (0,1)),
  status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled')),
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE devices (
  device_id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  platform TEXT NOT NULL CHECK(platform IN ('ios','android','macos','web')),
  device_name TEXT NOT NULL,
  revoked_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE auth_sessions (
  session_id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
  refresh_token_sha256 TEXT NOT NULL UNIQUE,  -- hash of high-entropy token; NOT password hash
  expires_at TEXT NOT NULL,
  revoked_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE folders (
  folder_id TEXT PRIMARY KEY,
  folder_name TEXT NOT NULL,
  owner_user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  archived_at TEXT
);
CREATE INDEX idx_folders_owner ON folders(owner_user_id);

-- OWNER is deliberately NOT copied into memberships. folders.owner_user_id is authoritative.
CREATE TABLE folder_members (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  role TEXT NOT NULL CHECK(role IN ('CONTRIBUTOR','VIEWER','UPLOADER')),
  joined_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY(folder_id,user_id)
);
CREATE INDEX idx_members_by_user ON folder_members(user_id,folder_id);

CREATE TRIGGER no_owner_membership_insert
BEFORE INSERT ON folder_members
WHEN EXISTS(SELECT 1 FROM folders WHERE folder_id=NEW.folder_id AND owner_user_id=NEW.user_id)
BEGIN SELECT RAISE(ABORT,'owner cannot be a member'); END;
CREATE TRIGGER no_owner_membership_update
BEFORE UPDATE OF user_id,folder_id ON folder_members
WHEN EXISTS(SELECT 1 FROM folders WHERE folder_id=NEW.folder_id AND owner_user_id=NEW.user_id)
BEGIN SELECT RAISE(ABORT,'owner cannot be a member'); END;
CREATE TRIGGER owner_must_be_active_insert
BEFORE INSERT ON folders
WHEN NOT EXISTS(SELECT 1 FROM users WHERE user_id=NEW.owner_user_id AND status='active')
BEGIN SELECT RAISE(ABORT,'owner must be active'); END;
CREATE TRIGGER owner_must_be_active_update
BEFORE UPDATE OF owner_user_id ON folders
WHEN NOT EXISTS(SELECT 1 FROM users WHERE user_id=NEW.owner_user_id AND status='active')
BEGIN SELECT RAISE(ABORT,'owner must be active'); END;
CREATE TRIGGER cannot_disable_owner
BEFORE UPDATE OF status ON users
WHEN NEW.status!='active' AND EXISTS(SELECT 1 FROM folders WHERE owner_user_id=NEW.user_id)
BEGIN SELECT RAISE(ABORT,'transfer owned albums before disabling account'); END;

CREATE TABLE folder_invitations (
  invitation_id TEXT PRIMARY KEY,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  invited_user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  inviter_user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  role TEXT NOT NULL CHECK(role IN ('CONTRIBUTOR','VIEWER','UPLOADER')),
  token_sha256 TEXT UNIQUE, -- optional one-time high entropy invitation secret
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','accepted','revoked','expired')),
  expires_at TEXT NOT NULL,
  accepted_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_invites_by_recipient ON folder_invitations(invited_user_id,status,expires_at);

CREATE TABLE blobs (
  sha256 TEXT PRIMARY KEY CHECK(length(sha256)=64),
  bytes INTEGER NOT NULL CHECK(bytes>0),
  mime_type TEXT NOT NULL,
  relative_path TEXT NOT NULL UNIQUE,
  verified_at TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE assets (
  asset_id TEXT PRIMARY KEY,  -- random opaque ID; never the predictable CAS SHA
  original_sha256 TEXT NOT NULL REFERENCES blobs(sha256),
  original_filename TEXT NOT NULL,
  media_kind TEXT NOT NULL CHECK(media_kind IN ('image','video','live_photo','raw','other')),
  preview_sha256 TEXT REFERENCES blobs(sha256),
  uploaded_by_user_id TEXT REFERENCES users(user_id) ON DELETE SET NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_asset_blob ON assets(original_sha256);
CREATE TABLE folder_assets (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE RESTRICT,
  added_by_user_id TEXT REFERENCES users(user_id) ON DELETE SET NULL,
  added_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  removed_at TEXT,
  PRIMARY KEY(folder_id,asset_id)
);
CREATE INDEX idx_folder_assets_asset ON folder_assets(asset_id);

CREATE TABLE folder_sequences (
  folder_id TEXT PRIMARY KEY REFERENCES folders(folder_id) ON DELETE CASCADE,
  last_seq INTEGER NOT NULL DEFAULT 0 CHECK(last_seq >= 0)
);
CREATE TABLE folder_changes (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  seq INTEGER NOT NULL CHECK(seq > 0),
  event_type TEXT NOT NULL CHECK(event_type IN ('asset_added','asset_removed','asset_updated')),
  asset_id TEXT NOT NULL,
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY(folder_id,seq)
);
CREATE TABLE device_sync_cursors (
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE CASCADE,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  applied_seq INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY(device_id,folder_id)
);
CREATE TABLE upload_receipts (
  receipt_id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  device_id TEXT NOT NULL REFERENCES devices(device_id) ON DELETE RESTRICT,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  request_id TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK(outcome IN ('accepted','failed')),
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(device_id,request_id)
);

CREATE TABLE audit_log (
  event_id TEXT PRIMARY KEY,
  actor_user_id TEXT REFERENCES users(user_id) ON DELETE SET NULL,
  action TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT,
  result TEXT NOT NULL CHECK(result IN ('allowed','denied','error')),
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  details_json TEXT
);

-- Application requirements NOT encoded solely by this DDL:
--  * At least one enabled SERVER_ADMIN must exist after bootstrap.
--  * All owner transfers and invitation acceptances must be atomic and audited.
--  * Global access to blobs/assets forbidden without a readable folder association.
--  * Invitation inviter must be CURRENT folder owner; invited user must be active.
--  * Always authorize at request entry AND immediately before upload commit.
--  * Existing state.json demo requires a separate, tested and backed-up migration.
