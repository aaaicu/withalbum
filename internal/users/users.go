// Package users stores server accounts in the SQLite users table.
//
// Rules enforced here (and by the schema triggers):
//
//   - Passwords arrive already hashed (internal/auth/password) and must be a
//     valid Argon2id PHC string; the store never sees a plaintext password and
//     never returns a hash. Hash before calling, outside the transaction, so
//     the write lock is not held during the expensive computation.
//
//   - The bootstrap administrator (account_bootstrap, created locked by
//     sqlitestore and owning every folder of earlier versions) is taken over
//     exactly once by ClaimBootstrap: new login name, display name and
//     password, same user_id, so existing folders keep their owner. Until
//     then the account cannot be changed in any other way, and no other
//     account can be created.
//
//   - At least one usable administrator (active SERVER_ADMIN whose password is
//     not locked) remains after the bootstrap: the last one cannot be
//     disabled or demoted.
//
//   - An account that owns folders cannot be disabled (trigger
//     cannot_disable_owner).
//
//   - Every successful change writes an audit_log row in the same transaction.
//
//   - Resetting a password revokes every session of the account, changing
//     one's own password every session but the current one, and disabling an
//     account all of them, in the same transaction (session.RevokeUserTx).
//     Role and album-creation changes need no revocation: the session layer
//     re-reads the account on every request.
package users

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aaaicu/withalbum/internal/audit"
	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/auth/session"
)

// Role is users.server_role.
type Role string

const (
	RoleServerAdmin Role = "SERVER_ADMIN"
	RoleUser        Role = "USER"
)

// Status is users.status.
type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// User is an account without its password hash.
type User struct {
	ID              string
	LoginName       string
	DisplayName     string
	Role            Role
	CanCreateAlbums bool
	Status          Status
	CreatedAt       string
	// PasswordLocked: the stored password is a locked marker, so the account
	// cannot log in (the bootstrap administrator before ClaimBootstrap).
	PasswordLocked bool
}

// Actor is who makes a change, for the audit log.
type Actor struct {
	UserID string // "" for the server console
	Via    string // "console" or "http"
}

// Console is the operator at the server console (CLI).
var Console = Actor{Via: "console"}

var (
	ErrNotFound           = errors.New("user not found")
	ErrLoginTaken         = errors.New("login name is already in use")
	ErrInvalidLoginName   = errors.New("login name must be 3-32 characters: letters, digits, '.', '_' or '-', starting with a letter or digit")
	ErrInvalidDisplayName = errors.New("display name must be 1-64 characters without control characters")
	ErrInvalidHash        = errors.New("password hash is not a valid Argon2id hash")
	ErrInvalidRole        = errors.New("invalid server role")
	ErrInvalidStatus      = errors.New("invalid account status")
	ErrLastAdmin          = errors.New("refusing to disable or demote the last active server administrator")
	ErrOwnsAlbums         = errors.New("account owns albums; transfer them before disabling it")
	// ErrNoBootstrap: the database has no bootstrap administrator; the
	// server (sqlitestore.Open) creates it on first start.
	ErrNoBootstrap = errors.New("no bootstrap administrator account in the database")
	// ErrAlreadyInitialized: ClaimBootstrap ran before.
	ErrAlreadyInitialized = errors.New("the administrator account has already been set up")
	// ErrNotInitialized: the bootstrap administrator has not been claimed.
	ErrNotInitialized = errors.New("no administrator has been set up yet (run: withalbum-server admin init)")
	// ErrBootstrapUnclaimed: the target is the locked bootstrap account,
	// which only ClaimBootstrap may change.
	ErrBootstrapUnclaimed = errors.New("the locked bootstrap administrator can only be set up with: withalbum-server admin init")
	// ErrBootstrapState: the bootstrap account is not an active
	// SERVER_ADMIN with a locked password, so it is not taken over.
	ErrBootstrapState = errors.New("bootstrap administrator account is in an unexpected state; refusing to take it over")
)

var loginRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`)

// ValidateLoginName checks a login name. Names are unique case-insensitively.
func ValidateLoginName(name string) error {
	if !loginRE.MatchString(name) {
		return ErrInvalidLoginName
	}
	return nil
}

// ValidateDisplayName checks a display name.
func ValidateDisplayName(name string) error {
	if !utf8.ValidString(name) || strings.TrimSpace(name) != name || name == "" || utf8.RuneCountInString(name) > 64 {
		return ErrInvalidDisplayName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return ErrInvalidDisplayName
		}
	}
	return nil
}

// Store reads and changes accounts.
type Store struct{ db *sql.DB }

// New returns a Store on a migrated database (internal/db).
func New(db *sql.DB) *Store { return &Store{db: db} }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("users: random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

const userColumns = "user_id, login_name, display_name, server_role, can_create_albums, status, created_at, substr(password_hash, 1, 1) = '" + password.LockedPrefix + "'"

type rowScanner interface{ Scan(...any) error }

func scanUser(r rowScanner) (User, error) {
	var u User
	var canCreate int
	err := r.Scan(&u.ID, &u.LoginName, &u.DisplayName, &u.Role, &canCreate, &u.Status, &u.CreatedAt, &u.PasswordLocked)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	u.CanCreateAlbums = canCreate == 1
	return u, err
}

// querier is *sql.DB or *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getUser(ctx context.Context, q querier, id string) (User, error) {
	return scanUser(q.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE user_id = ?", id))
}

// Get returns the account with user_id id.
func (s *Store) Get(ctx context.Context, id string) (User, error) { return getUser(ctx, s.db, id) }

// GetByLogin returns the account with this login name (case-insensitive).
func (s *Store) GetByLogin(ctx context.Context, login string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE login_name = ?", login))
}

// bootstrapState reads account_bootstrap: the user_id (or "" when there is
// no row) and whether it has been claimed.
func bootstrapState(ctx context.Context, q querier) (id string, claimed bool, err error) {
	var at sql.NullString
	err = q.QueryRowContext(ctx, "SELECT user_id, claimed_at FROM account_bootstrap WHERE singleton = 1").Scan(&id, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, at.Valid, err
}

// Initialized reports whether the bootstrap administrator has been claimed.
func (s *Store) Initialized(ctx context.Context) (bool, error) {
	_, claimed, err := bootstrapState(ctx, s.db)
	return claimed, err
}

// OwnedFolderCount returns how many folders the account owns.
func (s *Store) OwnedFolderCount(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM folders WHERE owner_user_id = ?", id).Scan(&n)
	return n, err
}

// loginTaken reports whether another account (not exceptID) uses login.
func loginTaken(ctx context.Context, tx *sql.Tx, login, exceptID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users WHERE login_name = ? AND user_id != ?", login, exceptID).Scan(&n)
	return n > 0, err
}

func record(ctx context.Context, tx *sql.Tx, actor Actor, action, targetID string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	details["via"] = actor.Via
	return audit.Record(ctx, tx, audit.Entry{ActorUserID: actor.UserID, Action: action, TargetType: "user",
		TargetID: targetID, Result: audit.Allowed, Details: details})
}

// Claim is the new identity of the bootstrap administrator.
type Claim struct {
	LoginName    string // may stay BootstrapLoginName ("admin")
	DisplayName  string
	PasswordHash string
}

// ClaimBootstrap takes over the locked bootstrap administrator: it sets the
// login name, display name and password, keeps the user_id (so the account
// keeps owning existing folders) and marks the bootstrap as claimed. It runs
// once; afterwards it returns ErrAlreadyInitialized.
func (s *Store) ClaimBootstrap(ctx context.Context, c Claim, actor Actor) (User, error) {
	if err := ValidateLoginName(c.LoginName); err != nil {
		return User{}, err
	}
	if err := ValidateDisplayName(c.DisplayName); err != nil {
		return User{}, err
	}
	if !password.Valid(c.PasswordHash) {
		return User{}, ErrInvalidHash
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	id, claimed, err := bootstrapState(ctx, tx)
	if err != nil {
		return User{}, err
	}
	if id == "" {
		return User{}, ErrNoBootstrap
	}
	if claimed {
		return User{}, ErrAlreadyInitialized
	}
	u, err := getUser(ctx, tx, id)
	if err != nil {
		return User{}, err
	}
	if !u.PasswordLocked || u.Role != RoleServerAdmin || u.Status != StatusActive {
		return User{}, ErrBootstrapState
	}
	if taken, err := loginTaken(ctx, tx, c.LoginName, id); err != nil {
		return User{}, err
	} else if taken {
		return User{}, ErrLoginTaken
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET login_name = ?, display_name = ?, password_hash = ? WHERE user_id = ?",
		c.LoginName, c.DisplayName, c.PasswordHash, id); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE account_bootstrap SET claimed_at = ? WHERE singleton = 1 AND claimed_at IS NULL", now()); err != nil {
		return User{}, err
	}
	var owned int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM folders WHERE owner_user_id = ?", id).Scan(&owned); err != nil {
		return User{}, err
	}
	if err := record(ctx, tx, actor, "account.bootstrap_claim", id, map[string]any{
		"previous_login": u.LoginName, "login": c.LoginName, "owned_folders": owned}); err != nil {
		return User{}, err
	}
	if u, err = getUser(ctx, tx, id); err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// NewUser describes an account to create.
type NewUser struct {
	LoginName       string
	DisplayName     string
	PasswordHash    string
	Role            Role
	CanCreateAlbums bool
}

// Create adds an active account. The administrator must have been set up
// first (ErrNotInitialized).
func (s *Store) Create(ctx context.Context, n NewUser, actor Actor) (User, error) {
	if err := ValidateLoginName(n.LoginName); err != nil {
		return User{}, err
	}
	if err := ValidateDisplayName(n.DisplayName); err != nil {
		return User{}, err
	}
	if n.Role != RoleServerAdmin && n.Role != RoleUser {
		return User{}, ErrInvalidRole
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
	canCreate := 0
	if n.CanCreateAlbums {
		canCreate = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users
		(user_id, login_name, display_name, password_hash, server_role, can_create_albums, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?)`,
		id, n.LoginName, n.DisplayName, n.PasswordHash, string(n.Role), canCreate, now()); err != nil {
		return User{}, err
	}
	if err := record(ctx, tx, actor, "account.create", id, map[string]any{
		"login": n.LoginName, "role": string(n.Role), "can_create_albums": n.CanCreateAlbums}); err != nil {
		return User{}, err
	}
	u, err := getUser(ctx, tx, id)
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// change runs fn on the account id inside one transaction. It refuses the
// unclaimed bootstrap account (ErrBootstrapUnclaimed).
func (s *Store) change(ctx context.Context, id string, fn func(tx *sql.Tx, u User) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u, err := getUser(ctx, tx, id)
	if err != nil {
		return err
	}
	bootID, claimed, err := bootstrapState(ctx, tx)
	if err != nil {
		return err
	}
	if id == bootID && !claimed {
		return ErrBootstrapUnclaimed
	}
	if err := fn(tx, u); err != nil {
		return err
	}
	return tx.Commit()
}

