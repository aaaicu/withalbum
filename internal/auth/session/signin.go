package session

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aaaicu/withalbum/internal/audit"
)

// ErrNotLinked: no account has linked this provider identity (M54).
var ErrNotLinked = errors.New("identity not linked to an account")

// SignInLinked creates a device and session for the account that linked
// (provider, subject), after the caller verified the provider's ID token
// (internal/auth/oidc). A disabled account is ErrInvalidCredentials, like
// a password login; an identity nobody linked is ErrNotLinked. Both are
// recorded in the audit log.
func (s *Service) SignInLinked(ctx context.Context, provider, subject string, req LoginRequest) (Tokens, Identity, error) {
	if err := ValidateDevice(req.DeviceName, req.Platform); err != nil {
		return Tokens{}, Identity{}, err
	}
	req.Method = provider
	var userID, status string
	err := s.db.QueryRowContext(ctx, `SELECT u.user_id, u.status FROM user_identities i JOIN users u ON u.user_id = i.user_id
		WHERE i.provider = ? AND i.subject = ?`, provider, subject).Scan(&userID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		if err := s.auditSignInFailure(ctx, "", req, "not_linked"); err != nil {
			return Tokens{}, Identity{}, err
		}
		return Tokens{}, Identity{}, ErrNotLinked
	}
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	if status != "active" {
		if err := s.auditSignInFailure(ctx, userID, req, string(FailDisabled)); err != nil {
			return Tokens{}, Identity{}, err
		}
		return Tokens{}, Identity{}, ErrInvalidCredentials
	}
	tok, err := s.create(ctx, userID, req)
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE user_identities SET last_used_at = ? WHERE provider = ? AND subject = ?",
		fmtTime(s.now()), provider, subject); err != nil {
		return Tokens{}, Identity{}, err
	}
	id, err := s.Authenticate(ctx, tok.AccessToken)
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	return tok, id, nil
}

func (s *Service) auditSignInFailure(ctx context.Context, userID string, req LoginRequest, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "auth.login", TargetType: "user", TargetID: userID,
		Result: audit.Denied, Details: req.Client.details(map[string]any{"method": req.Method, "reason": reason,
			"platform": req.Platform})}); err != nil {
		return err
	}
	return tx.Commit()
}
