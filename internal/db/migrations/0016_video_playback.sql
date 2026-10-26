-- Migration 0016: playback copies of videos.
--
-- A playback copy is a smaller MP4 (H.264 + AAC, at most 1080p, moov
-- before mdat) that the playback worker (internal/httpapi playback.go)
-- makes from a video original with an external tool, so that clients can
-- stream a video without buffering. It is derived data like a preview: it
-- lives in .system/playback/<sha[:2]>/<sha>.mp4, is never part of a
-- backup, and is made again when it is missing. The original is never
-- changed, and "kept" (the server verified the SHA-256) is only ever said
-- of the original.
--
-- One row per original that is (or was) the original of a video asset.
-- Live Photos are not included: their own original is the still, and the
-- paired video is a short clip that is streamed as it is.
--
--   status   'pending'     waiting for the worker (also between retries)
--            'ready'       the copy exists; bytes, width, height are set
--            'failed'      the tool failed attempts times (final)
--            'unsupported' no copy will be made: no tool on this server
--                          (reason no_converter), or the copy would not be
--                          meaningfully smaller than the original
--                          (original_small), or the video is too long
--                          (too_long); clients stream the original
--            'idle'        no video asset in use holds these bytes any
--                          more; the copy was deleted. A new link makes
--                          the row pending again (triggers below).
--   reason   why it failed or is unsupported (short code, no paths).
--   attempts conversion runs that failed (retried with a backoff until
--            the cap, then failed).
--   next_attempt_at  a pending row is not taken before this time.
--   queued_at  when it became pending: newer first, so a new upload is
--            not stuck behind the backfill.
CREATE TABLE blob_playback (
  sha256 TEXT PRIMARY KEY REFERENCES blobs(sha256),
  status TEXT NOT NULL CHECK(status IN ('pending','ready','failed','unsupported','idle')),
  reason TEXT CHECK(reason IS NULL OR (length(reason) BETWEEN 1 AND 32 AND status IN ('failed','unsupported','pending'))),
  attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
  next_attempt_at TEXT CHECK(next_attempt_at IS NULL OR status = 'pending'),
  bytes INTEGER CHECK((bytes IS NULL) = (status <> 'ready') AND (bytes IS NULL OR bytes > 0)),
  width INTEGER CHECK(width IS NULL OR (status = 'ready' AND width BETWEEN 1 AND 8192)),
  height INTEGER CHECK((height IS NULL) = (width IS NULL) AND (height IS NULL OR height BETWEEN 1 AND 8192)),
  queued_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX idx_blob_playback_pending ON blob_playback(queued_at) WHERE status = 'pending';

-- The backfill: every original of a video asset in use.
INSERT INTO blob_playback (sha256, status, queued_at, updated_at)
  SELECT DISTINCT a.original_sha256, 'pending', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
  FROM assets a JOIN folder_assets fa ON fa.asset_id = a.asset_id
  WHERE a.media_kind = 'video' AND fa.removed_at IS NULL;

-- A video asset linked to a folder (an upload, a legacy import) queues its
-- original, in the transaction that links it. An idle row (its copy was
-- deleted when the last link went away) is queued again; any other state
-- is kept (the same bytes already have, or are getting, a copy).
CREATE TRIGGER blob_playback_on_link AFTER INSERT ON folder_assets
WHEN NEW.removed_at IS NULL AND (SELECT media_kind FROM assets WHERE asset_id = NEW.asset_id) = 'video'
BEGIN
  INSERT INTO blob_playback (sha256, status, queued_at, updated_at)
    SELECT original_sha256, 'pending', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now') FROM assets WHERE asset_id = NEW.asset_id
  ON CONFLICT(sha256) DO UPDATE SET status = 'pending', reason = NULL, attempts = 0, next_attempt_at = NULL, bytes = NULL, width = NULL, height = NULL,
    queued_at = excluded.queued_at, updated_at = excluded.updated_at WHERE status = 'idle';
END;

-- The same for a link that comes back (removed_at cleared).
CREATE TRIGGER blob_playback_on_relink AFTER UPDATE OF removed_at ON folder_assets
WHEN OLD.removed_at IS NOT NULL AND NEW.removed_at IS NULL AND (SELECT media_kind FROM assets WHERE asset_id = NEW.asset_id) = 'video'
BEGIN
  INSERT INTO blob_playback (sha256, status, queued_at, updated_at)
    SELECT original_sha256, 'pending', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now') FROM assets WHERE asset_id = NEW.asset_id
  ON CONFLICT(sha256) DO UPDATE SET status = 'pending', reason = NULL, attempts = 0, next_attempt_at = NULL, bytes = NULL, width = NULL, height = NULL,
    queued_at = excluded.queued_at, updated_at = excluded.updated_at WHERE status = 'idle';
END;
