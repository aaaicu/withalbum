-- Migration 0013: where each original was taken, and the album setting
-- that hides it from members.
--
-- Like the capture time (0010) the location is a property of the bytes:
-- the capture-time worker reads it from the original's own metadata
-- (internal/capturetime.ExtractLocation: the EXIF GPS directory, the
-- QuickTime ISO 6709 location) after the commit. The original is never
-- changed: its bytes, GPS tags included, stay exactly as uploaded.
--
--   location_state       'pending' until the worker has looked, then
--                        'done'. Every existing row starts pending, which
--                        is the backfill.
--   location_lat/_lon    WGS 84 degrees, both NULL when the original
--                        records no readable location. Never guessed.
--   location_accuracy_m  the recorded horizontal accuracy in metres, NULL
--                        when the original gives none.
ALTER TABLE blobs ADD COLUMN location_state TEXT NOT NULL DEFAULT 'pending' CHECK(location_state IN ('pending','done'));
ALTER TABLE blobs ADD COLUMN location_lat REAL CHECK(location_lat IS NULL OR (location_state = 'done' AND location_lat BETWEEN -90 AND 90));
ALTER TABLE blobs ADD COLUMN location_lon REAL CHECK((location_lon IS NULL) = (location_lat IS NULL) AND (location_lon IS NULL OR location_lon BETWEEN -180 AND 180));
ALTER TABLE blobs ADD COLUMN location_accuracy_m REAL CHECK(location_accuracy_m IS NULL OR (location_lat IS NOT NULL AND location_accuracy_m > 0));
CREATE INDEX idx_blobs_location_pending ON blobs(sha256) WHERE location_state = 'pending';

-- hide_location: set by the album's owner. When 1, members (CONTRIBUTOR,
-- VIEWER) get "location": null for every asset of the folder; the owner
-- still sees it. An UPLOADER never sees any asset metadata. It only hides
-- the API field: originals keep their GPS tags, so whoever may download an
-- original can read them from the file.
ALTER TABLE folders ADD COLUMN hide_location INTEGER NOT NULL DEFAULT 0 CHECK(hide_location IN (0, 1));

-- hide_my_location: each account's own choice for the photos it uploaded.
-- When 1, every other reader gets "location": null for the assets this
-- account uploaded (in every album); the account itself still sees them.
-- Like hide_location it hides the API field only, not the GPS tags in
-- the originals.
ALTER TABLE users ADD COLUMN hide_my_location INTEGER NOT NULL DEFAULT 0 CHECK(hide_my_location IN (0, 1));
