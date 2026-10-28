package users

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
)

// testParams keeps tests fast. Production code always uses password.DefaultParams.
var testParams = password.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func hash(t *testing.T, pw string) string {
	t.Helper()
	h, err := password.Hash(pw, testParams)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// setup returns a migrated database with the locked bootstrap administrator
// (as the server creates it) owning two folders, and its user_id.
func setup(t *testing.T) (*sql.DB, *Store, string) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "withalbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	boot := ensureBootstrap(t, sqlDB)
	for _, f := range []string{"f1", "f2"} {
		exec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES (?, ?, ?)", f, "Legacy "+f, boot)
	}
	return sqlDB, New(sqlDB), boot
}

func ensureBootstrap(t *testing.T, sqlDB *sql.DB) string {
	t.Helper()
	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := sqlitestore.EnsureBootstrapOwnerTx(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func exec(t *testing.T, sqlDB *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := sqlDB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func claim(t *testing.T, s *Store, login string) User {
	t.Helper()
	u, err := s.ClaimBootstrap(context.Background(), Claim{LoginName: login, DisplayName: "Owner", PasswordHash: hash(t, "first admin password")}, Console)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func create(t *testing.T, s *Store, login string, role Role) User {
	t.Helper()
	u, err := s.Create(context.Background(), NewUser{LoginName: login, DisplayName: login, PasswordHash: hash(t, "a user password"), Role: role}, Console)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func storedHash(t *testing.T, sqlDB *sql.DB, id string) string {
	t.Helper()
	var h string
	if err := sqlDB.QueryRow("SELECT password_hash FROM users WHERE user_id = ?", id).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestBootstrapBeforeClaimIsLockedAndFrozen(t *testing.T) {
	ctx := context.Background()
	_, s, boot := setup(t)
	u, err := s.Get(ctx, boot)
	if err != nil || !u.PasswordLocked || u.LoginName != sqlitestore.BootstrapLoginName || u.Role != RoleServerAdmin {
		t.Fatalf("bootstrap = %+v, %v", u, err)
	}
	if ok, err := s.Initialized(ctx); ok || err != nil {
		t.Fatalf("Initialized = %v, %v", ok, err)
	}
	if _, err := s.Create(ctx, NewUser{LoginName: "bob", DisplayName: "Bob", PasswordHash: hash(t, "a user password"), Role: RoleUser}, Console); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Create before init = %v", err)
	}
	for name, err := range map[string]error{
		"SetPassword":        s.SetPassword(ctx, boot, hash(t, "some new password"), Console),
		"SetStatus":          s.SetStatus(ctx, boot, StatusDisabled, Console),
		"SetRole":            s.SetRole(ctx, boot, RoleUser, Console),
		"SetCanCreateAlbums": s.SetCanCreateAlbums(ctx, boot, false, Console),
	} {
		if !errors.Is(err, ErrBootstrapUnclaimed) {
			t.Errorf("%s on unclaimed bootstrap = %v", name, err)
		}
	}
	if u, _ := s.Get(ctx, boot); !u.PasswordLocked || u.Status != StatusActive {
		t.Fatalf("bootstrap changed: %+v", u)
	}
}

func TestClaimBootstrapKeepsLegacyFolders(t *testing.T) {
	ctx := context.Background()
	sqlDB, s, boot := setup(t)
	u := claim(t, s, "alice")
	if u.ID != boot || u.LoginName != "alice" || u.DisplayName != "Owner" || u.PasswordLocked || u.Role != RoleServerAdmin || u.Status != StatusActive {
		t.Fatalf("claimed = %+v", u)
	}
	if n, err := s.OwnedFolderCount(ctx, boot); n != 2 || err != nil {
		t.Fatalf("owned folders after claim = %d, %v", n, err)
	}
	if ok, err := password.Verify("first admin password", storedHash(t, sqlDB, boot)); !ok || err != nil {
		t.Fatalf("stored hash does not verify: %v %v", ok, err)
	}
	if ok, _ := s.Initialized(ctx); !ok {
		t.Fatal("not initialized after claim")
	}
	// The server keeps using the same account as folder owner: no second
	// locked 'admin' is created on the next start.
	if id := ensureBootstrap(t, sqlDB); id != boot {
		t.Fatalf("bootstrap owner after claim = %s, want %s", id, boot)
	}
	var users int
	sqlDB.QueryRow("SELECT count(*) FROM users").Scan(&users)
	if users != 1 {
		t.Fatalf("%d users after reopen, want 1", users)
	}
	// Second init is refused and changes nothing.
	before := storedHash(t, sqlDB, boot)
	if _, err := s.ClaimBootstrap(ctx, Claim{LoginName: "mallory", DisplayName: "M", PasswordHash: hash(t, "attacker password")}, Console); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second claim = %v", err)
	}
	if after, _ := s.Get(ctx, boot); after.LoginName != "alice" || storedHash(t, sqlDB, boot) != before {
		t.Fatal("second claim changed the account")
	}
	// The original name is free again once the account was renamed.
	if u := create(t, s, "Admin", RoleUser); u.ID == boot {
		t.Fatal("new account reused the bootstrap id")
	}
	// Audit row, without secret material.
	var actor sql.NullString
	var details string
	if err := sqlDB.QueryRow("SELECT actor_user_id, details_json FROM audit_log WHERE action = 'account.bootstrap_claim' AND target_id = ?", boot).Scan(&actor, &details); err != nil {
		t.Fatal(err)
	}
	if actor.Valid || !strings.Contains(details, `"previous_login":"admin"`) || !strings.Contains(details, `"owned_folders":2`) || !strings.Contains(details, `"via":"console"`) {
		t.Fatalf("audit row actor=%v details=%s", actor, details)
	}
	assertNoSecretsInAudit(t, sqlDB)
}

func assertNoSecretsInAudit(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	var n int
	if err := sqlDB.QueryRow("SELECT count(*) FROM audit_log WHERE details_json LIKE '%argon2%' OR details_json LIKE '%password%'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("audit_log holds secret material: %d rows, %v", n, err)
	}
}

func TestClaimMayKeepAdminName(t *testing.T) {
	_, s, boot := setup(t)
	if u := claim(t, s, "ADMIN"); u.ID != boot || u.LoginName != "ADMIN" {
		t.Fatalf("claimed = %+v", u)
	}
	if _, err := s.Create(context.Background(), NewUser{LoginName: "admin", DisplayName: "x", PasswordHash: hash(t, "a user password"), Role: RoleUser}, Console); !errors.Is(err, ErrLoginTaken) {
		t.Fatalf("duplicate admin = %v", err)
	}
}

func TestClaimRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	sqlDB, s, boot := setup(t)
	good := hash(t, "first admin password")
	for _, c := range []struct {
		claim Claim
		want  error
	}{
		{Claim{LoginName: "a", DisplayName: "A", PasswordHash: good}, ErrInvalidLoginName},
		{Claim{LoginName: "has space", DisplayName: "A", PasswordHash: good}, ErrInvalidLoginName},
		{Claim{LoginName: "-dash", DisplayName: "A", PasswordHash: good}, ErrInvalidLoginName},
		{Claim{LoginName: strings.Repeat("a", 33), DisplayName: "A", PasswordHash: good}, ErrInvalidLoginName},
		{Claim{LoginName: "alice", DisplayName: "", PasswordHash: good}, ErrInvalidDisplayName},
		{Claim{LoginName: "alice", DisplayName: " A", PasswordHash: good}, ErrInvalidDisplayName},
		{Claim{LoginName: "alice", DisplayName: "A\tB", PasswordHash: good}, ErrInvalidDisplayName},
		{Claim{LoginName: "alice", DisplayName: "A", PasswordHash: "!locked"}, ErrInvalidHash},
		{Claim{LoginName: "alice", DisplayName: "A", PasswordHash: "plaintext password"}, ErrInvalidHash},
		{Claim{LoginName: "alice", DisplayName: "A", PasswordHash: good + "$"}, ErrInvalidHash},
	} {
		if _, err := s.ClaimBootstrap(ctx, c.claim, Console); !errors.Is(err, c.want) {
			t.Errorf("ClaimBootstrap(%+v) = %v, want %v", c.claim.LoginName, err, c.want)
		}
	}
	if ok, _ := s.Initialized(ctx); ok {
		t.Fatal("rejected claims initialized the server")
	}
	// A bootstrap account that is not locked (unexpected state) is not taken over.
	exec(t, sqlDB, "UPDATE users SET password_hash = ? WHERE user_id = ?", good, boot)
	if _, err := s.ClaimBootstrap(ctx, Claim{LoginName: "alice", DisplayName: "A", PasswordHash: good}, Console); !errors.Is(err, ErrBootstrapState) {
		t.Fatalf("claim of unlocked bootstrap = %v", err)
	}
	// No bootstrap row at all.
	exec(t, sqlDB, "DELETE FROM account_bootstrap")
	if _, err := s.ClaimBootstrap(ctx, Claim{LoginName: "alice", DisplayName: "A", PasswordHash: good}, Console); !errors.Is(err, ErrNoBootstrap) {
		t.Fatalf("claim without bootstrap = %v", err)
	}
}

func TestConcurrentClaimsOnlyOneWins(t *testing.T) {
	_, s, _ := setup(t)
	h := hash(t, "first admin password")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.ClaimBootstrap(context.Background(), Claim{LoginName: "admin" + string(rune('a'+i)), DisplayName: "A", PasswordHash: h}, Console)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrAlreadyInitialized):
			t.Fatalf("unexpected error %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d claims succeeded, want 1", wins)
	}
}

func TestCreateUser(t *testing.T) {
	ctx := context.Background()
	sqlDB, s, _ := setup(t)
	claim(t, s, "alice")
	bob := create(t, s, "bob", RoleUser)
	if bob.Role != RoleUser || bob.CanCreateAlbums || bob.Status != StatusActive || bob.PasswordLocked {
		t.Fatalf("bob = %+v", bob)
	}
	if got, err := s.GetByLogin(ctx, "BOB"); err != nil || got.ID != bob.ID {
		t.Fatalf("GetByLogin case-insensitive = %+v, %v", got, err)
	}
	if _, err := s.GetByLogin(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByLogin missing = %v", err)
	}
	h := hash(t, "a user password")
	for _, c := range []struct {
		n    NewUser
		want error
	}{
		{NewUser{LoginName: "Bob", DisplayName: "B", PasswordHash: h, Role: RoleUser}, ErrLoginTaken},
		{NewUser{LoginName: "carol", DisplayName: "C", PasswordHash: h, Role: "OWNER"}, ErrInvalidRole},
		{NewUser{LoginName: "carol", DisplayName: "C", PasswordHash: "!locked", Role: RoleUser}, ErrInvalidHash},
		{NewUser{LoginName: "c", DisplayName: "C", PasswordHash: h, Role: RoleUser}, ErrInvalidLoginName},
	} {
		if _, err := s.Create(ctx, c.n, Console); !errors.Is(err, c.want) {
			t.Errorf("Create(%s) = %v, want %v", c.n.LoginName, err, c.want)
		}
	}
	creator, err := s.Create(ctx, NewUser{LoginName: "dora", DisplayName: "도라", PasswordHash: h, Role: RoleUser, CanCreateAlbums: true}, Console)
	if err != nil || !creator.CanCreateAlbums {
		t.Fatalf("Create with album creation = %+v, %v", creator, err)
	}
	var n int
	sqlDB.QueryRow("SELECT count(*) FROM audit_log WHERE action = 'account.create'").Scan(&n)
	if n != 2 {
		t.Fatalf("%d account.create audit rows, want 2", n)
	}
	assertNoSecretsInAudit(t, sqlDB)
}

func TestSetPassword(t *testing.T) {
	ctx := context.Background()
	sqlDB, s, _ := setup(t)
	claim(t, s, "alice")
	bob := create(t, s, "bob", RoleUser)
	if err := s.SetPassword(ctx, bob.ID, "!locked", Console); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("SetPassword(locked) = %v", err)
	}
	if err := s.SetPassword(ctx, "missing", hash(t, "another password"), Console); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetPassword(missing) = %v", err)
	}
	if err := s.SetPassword(ctx, bob.ID, hash(t, "another password"), Console); err != nil {
		t.Fatal(err)
	}
	h := storedHash(t, sqlDB, bob.ID)
	if ok, _ := password.Verify("another password", h); !ok {
		t.Fatal("new password does not verify")
	}
	if ok, _ := password.Verify("a user password", h); ok {
		t.Fatal("old password still verifies")
	}
	assertNoSecretsInAudit(t, sqlDB)
}

