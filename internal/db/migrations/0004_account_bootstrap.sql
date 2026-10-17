-- Migration 0004: identify the bootstrap administrator by user_id.
-- Until now the locked bootstrap account (owner of every PoC/legacy folder)
-- was found by its login name 'admin'. `withalbum-server admin init` takes that
-- account over and may rename it, so its identity is recorded here instead.
-- The row is written together with the account (sqlitestore.EnsureBootstrapOwnerTx).

CREATE TABLE account_bootstrap (
  singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
  user_id TEXT NOT NULL UNIQUE REFERENCES users(user_id) ON DELETE RESTRICT,
  claimed_at TEXT -- set once by `admin init`; NULL while the account is locked
);

-- Existing databases: the account created by earlier versions.
INSERT INTO account_bootstrap (singleton, user_id)
  SELECT 1, user_id FROM users WHERE login_name = 'admin' AND password_hash = '!locked';
