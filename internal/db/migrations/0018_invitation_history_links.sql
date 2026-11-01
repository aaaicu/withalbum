-- Migration 0018: invitation history and album invite links (M57).
--
-- folder_invitations keeps every invitation (it never deleted any); the
-- invitee and the owner now see the recent answered ones too. The time
-- of a decline and of a revocation, and why an invitation was revoked,
-- are recorded from now on. Rows written before this migration have no
-- such time or reason (NULL): nobody knows them, so none is invented.
ALTER TABLE folder_invitations ADD COLUMN declined_at TEXT;
ALTER TABLE folder_invitations ADD COLUMN revoked_at TEXT;
ALTER TABLE folder_invitations ADD COLUMN revoke_reason TEXT
  CHECK(revoke_reason IS NULL OR revoke_reason IN ('revoked_by_owner','album_deleted','album_archived','owner_changed','replaced'));

-- An invite link: a high-entropy bearer token (only its SHA-256 is
-- stored, like refresh tokens) that the album's owner shares, for example
-- through a messenger. Any signed-in account that redeems it before it
-- expires becomes a member with the link's role, up to max_uses times.
-- It is revoked by its owner, and like pending invitations when the
-- album is deleted or archived or changes owner.
CREATE TABLE invite_links (
  link_id TEXT PRIMARY KEY,
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  creator_user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
  role TEXT NOT NULL CHECK(role IN ('CONTRIBUTOR','VIEWER','UPLOADER')),
  token_sha256 TEXT NOT NULL UNIQUE CHECK(length(token_sha256) = 64),
  max_uses INTEGER NOT NULL CHECK(max_uses BETWEEN 1 AND 20),
  use_count INTEGER NOT NULL DEFAULT 0 CHECK(use_count >= 0 AND use_count <= max_uses),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  last_used_at TEXT,
  revoked_at TEXT,
  revoke_reason TEXT CHECK((revoked_at IS NULL) = (revoke_reason IS NULL)
    AND (revoke_reason IS NULL OR revoke_reason IN ('revoked_by_owner','album_deleted','album_archived','owner_changed')))
);
CREATE INDEX idx_invite_links_folder ON invite_links(folder_id, created_at);

-- An invitation created by redeeming a link names that link. It is
-- written accepted (the redeeming account answered by redeeming).
ALTER TABLE folder_invitations ADD COLUMN link_id TEXT REFERENCES invite_links(link_id) ON DELETE SET NULL;
CREATE INDEX idx_invites_by_link ON folder_invitations(link_id) WHERE link_id IS NOT NULL;
