-- Migration 0003: per-asset "preview is ready" flag.
-- The current preview pipeline writes .system/previews/<xx>/<original sha>.jpg,
-- which is not content-addressed, so assets.preview_sha256 (a blobs reference)
-- cannot describe it. This flag records whether that file was generated.
-- preview_sha256 stays NULL until previews become CAS blobs.

ALTER TABLE assets ADD COLUMN preview_ready INTEGER NOT NULL DEFAULT 0 CHECK(preview_ready IN (0,1));