func TestLastActiveAdminCannotBeRemoved(t *testing.T) {
	ctx := context.Background()
	_, s, boot := setup(t)
	claim(t, s, "alice") // owns the legacy folders
	if err := s.SetRole(ctx, boot, RoleUser, Console); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last admin = %v", err)
	}
	if err := s.SetStatus(ctx, boot, StatusDisabled, Console); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable last admin = %v", err)
	}
	bob := create(t, s, "bob", RoleServerAdmin)
	carol := create(t, s, "carol", RoleServerAdmin)
	if err := s.SetStatus(ctx, carol.ID, StatusDisabled, Console); err != nil {
		t.Fatal(err)
	}
	// A disabled administrator does not count.
	if err := s.SetRole(ctx, boot, RoleUser, Console); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, bob.ID, RoleUser, Console); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demote last remaining admin = %v", err)
	}
	if err := s.SetStatus(ctx, bob.ID, StatusDisabled, Console); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disable last remaining admin = %v", err)
	}
	// Re-enabling carol makes bob removable.
	if err := s.SetStatus(ctx, carol.ID, StatusActive, Console); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, bob.ID, RoleUser, Console); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get(ctx, bob.ID); u.Role != RoleUser {
		t.Fatalf("bob = %+v", u)
	}
	// Non-admins are not affected by the rule; no-op changes succeed.
	if err := s.SetRole(ctx, bob.ID, RoleUser, Console); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, bob.ID, "ROOT", Console); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role = %v", err)
	}
	if err := s.SetStatus(ctx, bob.ID, "gone", Console); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("invalid status = %v", err)
	}
}

