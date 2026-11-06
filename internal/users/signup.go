package users

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/aaaicu/withalbum/internal/auth/password"
)

// Sign up with Google or Apple (M65). The caller (internal/httpapi) has
// verified the provider's ID token and decided how the account starts:
// active (an album invite link) or pending the administrator's approval.

// External is the verified provider identity a new account is made from.
type External struct {
	Provider string // "google" or "apple"
	Subject  string
	Email    string // as the token carried it; may be empty
	// Pending: create the account disabled with a sign-up request.
	Pending bool
}

// SignUpRequest is an account waiting for the administrator.
type SignUpRequest struct {
	User        User
	Provider    string
	Email       string
	RequestedAt string
}

var (
	// ErrIdentityTaken: another account already linked this identity.
	ErrIdentityTaken = errors.New("identity is already linked to an account")
	// ErrNoSignUpRequest: the account has no pending sign-up request.
	ErrNoSignUpRequest = errors.New("no pending sign-up request for this account")
	// ErrTooManyRequests: MaxPendingSignUps requests are already waiting.
	ErrTooManyRequests = errors.New("too many sign-up requests are waiting for approval")
)

// MaxPendingSignUps bounds the waiting requests, so an open server cannot
// be filled with accounts nobody approves.
const MaxPendingSignUps = 20

// CreateExternal adds a USER account linked to ext in one transaction:
// the account (active, or disabled with a sign-up request when
// ext.Pending), the identity link and the audit rows. n.PasswordHash must
// be the hash of a password nobody knows. The identity may not be linked
// yet (ErrIdentityTaken); the login name must be free (ErrLoginTaken).
func (s *Store) CreateExternal(ctx context.Context, n NewUser, ext External, actor Actor) (User, error) {
	if err := ValidateLoginName(n.LoginName); err != nil {
		return User{}, err
	}
	if err := ValidateDisplayName(n.DisplayName); err != nil {
		return User{}, err
	}
	if !password.Valid(n.PasswordHash) {
		return User{}, ErrInvalidHash
	}
	id, err := newID()
	if err != nil {
		return User{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if _, claimed, err := bootstrapState(ctx, tx); err != nil {
		return User{}, err
	} else if !claimed {
		return User{}, ErrNotInitialized
	}
	if taken, err := loginTaken(ctx, tx, n.LoginName, ""); err != nil {
		return User{}, err
	} else if taken {
		return User{}, ErrLoginTaken
	}
	var owner string
	err = tx.QueryRowContext(ctx, "SELECT user_id FROM user_identities WHERE provider = ? AND subject = ?", ext.Provider, ext.Subject).Scan(&owner)
	if err == nil {
		return User{}, ErrIdentityTaken
	} else if !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}
	if ext.Pending {
		var waiting int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sign_up_requests").Scan(&waiting); err != nil {
			return User{}, err
		}
		if waiting >= MaxPendingSignUps {
			return User{}, ErrTooManyRequests
		}
	}
	status := StatusActive
	if ext.Pending {
		status = StatusDisabled
	}
	at := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users
		(user_id, login_name, display_name, password_hash, server_role, can_create_albums, status, created_at)
		VALUES (?, ?, ?, ?, 'USER', 0, ?, ?)`, id, n.LoginName, n.DisplayName, n.PasswordHash, string(status), at); err != nil {
		return User{}, err
	}
	email := strings.TrimSpace(ext.Email)
	if len(email) > 320 {
		email = ""
	}
	em := sql.NullString{String: email, Valid: email != ""}
	if _, err := tx.ExecContext(ctx, "INSERT INTO user_identities (provider, subject, user_id, email, linked_at) VALUES (?, ?, ?, ?, ?)",
		ext.Provider, ext.Subject, id, em, at); err != nil {
		return User{}, err
	}
	if ext.Pending {
		if _, err := tx.ExecContext(ctx, "INSERT INTO sign_up_requests (user_id, provider, email, requested_at) VALUES (?, ?, ?, ?)",
			id, ext.Provider, em, at); err != nil {
			return User{}, err
		}
	}
	if err := record(ctx, tx, actor, "account.sign_up", id, map[string]any{
		"login": n.LoginName, "provider": ext.Provider, "pending": ext.Pending}); err != nil {
		return User{}, err
	}
	u, err := getUser(ctx, tx, id)
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// PendingSignUp reports whether the account waits for approval.
func (s *Store) PendingSignUp(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sign_up_requests WHERE user_id = ?", id).Scan(&n)
	return n > 0, err
}

// SignUpRequests lists the waiting requests, oldest first.
func (s *Store) SignUpRequests(ctx context.Context) ([]SignUpRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.provider, coalesce(r.email, ''), r.requested_at, u.user_id, u.login_name, u.display_name, u.server_role,
		u.can_create_albums, u.status, u.created_at, substr(u.password_hash, 1, 1) = '`+password.LockedPrefix+`'

		FROM sign_up_requests r JOIN users u ON u.user_id = r.user_id ORDER BY r.requested_at, r.user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignUpRequest
	for rows.Next() {
		var r SignUpRequest
		var canCreate int
		u := &r.User
		if err := rows.Scan(&r.Provider, &r.Email, &r.RequestedAt, &u.ID, &u.LoginName, &u.DisplayName, &u.Role, &canCreate,
			&u.Status, &u.CreatedAt, &u.PasswordLocked); err != nil {
			return nil, err
		}
		u.CanCreateAlbums = canCreate == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApproveSignUp enables a waiting account and drops its request.
func (s *Store) ApproveSignUp(ctx context.Context, id string, actor Actor) error {
	return s.change(ctx, id, func(tx *sql.Tx, u User) error {
		if err := dropRequest(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET status = 'active' WHERE user_id = ?", id); err != nil {
			return err
		}
		return record(ctx, tx, actor, "account.sign_up_approve", id, map[string]any{"login": u.LoginName})
	})
}

// RejectSignUp deletes a waiting account (it never signed in, so nothing
// else refers to it) with its identity link and request.
func (s *Store) RejectSignUp(ctx context.Context, id string, actor Actor) error {
	return s.change(ctx, id, func(tx *sql.Tx, u User) error {
		if err := dropRequest(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE user_id = ?", id); err != nil {
			return err
		}
		return record(ctx, tx, actor, "account.sign_up_reject", id, map[string]any{"login": u.LoginName})
	})
}

func dropRequest(ctx context.Context, tx *sql.Tx, id string) error {
	res, err := tx.ExecContext(ctx, "DELETE FROM sign_up_requests WHERE user_id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSignUpRequest
	}
	return nil
}
