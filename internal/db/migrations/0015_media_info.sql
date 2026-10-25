-- Migration 0015: the camera and pixel size of each original.
--
-- Like the capture time (0010) and the location (0013) these are
-- properties of the bytes: the capture-time worker reads them from the
-- original's own metadata (internal/capturetime.ExtractMediaInfo: EXIF
-- Make, Model, LensModel, FocalLengthIn35mmFilm, the JPEG frame header,
-- the HEIF ispe/irot of the primary item, a movie's track header) after
-- the commit. The original is never changed.
--
--   media_state        'pending' until the worker has looked, then 'done'.
--                      Every existing row starts pending: the backfill.
--   camera_make/_model/_lens  as recorded (printable, at most 64 bytes),
--                      NULL when not recorded.
--   camera_focal_35mm  focal length in 35 mm terms (whole mm), NULL when
--                      not recorded.
--   pixel_width/_height  the size as displayed (after the recorded
--                      orientation), both NULL when unknown.
ALTER TABLE blobs ADD COLUMN media_state TEXT NOT NULL DEFAULT 'pending' CHECK(media_state IN ('pending','done'));
ALTER TABLE blobs ADD COLUMN camera_make TEXT CHECK(camera_make IS NULL OR (media_state = 'done' AND length(camera_make) BETWEEN 1 AND 64));
ALTER TABLE blobs ADD COLUMN camera_model TEXT CHECK(camera_model IS NULL OR (media_state = 'done' AND length(camera_model) BETWEEN 1 AND 64));
ALTER TABLE blobs ADD COLUMN camera_lens TEXT CHECK(camera_lens IS NULL OR (media_state = 'done' AND length(camera_lens) BETWEEN 1 AND 64));
ALTER TABLE blobs ADD COLUMN camera_focal_35mm INTEGER CHECK(camera_focal_35mm IS NULL OR (media_state = 'done' AND camera_focal_35mm BETWEEN 1 AND 10000));
ALTER TABLE blobs ADD COLUMN pixel_width INTEGER CHECK(pixel_width IS NULL OR (media_state = 'done' AND pixel_width BETWEEN 1 AND 131072));
ALTER TABLE blobs ADD COLUMN pixel_height INTEGER CHECK((pixel_height IS NULL) = (pixel_width IS NULL) AND (pixel_height IS NULL OR pixel_height BETWEEN 1 AND 131072));
CREATE INDEX idx_blobs_media_pending ON blobs(sha256) WHERE media_state = 'pending';
