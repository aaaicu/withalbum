-- Migration 0014: likes, comments and mentions on the assets of a folder.
--
-- Everything is keyed by the folder as well as the asset: an asset linked
-- to several folders (PoC imports) has a separate like set and thread in
-- each, so nothing written in one album is shown in another.
--
-- Who is shown (likers, comment authors, mentioned people) is decided when
-- it is read: only accounts that can log in and can read the folder now
-- (owner, CONTRIBUTOR, VIEWER). Rows of people who lost access stay until a
-- cleanup removes them (sqlitestore: likes on removal from the album or a
-- change to UPLOADER; everything of an asset when it leaves the folder).

-- One like per account, asset and folder.
CREATE TABLE asset_likes (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY(folder_id, asset_id, user_id)
);
CREATE INDEX idx_asset_likes_user ON asset_likes(user_id, folder_id);

-- A comment. body is the source text: mentions are tokens "<@user_id>"
-- whose accounts are listed in comment_mentions. client_key is the
-- author's Idempotency-Key of the creating request (NULL without one).
-- A deleted comment is deleted (no tombstone); its mentions go with it.
CREATE TABLE asset_comments (
  comment_id TEXT PRIMARY KEY CHECK(length(comment_id) = 32),
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE CASCADE,
  author_user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  body TEXT NOT NULL CHECK(length(body) > 0 AND length(CAST(body AS BLOB)) <= 16384),
  client_key TEXT,
  created_at TEXT NOT NULL,
  edited_at TEXT,
  UNIQUE(author_user_id, client_key)
);
CREATE INDEX idx_asset_comments_thread ON asset_comments(folder_id, asset_id, created_at, comment_id);
CREATE INDEX idx_asset_comments_author ON asset_comments(author_user_id);

-- The accounts a comment mentions, with the display name they had when
-- the comment was written (shown as plain text once they cannot read the
-- folder any more).
CREATE TABLE comment_mentions (
  comment_id TEXT NOT NULL REFERENCES asset_comments(comment_id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  display_name TEXT NOT NULL,
  PRIMARY KEY(comment_id, user_id)
);
CREATE INDEX idx_comment_mentions_user ON comment_mentions(user_id);

-- "Activity on my uploads" looks assets up by their uploader.
CREATE INDEX idx_assets_uploader ON assets(uploaded_by_user_id);

-- The change feed gains the social event kinds; comment events carry the
-- comment. SQLite cannot change a CHECK constraint in place, so the table
-- is rebuilt as in 0011: renamed, created again, every row copied
-- unchanged, the old one dropped. No other table, index or trigger refers
-- to folder_changes.
ALTER TABLE folder_changes RENAME TO folder_changes_prev;

CREATE TABLE folder_changes (
  folder_id TEXT NOT NULL REFERENCES folders(folder_id) ON DELETE CASCADE,
  seq INTEGER NOT NULL CHECK(seq > 0),
  event_type TEXT NOT NULL CHECK(event_type IN ('asset_added','asset_removed','asset_updated',
    'likes_updated','comment_added','comment_updated','comment_removed')),
  asset_id TEXT NOT NULL,
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  uploader_device_label TEXT,
  comment_id TEXT CHECK((comment_id IS NOT NULL) = (event_type IN ('comment_added','comment_updated','comment_removed'))),
  PRIMARY KEY(folder_id, seq)
);

INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
  SELECT folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label FROM folder_changes_prev;

DROP TABLE folder_changes_prev;
