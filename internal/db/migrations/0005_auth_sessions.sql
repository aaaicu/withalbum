-- Migration 0005: login sessions (internal/auth/session).
-- auth_sessions (schema v1.4) only held the refresh token hash. A session now
-- also carries its current short-lived access token, the CSRF token of a
-- browser session, and remembers the refresh tokens it has rotated away so
-- that a replayed (stolen) refresh token revokes the whole session.
-- Every token column holds the SHA-256 (hex) of a 256-bit random token, never
-- the token itself.

ALTER TABLE auth_sessions ADD COLUMN access_token_sha256 TEXT;
ALTER TABLE auth_sessions ADD COLUMN access_expires_at TEXT;
ALTER TABLE auth_sessions ADD COLUMN csrf_token_sha256 TEXT; -- NULL: not a browser (cookie) session
ALTER TABLE auth_sessions ADD COLUMN refreshed_at TEXT;
ALTER TABLE auth_sessions ADD COLUMN revoke_reason TEXT;

CREATE UNIQUE INDEX idx_auth_sessions_access ON auth_sessions(access_token_sha256);
CREATE INDEX idx_auth_sessions_device ON auth_sessions(device_id);
CREATE INDEX idx_devices_user ON devices(user_id);

-- Refresh tokens already exchanged. Presenting one again means it was copied:
-- the session is revoked. Rows past expires_at can be pruned (the token would
-- be refused as expired anyway).
CREATE TABLE auth_refresh_used (
  token_sha256 TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES auth_sessions(session_id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL
);
CREATE INDEX idx_auth_refresh_used_session ON auth_refresh_used(session_id);
