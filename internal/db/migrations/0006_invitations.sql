-- Migration 0006: invitation bookkeeping (sqlitestore memberships).
-- folder_invitations (schema v1.4) is used from now on. At most one pending
-- invitation may exist per folder and invited account; an expired pending
-- row is marked 'expired' before a new invitation for the same account is
-- written. token_sha256 stays NULL: an invitation names one registered
-- account and is accepted with that account's own session, so it carries no
-- bearer secret (public invitation links are not supported).

CREATE UNIQUE INDEX idx_invites_one_pending ON folder_invitations(folder_id, invited_user_id) WHERE status = 'pending';
CREATE INDEX idx_invites_by_folder ON folder_invitations(folder_id, status);
