package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
	"golang.org/x/term"
)

// Console account administration: "withalbum-server admin <command>".
//
// These commands are the only way to set up the first administrator (there
// is no HTTP route for it), so whoever controls the server console decides
// who administers it. Passwords are never taken from the command line: they
// are typed twice on the terminal with echo off, or, when stdin is not a
// terminal, read as the single line on stdin. Nothing printed contains a
// password or a hash.
//
// Every command takes the storage instance lock (.system/withalbum.lock), so
// it refuses to run while a server, a migration or another admin command
// uses the storage: stop the server first. One process changes a storage at
// a time.

// adminIO is the environment of an admin command. Tests replace it.
type adminIO struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	// readHidden reads one line from the terminal without echo. nil when
	// stdin is not a terminal: the password is then read from stdin.
	readHidden func() ([]byte, error)
	params     password.Params
}

func consoleIO() adminIO {
	aio := adminIO{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, params: password.DefaultParams}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		aio.readHidden = func() ([]byte, error) { return term.ReadPassword(fd) }
	}
	return aio
}

const adminUsage = `usage: withalbum-server admin <command> [flags]

Commands (run at the server console, with the server stopped):
  init            set up the first administrator; takes over the locked bootstrap
                  account "admin" that owns the existing folders (once only)
  create-user     create an account (USER by default)
  reset-password  set a new password for an account (console recovery)

The password is typed twice without echo, or read as one line from stdin when
stdin is not a terminal (for example: withalbum-server admin init -login alice < file).
It is never accepted as a command-line argument.
Run "withalbum-server admin <command> -h" for the flags of a command.
`

