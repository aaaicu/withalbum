-- Migration 0010: capture time of each original.
--
-- The capture time is a property of the bytes, so it lives on blobs and is
-- shared by every asset (in every folder) of the same original. The server
-- reads it from the original's own metadata (internal/capturetime: EXIF
-- DateTimeOriginal + OffsetTimeOriginal, QuickTime creation date) in a
-- background worker after the commit; the original is never changed.
--
--   capture_state  'pending' until the worker has looked at the original,
--                  then 'done' (whether or not it found a capture time).
--                  Every existing row starts pending, which is the backfill:
--                  the worker works through them after the upgrade, and a
--                  restart simply continues with the rows still pending.
--   captured_at    NULL when unknown (never the upload time), else the
--                  canonical text form (capturetime.Time.String):
--                    2026-10-19T14:03:22+09:00  local time and its offset
--                    2026-10-19T05:03:22Z       an instant, offset unknown
--                    2026-10-19T14:03:22        local time, zone unknown
--   captured_utc   the UTC instant ("...Z") when the original recorded one,
--                  NULL for an unknown or zone-less capture time; for
--                  ordering by capture time.
ALTER TABLE blobs ADD COLUMN capture_state TEXT NOT NULL DEFAULT 'pending' CHECK(capture_state IN ('pending','done'));
ALTER TABLE blobs ADD COLUMN captured_at TEXT CHECK(captured_at IS NULL OR capture_state = 'done');
ALTER TABLE blobs ADD COLUMN captured_utc TEXT CHECK(captured_utc IS NULL OR captured_at IS NOT NULL);
CREATE INDEX idx_blobs_capture_pending ON blobs(sha256) WHERE capture_state = 'pending';