// usableAdmin reports whether u counts towards the last-administrator rule.
func usableAdmin(u User) bool {
	return u.Role == RoleServerAdmin && u.Status == StatusActive && !u.PasswordLocked
}

// UsableAdminCount counts the administrators who can log in: active
// SERVER_ADMIN accounts whose password is not locked (the unclaimed
// bootstrap account does not count).
func (s *Store) UsableAdminCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users
		WHERE server_role = 'SERVER_ADMIN' AND status = 'active' AND substr(password_hash, 1, 1) != ?`,
		password.LockedPrefix).Scan(&n)
	return n, err
}

// requireOtherAdmin returns ErrLastAdmin when no usable administrator other
// than id exists.
func requireOtherAdmin(ctx context.Context, tx *sql.Tx, id string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users
		WHERE server_role = 'SERVER_ADMIN' AND status = 'active' AND substr(password_hash, 1, 1) != ? AND user_id != ?`,
		password.LockedPrefix, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// SetPassword replaces the account's password hash (console recovery or an
// administrator) and revokes every session of the account. It does not
// change the account status.
func (s *Store) SetPassword(ctx context.Context, id, hash string, actor Actor) error {
	return s.setPassword(ctx, id, hash, "", "account.password_reset", session.ReasonPasswordReset, actor)
}

// ChangeOwnPassword replaces the password of the account logged in with
// session keepSessionID and revokes all its other sessions. The caller has
// verified the current password.
func (s *Store) ChangeOwnPassword(ctx context.Context, id, hash, keepSessionID string, actor Actor) error {
	if keepSessionID == "" {
		return errors.New("users: ChangeOwnPassword needs the current session")
	}
	return s.setPassword(ctx, id, hash, keepSessionID, "account.password_change", session.ReasonPasswordChange, actor)
}

