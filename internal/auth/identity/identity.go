// Package identity keeps the Google and Apple identities linked to
// accounts (M54, table user_identities). An account links at most one
// identity per provider; an identity belongs to at most one account. The
// caller verifies the provider's ID token (internal/auth/oidc) and the
// account's password before linking; this package only stores and audits.
package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/audit"
)

var (
	// ErrInUse: the identity is linked to another account.
	ErrInUse = errors.New("identity linked to another account")
	// ErrProviderLinked: the account already linked another identity of
	// this provider (unlink it first).
	ErrProviderLinked = errors.New("account already linked to this provider")
	// ErrNotFound: the account has no identity of this provider.
	ErrNotFound = errors.New("no linked identity for this provider")
)

// Link is one linked identity as the account itself sees it. The
// provider's subject identifier is not shown.
type Link struct {
	Provider   string
	Email      string
	LinkedAt   time.Time
	LastUsedAt time.Time // zero: never used to sign in
}

// Store reads and writes links in a migrated database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New returns a Store.
func New(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

const timeLayout = "2006-01-02T15:04:05.000000Z"

func parse(s sql.NullString) time.Time {
	if !s.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(timeLayout, s.String)
	return t
}

// List returns the account's links in provider order (google, apple).
func (s *Store) List(ctx context.Context, userID string) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, email, linked_at, last_used_at FROM user_identities
		WHERE user_id = ? ORDER BY CASE provider WHEN 'google' THEN 0 ELSE 1 END`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		var l Link
		var email, linked, used sql.NullString
		if err := rows.Scan(&l.Provider, &email, &linked, &used); err != nil {
			return nil, err
		}
		l.Email, l.LinkedAt, l.LastUsedAt = email.String, parse(linked), parse(used)
		out = append(out, l)
	}
	return out, rows.Err()
}

// Details are request facts for the audit log.
type Details map[string]any

// Add links (provider, subject) to userID. Linking the same identity again
// is not an error and returns the existing link.
func (s *Store) Add(ctx context.Context, userID, provider, subject, email string, d Details) (Link, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	var owner, mine string
	err = tx.QueryRowContext(ctx, "SELECT user_id FROM user_identities WHERE provider = ? AND subject = ?", provider, subject).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Link{}, err
	}
	err = tx.QueryRowContext(ctx, "SELECT subject FROM user_identities WHERE user_id = ? AND provider = ?", userID, provider).Scan(&mine)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Link{}, err
	}
	switch {
	case owner == userID:
		if err := tx.Commit(); err != nil {
			return Link{}, err
		}
		return s.get(ctx, userID, provider)
	case owner != "":
		if err := s.deny(ctx, tx, userID, provider, "identity_in_use", d); err != nil {
			return Link{}, err
		}
		return Link{}, ErrInUse
	case mine != "":
		if err := s.deny(ctx, tx, userID, provider, "provider_linked", d); err != nil {
			return Link{}, err
		}
		return Link{}, ErrProviderLinked
	}
	email = strings.TrimSpace(email)
	if len(email) > 320 {
		email = ""
	}
	var em sql.NullString
	if email != "" {
		em = sql.NullString{String: email, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO user_identities (provider, subject, user_id, email, linked_at) VALUES (?, ?, ?, ?, ?)",
		provider, subject, userID, em, s.now().UTC().Format(timeLayout)); err != nil {
		return Link{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "account.identity_link", TargetType: "user", TargetID: userID,
		Result: audit.Allowed, Details: with(d, map[string]any{"provider": provider})}); err != nil {
		return Link{}, err
	}
	if err := tx.Commit(); err != nil {
		return Link{}, err
	}
	return s.get(ctx, userID, provider)
}

func (s *Store) deny(ctx context.Context, tx *sql.Tx, userID, provider, reason string, d Details) error {
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "account.identity_link", TargetType: "user", TargetID: userID,
		Result: audit.Denied, Details: with(d, map[string]any{"provider": provider, "reason": reason})}); err != nil {
		return err
	}
	return tx.Commit()
}

func with(d Details, m map[string]any) map[string]any {
	for k, v := range d {
		m[k] = v
	}
	return m
}

func (s *Store) get(ctx context.Context, userID, provider string) (Link, error) {
	links, err := s.List(ctx, userID)
	if err != nil {
		return Link{}, err
	}
	for _, l := range links {
		if l.Provider == provider {
			return l, nil
		}
	}
	return Link{}, ErrNotFound
}

// Remove unlinks the account's identity of provider. Sessions made with it
// stay valid (like sessions after linking); revoke devices to end them.
func (s *Store) Remove(ctx context.Context, userID, provider string, d Details) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM user_identities WHERE user_id = ? AND provider = ?", userID, provider)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "account.identity_unlink", TargetType: "user", TargetID: userID,
		Result: audit.Allowed, Details: with(d, map[string]any{"provider": provider})}); err != nil {
		return err
	}
	return tx.Commit()
}
