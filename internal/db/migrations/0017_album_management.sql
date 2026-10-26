-- Migration 0017: album management (M51) — deleted albums, the album trash
-- and hidden assets.
--
-- A deleted album (folders.deleted_at) is hidden from everyone like an
-- archived one, but its owner can restore it for 30 days; after that the
-- server purges its rows (internal/meta/sqlitestore, albums.go) and the
-- originals no other asset holds. Archiving (archived_at) is unchanged.
ALTER TABLE folders ADD COLUMN deleted_at TEXT;
CREATE INDEX idx_folders_deleted ON folders(deleted_at) WHERE deleted_at IS NOT NULL;

-- The album trash. An asset removed from a folder (removed_at) keeps its
-- likes and comments and can be restored until it is purged (purged_at):
-- 30 days later the server deletes its likes and comments and the removal
-- becomes final. Removals made before this migration already dropped their
-- likes and comments; they count as purged.
ALTER TABLE folder_assets ADD COLUMN removed_by_user_id TEXT REFERENCES users(user_id) ON DELETE SET NULL;
ALTER TABLE folder_assets ADD COLUMN purged_at TEXT;
UPDATE folder_assets SET purged_at = removed_at WHERE removed_at IS NOT NULL;
CREATE INDEX idx_folder_assets_trash ON folder_assets(folder_id, removed_at) WHERE removed_at IS NOT NULL AND purged_at IS NULL;

-- A purged removal is final, and only a removed link can be purged.
CREATE TRIGGER folder_assets_purged_final BEFORE UPDATE OF removed_at, purged_at ON folder_assets
WHEN OLD.purged_at IS NOT NULL AND (NEW.removed_at IS NOT OLD.removed_at OR NEW.purged_at IS NOT OLD.purged_at)
BEGIN SELECT RAISE(ABORT, 'a purged removal is final'); END;
CREATE TRIGGER folder_assets_purge_removed BEFORE UPDATE OF purged_at ON folder_assets
WHEN NEW.purged_at IS NOT NULL AND NEW.removed_at IS NULL
BEGIN SELECT RAISE(ABORT, 'only a removed asset can be purged'); END;

-- Hidden assets: the folder's OWNER and CONTRIBUTORs see them (marked
-- hidden), nobody else does. Per folder link, like everything an album
-- shows.
ALTER TABLE folder_assets ADD COLUMN hidden_at TEXT;
ALTER TABLE folder_assets ADD COLUMN hidden_by_user_id TEXT REFERENCES users(user_id) ON DELETE SET NULL;

-- An account that owns only deleted albums can be disabled: nobody can
-- use those albums, and they are purged after 30 days (restoring one needs
-- its owner to be able to log in again).
DROP TRIGGER cannot_disable_owner;
CREATE TRIGGER cannot_disable_owner
BEFORE UPDATE OF status ON users
WHEN NEW.status!='active' AND EXISTS(SELECT 1 FROM folders WHERE owner_user_id=NEW.user_id AND deleted_at IS NULL)
BEGIN SELECT RAISE(ABORT,'transfer owned albums before disabling account'); END;

-- The change feed gains asset_hidden and asset_unhidden. They are never
-- shown under these names: the server turns them into asset_updated for
-- the OWNER and CONTRIBUTORs and into asset_removed / asset_added for
-- everyone else. The table is rebuilt as in 0014: renamed, created again,
-- every row copied unchanged, the old one dropped.
ALTER TABLE folder_changes RENAME TO folder_changes_prev;

CREATE TABLE folder_changes (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  seq INTEGER NOT NULL CHECK(seq > 0),
  event_type TEXT NOT NULL CHECK(event_type IN ('asset_added','asset_removed','asset_updated',
    'likes_updated','comment_added','comment_updated','comment_removed','asset_hidden','asset_unhidden')),
  asset_id TEXT NOT NULL,
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  uploader_device_label TEXT,
  comment_id TEXT CHECK((comment_id IS NOT NULL) = (event_type IN ('comment_added','comment_updated','comment_removed'))),
  PRIMARY KEY(folder_id, seq)
);

INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label, comment_id)
  SELECT folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label, comment_id FROM folder_changes_prev;

DROP TABLE folder_changes_prev;