// runAdmin implements "withalbum-server admin ..." and returns the exit code:
// 0 success, 1 failure, 2 usage error.
func runAdmin(args []string, aio adminIO) int {
	if len(args) == 0 {
		fmt.Fprint(aio.stderr, adminUsage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("admin "+cmd, flag.ContinueOnError)
	fs.SetOutput(aio.stderr)
	login := fs.String("login", "", "login name: 3-32 letters, digits, '.', '_' or '-' (required)")
	var displayName, role *string
	var canCreate *bool
	switch cmd {
	case "init":
		displayName = fs.String("display-name", "", "display name (default: the login name)")
	case "create-user":
		displayName = fs.String("display-name", "", "display name (default: the login name)")
		role = fs.String("role", string(users.RoleUser), "server role: USER or SERVER_ADMIN")
		canCreate = fs.Bool("can-create-albums", false, "allow a USER to create albums")
	case "reset-password":
	case "-h", "-help", "--help", "help":
		fmt.Fprint(aio.stderr, adminUsage)
		return 0
	default:
		fmt.Fprintf(aio.stderr, "admin: unknown command %q\n\n%s", cmd, adminUsage)
		return 2
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(aio.stderr, "admin %s: unexpected argument %q (passwords are never accepted as arguments)\n", cmd, fs.Arg(0))
		return 2
	}
	if *login == "" {
		fmt.Fprintf(aio.stderr, "admin %s: -login is required\n", cmd)
		return 2
	}
	if displayName != nil && *displayName == "" {
		*displayName = *login
	}
	fail := func(err error) int {
		fmt.Fprintf(aio.stderr, "admin %s: %v\n", cmd, err)
		return 1
	}
	ctx := context.Background()
	store, closeStore, err := openAccounts(ctx)
	if err != nil {
		return fail(err)
	}
	defer closeStore()
	switch cmd {
	case "init":
		return adminInit(ctx, store, aio, *login, *displayName, fail)
	case "create-user":
		return adminCreateUser(ctx, store, aio, users.NewUser{LoginName: *login, DisplayName: *displayName,
			Role: users.Role(*role), CanCreateAlbums: *canCreate}, fail)
	default:
		return adminResetPassword(ctx, store, aio, *login, fail)
	}
}

// openAccounts opens the storage like the server does (volume check,
// instance lock, metadata with the legacy-state refusal) and returns the
// account store and a function that releases everything.
func openAccounts(ctx context.Context) (*users.Store, func(), error) {
	root, err := storageRoot()
	if err != nil {
		return nil, nil, err
	}
	vol, err := storage.Open(root, storage.OpenOptions{ExpectedVolumeID: os.Getenv("WITHALBUM_VOLUME_ID")})
	if err != nil {
		return nil, nil, fmt.Errorf("STORAGE: %s (%w)", openStorageHint(root, err), err)
	}
	lock, err := vol.Lock()
	if err != nil {
		return nil, nil, fmt.Errorf("%s (%w)", lockHint(root, err), err)
	}
	m, err := openMetadata(ctx, vol)
	if err != nil {
		lock.Release()
		if errors.Is(err, errUnmigratedState) {
			return nil, nil, fmt.Errorf("METADATA: %q holds metadata from an earlier version (.system/state.json) that has not been migrated. Run withalbum-server migrate-legacy -apply first. (%w)", root, err)
		}
		return nil, nil, fmt.Errorf("METADATA: %w", err)
	}
	dbm, ok := m.(interface{ DB() *sql.DB })
	if !ok {
		m.Close()
		lock.Release()
		return nil, nil, fmt.Errorf("metadata store %T has no account database", m)
	}
	return users.New(dbm.DB()), func() { closeAll(m, lock) }, nil
}

func closeAll(m meta.MetadataStore, lock *storage.InstanceLock) {
	m.Close()
	lock.Release()
}

// readNewPassword reads a new password and checks it against the policy.
func readNewPassword(aio adminIO) (string, error) {
	if aio.readHidden == nil {
		// One line on stdin; read at most a few bytes past the limit.
		b, err := io.ReadAll(io.LimitReader(aio.stdin, password.MaxBytes+3))
		if err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		b = bytes.TrimSuffix(b, []byte("\n"))
		b = bytes.TrimSuffix(b, []byte("\r"))
		if len(b) == 0 {
			return "", errors.New("no password on stdin (type it on a terminal, or pipe exactly one line)")
		}
		pw := string(b)
		return pw, password.CheckPolicy(pw)
	}
	fmt.Fprintf(aio.stderr, "New password (at least %d characters): ", password.MinRunes)
	first, err := aio.readHidden()
	fmt.Fprintln(aio.stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if err := password.CheckPolicy(string(first)); err != nil {
		return "", err
	}
	fmt.Fprint(aio.stderr, "Repeat the password: ")
	second, err := aio.readHidden()
	fmt.Fprintln(aio.stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if subtle.ConstantTimeCompare(first, second) != 1 {
		return "", errors.New("the passwords do not match; nothing was changed")
	}
	return string(first), nil
}

// newHash reads a new password and hashes it.
func newHash(aio adminIO) (string, error) {
	pw, err := readNewPassword(aio)
	if err != nil {
		return "", err
	}
	return password.Hash(pw, aio.params)
}

func adminInit(ctx context.Context, store *users.Store, aio adminIO, login, displayName string, fail func(error) int) int {
	// Refuse before asking for a password.
	if done, err := store.Initialized(ctx); err != nil {
		return fail(err)
	} else if done {
		return fail(fmt.Errorf("%w. Use \"withalbum-server admin reset-password\" to recover an administrator's password", users.ErrAlreadyInitialized))
	}
	if err := users.ValidateLoginName(login); err != nil {
		return fail(err)
	}
	if err := users.ValidateDisplayName(displayName); err != nil {
		return fail(err)
	}
	h, err := newHash(aio)
	if err != nil {
		return fail(err)
	}
	u, err := store.ClaimBootstrap(ctx, users.Claim{LoginName: login, DisplayName: displayName, PasswordHash: h}, users.Console)
	if err != nil {
		return fail(err)
	}
	owned, err := store.OwnedFolderCount(ctx, u.ID)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(aio.stdout, "Administrator set up: login=%s user_id=%s\n", u.LoginName, u.ID)
	fmt.Fprintf(aio.stdout, "It took over the bootstrap account and keeps owning the %d existing folder(s).\n", owned)
	return 0
}

func adminCreateUser(ctx context.Context, store *users.Store, aio adminIO, n users.NewUser, fail func(error) int) int {
	if done, err := store.Initialized(ctx); err != nil {
		return fail(err)
	} else if !done {
		return fail(users.ErrNotInitialized)
	}
	if err := users.ValidateLoginName(n.LoginName); err != nil {
		return fail(err)
	}
	if err := users.ValidateDisplayName(n.DisplayName); err != nil {
		return fail(err)
	}
	if n.Role != users.RoleUser && n.Role != users.RoleServerAdmin {
		return fail(fmt.Errorf("%w %q (USER or SERVER_ADMIN)", users.ErrInvalidRole, n.Role))
	}
	if _, err := store.GetByLogin(ctx, n.LoginName); err == nil {
		return fail(users.ErrLoginTaken)
	} else if !errors.Is(err, users.ErrNotFound) {
		return fail(err)
	}
	var err error
	if n.PasswordHash, err = newHash(aio); err != nil {
		return fail(err)
	}
	u, err := store.Create(ctx, n, users.Console)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(aio.stdout, "Account created: login=%s user_id=%s role=%s can_create_albums=%t\n", u.LoginName, u.ID, u.Role, u.CanCreateAlbums)
	return 0
}

func adminResetPassword(ctx context.Context, store *users.Store, aio adminIO, login string, fail func(error) int) int {
	u, err := store.GetByLogin(ctx, login)
	if errors.Is(err, users.ErrNotFound) {
		return fail(fmt.Errorf("no account with login %q", login))
	} else if err != nil {
		return fail(err)
	}
	if u.PasswordLocked {
		return fail(users.ErrBootstrapUnclaimed)
	}
	h, err := newHash(aio)
	if err != nil {
		return fail(err)
	}
	if err := store.SetPassword(ctx, u.ID, h, users.Console); err != nil {
		return fail(err)
	}
	status := ""
	if u.Status != users.StatusActive {
		status = " (the account stays disabled)"
	}
	fmt.Fprintf(aio.stdout, "Password reset: login=%s user_id=%s%s\n", u.LoginName, u.ID, status)
	return 0
}