func TestOwnerCannotBeDisabled(t *testing.T) {
	ctx := context.Background()
	sqlDB, s, _ := setup(t)
	claim(t, s, "alice")
	create(t, s, "root2", RoleServerAdmin) // keeps the last-admin rule out of the way
	dave := create(t, s, "dave", RoleUser)
	exec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('fd', 'Dave', ?)", dave.ID)
	// Enforced by the schema trigger, through the store and through raw SQL.
	if err := s.SetStatus(ctx, dave.ID, StatusDisabled, Console); !errors.Is(err, ErrOwnsAlbums) {
		t.Fatalf("disable owner = %v", err)
	}
	if _, err := sqlDB.Exec("UPDATE users SET status = 'disabled' WHERE user_id = ?", dave.ID); err == nil || !strings.Contains(err.Error(), "transfer owned albums") {
		t.Fatalf("raw disable of owner = %v", err)
	}
	if u, _ := s.Get(ctx, dave.ID); u.Status != StatusActive {
		t.Fatal("owner was disabled")
	}
	var audits int
	sqlDB.QueryRow("SELECT count(*) FROM audit_log WHERE action = 'account.disable'").Scan(&audits)
	if audits != 0 {
		t.Fatal("refused change was audited")
	}
	exec(t, sqlDB, "DELETE FROM folders WHERE folder_id = 'fd'")
	if err := s.SetStatus(ctx, dave.ID, StatusDisabled, Console); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(ctx, dave.ID, StatusActive, Console); err != nil {
		t.Fatal(err)
	}
	// The administrator owning the legacy folders cannot be disabled either.
	alice, _ := s.GetByLogin(ctx, "alice")
	if err := s.SetStatus(ctx, alice.ID, StatusDisabled, Console); !errors.Is(err, ErrOwnsAlbums) {
		t.Fatalf("disable legacy owner = %v", err)
	}
}

