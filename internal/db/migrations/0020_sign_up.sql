-- Migration 0020: sign up with Google or Apple (M65).
--
-- An account can now be created from a verified Google or Apple sign-in
-- in two ways, both decided by the server (internal/httpapi/signup.go):
--
-- * with a usable album invite link: the account is active at once and
--   joins that album;
-- * without one: the account is created disabled and a row here asks the
--   administrator to approve it (enable) or reject it (delete). Until then
--   signing in answers sign_up_pending.
--
-- Such an account has a password nobody knows (a random one, hashed like
-- any other), so it signs in only with its linked identity.
CREATE TABLE sign_up_requests (
  user_id TEXT PRIMARY KEY REFERENCES users(user_id) ON DELETE CASCADE,
  provider TEXT NOT NULL CHECK(provider IN ('google','apple')),
  email TEXT CHECK(email IS NULL OR length(email) <= 320),
  requested_at TEXT NOT NULL
);
