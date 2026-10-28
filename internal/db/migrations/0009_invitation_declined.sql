-- Migration 0009: an invitee can decline an invitation.
--
-- folder_invitations.status gains 'declined'. SQLite cannot change a CHECK
-- constraint in place, so the table is rebuilt: the old one is renamed, the
-- new one created with the same columns, every row copied unchanged, the
-- old one dropped (its indexes with it) and the indexes created again. No
-- other table, trigger or view refers to folder_invitations.
--
-- A declined invitation is no longer pending, so the partial UNIQUE index
-- (one pending invitation per folder and account, migration 0006) lets the
-- owner invite the same account again.

ALTER TABLE folder_invitations RENAME TO folder_invitations_0008;

CREATE TABLE folder_invitations (
  invitation_id TEXT PRIMARY KEY,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  invited_user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  inviter_user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  role TEXT NOT NULL CHECK(role IN ('CONTRIBUTOR','VIEWER','UPLOADER')),
  token_sha256 TEXT UNIQUE, -- optional one-time high entropy invitation secret
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','accepted','revoked','expired','declined')),
  expires_at TEXT NOT NULL,
  accepted_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO folder_invitations
  (invitation_id, folder_id, invited_user_id, inviter_user_id, role, token_sha256, status, expires_at, accepted_at, created_at)
  SELECT invitation_id, folder_id, invited_user_id, inviter_user_id, role, token_sha256, status, expires_at, accepted_at, created_at
  FROM folder_invitations_0008;

DROP TABLE folder_invitations_0008;

CREATE INDEX idx_invites_by_recipient ON folder_invitations(invited_user_id,status,expires_at);
CREATE UNIQUE INDEX idx_invites_one_pending ON folder_invitations(folder_id, invited_user_id) WHERE status = 'pending';
CREATE INDEX idx_invites_by_folder ON folder_invitations(folder_id, status);