func TestSetCanCreateAlbums(t *testing.T) {
	ctx := context.Background()
	sqlDB, s, _ := setup(t)
	claim(t, s, "alice")
	bob := create(t, s, "bob", RoleUser)
	if err := s.SetCanCreateAlbums(ctx, bob.ID, true, Actor{UserID: bob.ID, Via: "test"}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get(ctx, bob.ID); !u.CanCreateAlbums {
		t.Fatal("flag not set")
	}
	var actor string
	if err := sqlDB.QueryRow("SELECT actor_user_id FROM audit_log WHERE action = 'account.album_creation_change'").Scan(&actor); err != nil || actor != bob.ID {
		t.Fatalf("audit actor = %q, %v", actor, err)
	}
}

func TestSetPermissionsIsAtomic(t *testing.T) {
	ctx := context.Background()
	_, s, boot := setup(t)
	claim(t, s, "alice")
	bob := create(t, s, "bob", RoleUser)
	yes := true
	user, bad := RoleUser, Role("ROOT")
	// The role change fails (last administrator), so the album permission
	// granted in the same call is not applied either.
	if err := s.SetPermissions(ctx, boot, Permissions{Role: &user, CanCreateAlbums: &yes}, Console); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("SetPermissions = %v", err)
	}
	if err := s.SetPermissions(ctx, bob.ID, Permissions{Role: &bad, CanCreateAlbums: &yes}, Console); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("SetPermissions(bad role) = %v", err)
	}
	if u, _ := s.Get(ctx, bob.ID); u.CanCreateAlbums {
		t.Fatal("partial permission change applied")
	}
	admin := RoleServerAdmin
	if err := s.SetPermissions(ctx, bob.ID, Permissions{Role: &admin, CanCreateAlbums: &yes}, Console); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get(ctx, bob.ID); u.Role != RoleServerAdmin || !u.CanCreateAlbums {
		t.Fatalf("bob = %+v", u)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 || list[0].LoginName != "alice" || list[1].LoginName != "bob" {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

// TestUsableAdminCount: only active administrators with a usable password
// count (the LAN listen gate in server/main.go relies on it).
func TestUsableAdminCount(t *testing.T) {
	ctx := context.Background()
	sqlDB, s, _ := setup(t)
	count := func(want int) {
		t.Helper()
		if n, err := s.UsableAdminCount(ctx); err != nil || n != want {
			t.Fatalf("UsableAdminCount = %d, %v, want %d", n, err, want)
		}
	}
	count(0) // only the locked bootstrap account
	claim(t, s, "alice")
	count(1)
	create(t, s, "bob", RoleUser)
	count(1)
	carol := create(t, s, "carol", RoleServerAdmin)
	count(2)
	if err := s.SetStatus(ctx, carol.ID, StatusDisabled, Console); err != nil {
		t.Fatal(err)
	}
	count(1)
	exec(t, sqlDB, "UPDATE users SET password_hash = '!locked' WHERE login_name = 'alice'")
	count(0)
}
