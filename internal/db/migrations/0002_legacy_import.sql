-- Migration 0002: bookkeeping for importing the pre-SQLite PoC state.json.
-- The import itself is a separate, explicit command; this only adds the tables
-- and columns needed to keep that import lossless and auditable.

-- One row per successful import run.
CREATE TABLE legacy_imports (
  import_id TEXT PRIMARY KEY,
  source_sha256 TEXT NOT NULL CHECK(length(source_sha256)=64), -- SHA-256 of the imported state.json bytes
  imported_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  report_json TEXT NOT NULL
);

-- Legacy assets were identified by the SHA-256 of their bytes. The import gives
-- each one a random asset_id and keeps the old identifier here.
CREATE TABLE legacy_asset_map (
  legacy_sha256 TEXT PRIMARY KEY CHECK(length(legacy_sha256)=64),
  asset_id TEXT NOT NULL UNIQUE REFERENCES assets(asset_id) ON DELETE RESTRICT
);

-- The PoC accepted a free-form X-Device-ID string per upload, with no registered
-- device row. Keep that string verbatim instead of inventing devices.
ALTER TABLE folder_assets ADD COLUMN uploader_device_label TEXT;
ALTER TABLE folder_changes ADD COLUMN uploader_device_label TEXT;