func (s *Store) setPassword(ctx context.Context, id, hash, keepSessionID, action, reason string, actor Actor) error {
	if !password.Valid(hash) {
		return ErrInvalidHash
	}
	return s.change(ctx, id, func(tx *sql.Tx, u User) error {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE user_id = ?", hash, id); err != nil {
			return err
		}
		n, err := session.RevokeUserTx(ctx, tx, id, keepSessionID, reason)
		if err != nil {
			return err
		}
		return record(ctx, tx, actor, action, id, map[string]any{"login": u.LoginName, "sessions_revoked": n})
	})
}

// SetStatus enables or disables the account.
func (s *Store) SetStatus(ctx context.Context, id string, status Status, actor Actor) error {
	if status != StatusActive && status != StatusDisabled {
		return ErrInvalidStatus
	}
	return s.change(ctx, id, func(tx *sql.Tx, u User) error {
		if u.Status == status {
			return nil
		}
		if status == StatusDisabled && usableAdmin(u) {
			if err := requireOtherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET status = ? WHERE user_id = ?", string(status), id); err != nil {
			if strings.Contains(err.Error(), "transfer owned albums before disabling account") {
				return fmt.Errorf("%w (%w)", ErrOwnsAlbums, err)
			}
			return err
		}
		if status == StatusActive {
			return record(ctx, tx, actor, "account.enable", id, map[string]any{"login": u.LoginName})
		}
		n, err := session.RevokeUserTx(ctx, tx, id, "", session.ReasonAccountDisable)
		if err != nil {
			return err
		}
		return record(ctx, tx, actor, "account.disable", id, map[string]any{"login": u.LoginName, "sessions_revoked": n})
	})
}

// SetRole changes the server role.
func (s *Store) SetRole(ctx context.Context, id string, role Role, actor Actor) error {
	return s.SetPermissions(ctx, id, Permissions{Role: &role}, actor)
}

// SetCanCreateAlbums grants or revokes album creation for a USER account
// (SERVER_ADMIN may always create albums, see internal/authz).
func (s *Store) SetCanCreateAlbums(ctx context.Context, id string, allowed bool, actor Actor) error {
	return s.SetPermissions(ctx, id, Permissions{CanCreateAlbums: &allowed}, actor)
}

// List returns every account, ordered by login name.
func (s *Store) List(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+userColumns+" FROM users ORDER BY login_name COLLATE NOCASE, user_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

// Permissions is a change of server permissions; nil fields stay as they are.
type Permissions struct {
	Role            *Role
	CanCreateAlbums *bool
}

// SetPermissions applies p in one transaction (both changes or neither).
func (s *Store) SetPermissions(ctx context.Context, id string, p Permissions, actor Actor) error {
	if p.Role != nil && *p.Role != RoleServerAdmin && *p.Role != RoleUser {
		return ErrInvalidRole
	}
	return s.change(ctx, id, func(tx *sql.Tx, u User) error {
		if p.Role != nil && *p.Role != u.Role {
			if *p.Role != RoleServerAdmin && usableAdmin(u) {
				if err := requireOtherAdmin(ctx, tx, id); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, "UPDATE users SET server_role = ? WHERE user_id = ?", string(*p.Role), id); err != nil {
				return err
			}
			if err := record(ctx, tx, actor, "account.role_change", id, map[string]any{
				"login": u.LoginName, "from": string(u.Role), "to": string(*p.Role)}); err != nil {
				return err
			}
		}
		if p.CanCreateAlbums != nil && *p.CanCreateAlbums != u.CanCreateAlbums {
			v := 0
			if *p.CanCreateAlbums {
				v = 1
			}
			if _, err := tx.ExecContext(ctx, "UPDATE users SET can_create_albums = ? WHERE user_id = ?", v, id); err != nil {
				return err
			}
			if err := record(ctx, tx, actor, "account.album_creation_change", id, map[string]any{
				"login": u.LoginName, "can_create_albums": *p.CanCreateAlbums}); err != nil {
				return err
			}
		}
		return nil
	})
}
