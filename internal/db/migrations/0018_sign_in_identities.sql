-- Migration 0018: Google and Apple sign-in (M54).
--
-- An account can link one Google and one Apple identity and then sign in
-- with it instead of its password. The server verifies the provider's ID
-- token itself (internal/auth/oidc) and keeps only the provider's stable
-- subject identifier and the e-mail address the token carried when the
-- link was made (shown to the account itself, never used to match
-- accounts). Linking needs the account's password; nothing creates an
-- account from a sign-in.
CREATE TABLE user_identities (
  provider TEXT NOT NULL CHECK(provider IN ('google','apple')),
  subject TEXT NOT NULL CHECK(length(subject) BETWEEN 1 AND 255),
  user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
  email TEXT CHECK(email IS NULL OR length(email) <= 320),
  linked_at TEXT NOT NULL,
  last_used_at TEXT,
  PRIMARY KEY (provider, subject),
  UNIQUE (user_id, provider)
);
