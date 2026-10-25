package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func openTemp(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "withalbum.db")
	sqlDB, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return sqlDB, path
}

func openMigrated(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, _ := openTemp(t)
	if err := Migrate(context.Background(), sqlDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return sqlDB
}

func TestEmbeddedMigrationsAreContiguous(t *testing.T) {
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 15 || LatestVersion() != 15 {
		t.Fatalf("got %d migrations, LatestVersion %d; want 15", len(ms), LatestVersion())
	}
}

func TestLoadMigrationsRejectsGapsAndDuplicates(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"gap":       {"m/0001_a.sql": {}, "m/0003_c.sql": {}},
		"duplicate": {"m/0001_a.sql": {}, "m/0001_b.sql": {}},
		"bad name":  {"m/first.sql": {}},
		"zero":      {"m/0000_a.sql": {}},
	} {
		if _, err := loadMigrations(files, "m"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestOpenAppliesPragmasOnEveryConnection(t *testing.T) {
	sqlDB, _ := openTemp(t)
	ctx := context.Background()
	// Hold several connections at once so the pool must open distinct ones.
	var conns []*sql.Conn
	for i := 0; i < maxOpenConns; i++ {
		c, err := sqlDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	if got := sqlDB.Stats().OpenConnections; got != maxOpenConns {
		t.Fatalf("OpenConnections = %d, want %d", got, maxOpenConns)
	}
	for i, c := range conns {
		var fk int
		var mode string
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || mode != "wal" {
			t.Errorf("conn %d: foreign_keys=%d journal_mode=%q", i, fk, mode)
		}
		if err := CheckConn(ctx, c); err != nil {
			t.Errorf("conn %d: %v", i, err)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestSQLiteVersionAtLeast338(t *testing.T) {
	sqlDB, _ := openTemp(t)
	// The schema requires SQLite 3.38+ (documented in schema_v1.4.sql).
	var v string
	if err := sqlDB.QueryRow("SELECT sqlite_version()").Scan(&v); err != nil {
		t.Fatal(err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(v, "%d.%d", &major, &minor); err != nil {
		t.Fatal(err)
	}
	if major < 3 || (major == 3 && minor < 38) {
		t.Fatalf("sqlite %s < 3.38", v)
	}
}

func TestOpenRejectsMissingDirectoryAndQuestionMark(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "unmounted", "withalbum.db")
	if db, err := Open(missing); err == nil {
		db.Close()
		t.Fatal("Open succeeded in a missing directory")
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open created the parent directory: %v", err)
	}
	if db, err := Open(filepath.Join(t.TempDir(), "a?b.db")); err == nil {
		db.Close()
		t.Fatal("Open accepted a path with '?'")
	}
}

func TestMigrateNewDatabase(t *testing.T) {
	sqlDB := openMigrated(t)
	ctx := context.Background()
	v, err := SchemaVersion(ctx, sqlDB)
	if err != nil || v != LatestVersion() {
		t.Fatalf("user_version = %d, %v; want %d", v, err, LatestVersion())
	}
	for _, table := range []string{
		"users", "devices", "auth_sessions", "folders", "folder_members",
		"folder_invitations", "blobs", "assets", "folder_assets",
		"folder_sequences", "folder_changes", "device_sync_cursors",
		"upload_receipts", "audit_log", "legacy_imports", "legacy_asset_map", "sync_state",
	} {
		var n int
		if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: count=%d err=%v", table, n, err)
		}
	}
	for _, tc := range [][2]string{{"folder_assets", "uploader_device_label"}, {"folder_changes", "uploader_device_label"}, {"assets", "preview_ready"},
		{"folder_sequences", "pruned_seq"}, {"upload_receipts", "idempotency_key"}, {"auth_refresh_used", "grace"}} {
		var n int
		if err := sqlDB.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", tc[0], tc[1]).Scan(&n); err != nil || n != 1 {
			t.Errorf("column %s.%s missing: %v", tc[0], tc[1], err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	sqlDB := openMigrated(t)
	ctx := context.Background()
	var before string
	if err := sqlDB.QueryRow("SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_schema ORDER BY name)").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, sqlDB); err != nil {
			t.Fatalf("re-run %d: %v", i, err)
		}
	}
	var after string
	if err := sqlDB.QueryRow("SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_schema ORDER BY name)").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("schema changed on re-run")
	}
	if v, _ := SchemaVersion(ctx, sqlDB); v != LatestVersion() {
		t.Fatalf("user_version = %d", v)
	}
}

func TestMigrateAfterReopen(t *testing.T) {
	sqlDB, path := openTemp(t)
	ctx := context.Background()
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if err := Migrate(ctx, again); err != nil {
		t.Fatal(err)
	}
	if v, _ := SchemaVersion(ctx, again); v != LatestVersion() {
		t.Fatalf("user_version = %d", v)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	sqlDB := openMigrated(t)
	ctx := context.Background()
	if _, err := sqlDB.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, sqlDB); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Migrate = %v, want ErrSchemaTooNew", err)
	}
	if v, _ := SchemaVersion(ctx, sqlDB); v != 99 {
		t.Fatalf("user_version changed to %d", v)
	}
}

func TestMigrateRefusesForeignDatabase(t *testing.T) {
	sqlDB, _ := openTemp(t)
	if _, err := sqlDB.Exec("CREATE TABLE something_else (x)"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), sqlDB); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("Migrate = %v, want ErrForeignDatabase", err)
	}
	if v, _ := SchemaVersion(context.Background(), sqlDB); v != 0 {
		t.Fatalf("user_version = %d", v)
	}
}

func TestMigrateRunsIntegrityCheckFirst(t *testing.T) {
	sqlDB, _ := openTemp(t)
	ctx := context.Background()
	// integrity_check also verifies CHECK constraints; plant a violating row.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE c (x INTEGER CHECK(x > 0))",
		"PRAGMA ignore_check_constraints = ON",
		"INSERT INTO c VALUES (-1)",
		"PRAGMA ignore_check_constraints = OFF",
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	conn.Close()
	if err := Migrate(ctx, sqlDB); !errors.Is(err, ErrIntegrityCheck) {
		t.Fatalf("Migrate = %v, want ErrIntegrityCheck", err)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	sqlDB, _ := openTemp(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"m/0001_ok.sql":     {Data: []byte("CREATE TABLE a (x);")},
		"m/0002_broken.sql": {Data: []byte("CREATE TABLE b (x); CREATE TABLE a (y);")},
	}
	if err := migrate(ctx, sqlDB, fsys, "m"); err == nil {
		t.Fatal("expected failure")
	}
	if v, _ := SchemaVersion(ctx, sqlDB); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
	var n int
	if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='b'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration 0002 left table b: n=%d err=%v", n, err)
	}
}

func TestConcurrentMigrate(t *testing.T) {
	_, path := openTemp(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := Open(path) // separate pools, like separate processes
			if err != nil {
				errs <- err
				return
			}
			defer d.Close()
			errs <- Migrate(ctx, d)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestMigration0001MatchesSchemaDoc keeps docs/schema_v1.4.sql and the
// canonical migration from drifting apart (comments and PRAGMAs excluded).
func TestMigration0001MatchesSchemaDoc(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "schema_v1.4.sql"))
	if err != nil {
		t.Fatal(err)
	}
	mig, err := migrationFS.ReadFile("migrations/0001_schema_v1_4.sql")
	if err != nil {
		t.Fatal(err)
	}
	if a, b := ddlOnly(string(doc)), ddlOnly(string(mig)); a != b {
		t.Fatalf("docs/schema_v1.4.sql and migration 0001 differ:\n--- doc\n%s\n--- migration\n%s", a, b)
	}
}

func ddlOnly(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") || strings.HasPrefix(line, "PRAGMA ") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestMigrateNewTx: the schema is created inside the caller's transaction, so
// a rollback leaves an empty version-0 file and a commit the latest schema.
func TestMigrateNewTx(t *testing.T) {
	ctx := context.Background()
	sqlDB, _ := openTemp(t)
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateNewTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil || n != 0 {
		t.Fatalf("after rollback: %d schema objects, err %v", n, err)
	}
	if v, _ := SchemaVersion(ctx, sqlDB); v != 0 {
		t.Fatalf("after rollback: user_version %d", v)
	}

	tx, err = sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateNewTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if v, _ := SchemaVersion(ctx, sqlDB); v != LatestVersion() {
		t.Fatalf("user_version %d, want %d", v, LatestVersion())
	}
	// A database that already has a schema is refused.
	tx, err = sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := MigrateNewTx(ctx, tx); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("MigrateNewTx on migrated db = %v, want ErrForeignDatabase", err)
	}
}

// migrateTo applies only the embedded migrations up to version v.
func migrateTo(t *testing.T, sqlDB *sql.DB, v int) {
	t.Helper()
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{}
	for _, m := range ms[:v] {
		fsys["m/"+m.Name] = &fstest.MapFile{Data: []byte(m.SQL)}
	}
	if err := migrate(context.Background(), sqlDB, fsys, "m"); err != nil {
		t.Fatal(err)
	}
}

func TestMigration0004RecordsExistingBootstrapAccount(t *testing.T) {
	ctx := context.Background()
	// A database written by an earlier version: locked 'admin' owns a folder.
	sqlDB, _ := openTemp(t)
	migrateTo(t, sqlDB, 3)
	mustExec(t, sqlDB, "INSERT INTO users(user_id, login_name, display_name, password_hash, server_role, status) VALUES ('boot', 'admin', 'Administrator', '!locked', 'SERVER_ADMIN', 'active')")
	addUser(t, sqlDB, "other", "active")
	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'boot')")
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var id string
	var claimed sql.NullString
	if err := sqlDB.QueryRow("SELECT user_id, claimed_at FROM account_bootstrap WHERE singleton = 1").Scan(&id, &claimed); err != nil || id != "boot" || claimed.Valid {
		t.Fatalf("account_bootstrap = %q %v, %v", id, claimed, err)
	}
	expectReject(t, sqlDB, "CHECK constraint", "INSERT INTO account_bootstrap(singleton, user_id) VALUES (2, 'other')")
	expectReject(t, sqlDB, "UNIQUE constraint", "INSERT INTO account_bootstrap(singleton, user_id) VALUES (1, 'other')")
	expectReject(t, sqlDB, "FOREIGN KEY", "DELETE FROM users WHERE user_id = 'boot'")

	// No locked 'admin' (or a non-locked one): nothing is recorded.
	empty, _ := openTemp(t)
	migrateTo(t, empty, 3)
	mustExec(t, empty, "INSERT INTO users(user_id, login_name, display_name, password_hash) VALUES ('u', 'admin', 'x', '$argon2id$x')")
	if err := Migrate(ctx, empty); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := empty.QueryRow("SELECT count(*) FROM account_bootstrap").Scan(&n); err != nil || n != 0 {
		t.Fatalf("account_bootstrap rows %d, %v", n, err)
	}
}

// TestMigration0009AllowsDeclinedInvitations: the rebuilt folder_invitations
// keeps every row and column, accepts 'declined', still refuses unknown
// statuses and still allows only one pending invitation per folder and
// account, while a declined one does not block a new one.
func TestMigration0009AllowsDeclinedInvitations(t *testing.T) {
	ctx := context.Background()
	sqlDB, _ := openTemp(t)
	migrateTo(t, sqlDB, 8)
	addUser(t, sqlDB, "owner", "active")
	addUser(t, sqlDB, "alice", "active")
	addUser(t, sqlDB, "bob", "active")
	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'owner')")
	expectReject(t, sqlDB, "CHECK constraint", `INSERT INTO folder_invitations(invitation_id, folder_id, invited_user_id, inviter_user_id, role, status, expires_at)
		VALUES ('old', 'f1', 'alice', 'owner', 'VIEWER', 'declined', 'x')`)
	mustExec(t, sqlDB, `INSERT INTO folder_invitations(invitation_id, folder_id, invited_user_id, inviter_user_id, role, status, expires_at, accepted_at, created_at) VALUES
		('i1', 'f1', 'alice', 'owner', 'VIEWER', 'pending', 'e1', NULL, 'c1'),
		('i2', 'f1', 'bob', 'owner', 'CONTRIBUTOR', 'accepted', 'e2', 'a2', 'c2'),
		('i3', 'f1', 'bob', 'owner', 'UPLOADER', 'revoked', 'e3', NULL, 'c3')`)
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var got []string
	rows, err := sqlDB.Query(`SELECT invitation_id || folder_id || invited_user_id || inviter_user_id || role || COALESCE(token_sha256, '-') ||
		status || expires_at || COALESCE(accepted_at, '-') || created_at FROM folder_invitations ORDER BY invitation_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	rows.Close()
	if strings.Join(got, ",") != "i1f1aliceownerVIEWER-pendinge1-c1,i2f1bobownerCONTRIBUTOR-acceptede2a2c2,i3f1bobownerUPLOADER-revokede3-c3" {
		t.Fatalf("rows after migration: %v", got)
	}
	var n int
	if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE tbl_name = 'folder_invitations' AND type = 'index' AND name IN ('idx_invites_by_recipient', 'idx_invites_one_pending', 'idx_invites_by_folder')").Scan(&n); err != nil || n != 3 {
		t.Fatalf("indexes %d, %v", n, err)
	}
	if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name LIKE 'folder_invitations_%'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("old table left behind: %d, %v", n, err)
	}
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE folder_invitations SET status = 'bogus' WHERE invitation_id = 'i1'")
	expectReject(t, sqlDB, "UNIQUE constraint", `INSERT INTO folder_invitations(invitation_id, folder_id, invited_user_id, inviter_user_id, role, expires_at)
		VALUES ('i4', 'f1', 'alice', 'owner', 'VIEWER', 'e4')`)
	mustExec(t, sqlDB, "UPDATE folder_invitations SET status = 'declined' WHERE invitation_id = 'i1'")
	mustExec(t, sqlDB, `INSERT INTO folder_invitations(invitation_id, folder_id, invited_user_id, inviter_user_id, role, expires_at)
		VALUES ('i4', 'f1', 'alice', 'owner', 'VIEWER', 'e4')`)
	expectReject(t, sqlDB, "FOREIGN KEY", "DELETE FROM users WHERE user_id = 'alice'")
	mustExec(t, sqlDB, "DELETE FROM folders WHERE folder_id = 'f1'")
	if err := sqlDB.QueryRow("SELECT count(*) FROM folder_invitations").Scan(&n); err != nil || n != 0 {
		t.Fatalf("ON DELETE CASCADE lost: %d rows, %v", n, err)
	}
}

// TestMigration0010AddsCaptureTime: on a database of the previous version
// with originals, every existing blob becomes pending (the backfill) with
// no capture time, nothing else changes, and the constraints keep the
// columns consistent.
func TestMigration0010AddsCaptureTime(t *testing.T) {
	ctx := context.Background()
	sqlDB, _ := openTemp(t)
	migrateTo(t, sqlDB, 9)
	addUser(t, sqlDB, "owner", "active")
	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'owner')")
	for i, sha := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		mustExec(t, sqlDB, "INSERT INTO blobs(sha256, bytes, mime_type, relative_path, verified_at) VALUES (?, 10, 'image/jpeg', ?, 'v')", sha, "pool/"+sha)
		mustExec(t, sqlDB, "INSERT INTO assets(asset_id, original_sha256, original_filename, media_kind, uploaded_by_user_id) VALUES (?, ?, 'p.jpg', 'image', 'owner')", fmt.Sprint("asset", i), sha)
		mustExec(t, sqlDB, "INSERT INTO folder_assets(folder_id, asset_id) VALUES ('f1', ?)", fmt.Sprint("asset", i))
	}
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var pending, withTime, assets int
	if err := sqlDB.QueryRow("SELECT count(*) FILTER (WHERE capture_state = 'pending'), count(captured_at) + count(captured_utc) FROM blobs").Scan(&pending, &withTime); err != nil || pending != 2 || withTime != 0 {
		t.Fatalf("pending %d, with a capture time %d, %v", pending, withTime, err)
	}
	if err := sqlDB.QueryRow("SELECT count(*) FROM assets a JOIN folder_assets fa USING (asset_id)").Scan(&assets); err != nil || assets != 2 {
		t.Fatalf("assets %d, %v", assets, err)
	}
	var idx int
	if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'idx_blobs_capture_pending'").Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("index %d, %v", idx, err)
	}
	a := strings.Repeat("a", 64)
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET capture_state = 'bogus' WHERE sha256 = ?", a)
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET captured_at = '2026-10-19T14:03:22' WHERE sha256 = ?", a)
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET capture_state = 'done', captured_utc = '2026-10-19T05:03:22Z' WHERE sha256 = ?", a)
	mustExec(t, sqlDB, "UPDATE blobs SET capture_state = 'done', captured_at = '2026-10-19T14:03:22+09:00', captured_utc = '2026-10-19T05:03:22Z' WHERE sha256 = ?", a)
	// A new blob row starts pending.
	c := strings.Repeat("c", 64)
	mustExec(t, sqlDB, "INSERT INTO blobs(sha256, bytes, mime_type, relative_path, verified_at) VALUES (?, 10, 'image/jpeg', 'pool/c', 'v')", c)
	var state string
	if err := sqlDB.QueryRow("SELECT capture_state FROM blobs WHERE sha256 = ?", c).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("new blob %q, %v", state, err)
	}
}

// TestMigration0011LivePhotos: receipts of the previous version are kept
// unchanged (with no paired video), the new result and tables accept what
// a Live Photo commit writes, and a chunk on one component session extends
// its bundle and the other component together.
func TestMigration0011LivePhotos(t *testing.T) {
	ctx := context.Background()
	sqlDB, _ := openTemp(t)
	migrateTo(t, sqlDB, 10)
	addUser(t, sqlDB, "owner", "active")
	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'owner')")
	mustExec(t, sqlDB, "INSERT INTO devices(device_id, user_id, device_name, platform) VALUES ('d1', 'owner', 'phone', 'ios')")
	still, mov := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, sha := range []string{still, mov} {
		mustExec(t, sqlDB, "INSERT INTO blobs(sha256, bytes, mime_type, relative_path, verified_at) VALUES (?, 10, 'image/heic', ?, 'v')", sha, "pool/"+sha)
	}
	mustExec(t, sqlDB, "INSERT INTO assets(asset_id, original_sha256, original_filename, media_kind) VALUES ('a1', ?, 'IMG_1.HEIC', 'image')", still)
	mustExec(t, sqlDB, `INSERT INTO upload_receipts(user_id, device_id, folder_id, idempotency_key, sha256, bytes, result, asset_id, created_at)
		VALUES ('owner', 'd1', 'f1', 'k1', ?, 10, 'created', 'a1', 'c1')`, still)
	expectReject(t, sqlDB, "CHECK constraint", `INSERT INTO upload_receipts(user_id, device_id, folder_id, idempotency_key, sha256, bytes, result, asset_id, created_at)
		VALUES ('owner', 'd1', 'f1', 'k2', ?, 10, 'upgraded', 'a1', 'c2')`, still)
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var row string
	if err := sqlDB.QueryRow(`SELECT user_id || device_id || folder_id || idempotency_key || sha256 || bytes || result || asset_id || created_at || COALESCE(paired_sha256, '-')
		FROM upload_receipts`).Scan(&row); err != nil || row != "ownerd1f1k1"+still+"10createda1c1-" {
		t.Fatalf("receipt after migration %q, %v", row, err)
	}
	var n int
	if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name IN ('upload_receipts_0010') OR (type = 'index' AND name = 'idx_upload_receipts_created')").Scan(&n); err != nil || n != 1 {
		t.Fatalf("old table or index: %d, %v", n, err)
	}
	mustExec(t, sqlDB, `INSERT INTO upload_receipts(user_id, device_id, folder_id, idempotency_key, sha256, bytes, result, asset_id, created_at, paired_sha256)
		VALUES ('owner', 'd1', 'f1', 'k2', ?, 10, 'upgraded', 'a1', 'c2', ?)`, still, mov)
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE upload_receipts SET paired_sha256 = 'short' WHERE idempotency_key = 'k2'")
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE assets SET pairing_state = 'maybe'")
	mustExec(t, sqlDB, "UPDATE assets SET media_kind = 'live_photo', pairing_state = 'verified'")
	mustExec(t, sqlDB, "INSERT INTO asset_components(asset_id, role, sha256, filename, pairing_id, added_at) VALUES ('a1', 'paired_video', ?, 'IMG_1.MOV', 'ID', 't')", mov)
	expectReject(t, sqlDB, "UNIQUE constraint", "INSERT INTO asset_components(asset_id, role, sha256, filename, added_at) VALUES ('a1', 'paired_video', ?, 'x', 't')", mov)
	expectReject(t, sqlDB, "CHECK constraint", "INSERT INTO asset_components(asset_id, role, sha256, filename, added_at) VALUES ('a1', 'still', ?, 'x', 't')", mov)
	mustExec(t, sqlDB, "INSERT INTO assets(asset_id, original_sha256, original_filename, media_kind) VALUES ('a2', ?, 'x', 'image')", still)
	expectReject(t, sqlDB, "FOREIGN KEY", "INSERT INTO asset_components(asset_id, role, sha256, filename, added_at) VALUES ('a2', 'paired_video', ?, 'x', 't')", strings.Repeat("c", 64))
	expectReject(t, sqlDB, "FOREIGN KEY", "DELETE FROM assets WHERE asset_id = 'a1'")

	// A bundle with two component sessions; extending one extends all.
	mustExec(t, sqlDB, `INSERT INTO upload_bundles(bundle_id, user_id, device_id, folder_id, idempotency_key, kind, device_label, created_at, updated_at, expires_at)
		VALUES (?, 'owner', 'd1', 'f1', 'bk', 'live_photo', 'phone', 't0', 't0', 't1')`, strings.Repeat("1", 32))
	for i, role := range []string{"still", "paired_video"} {
		mustExec(t, sqlDB, `INSERT INTO upload_sessions(upload_id, user_id, device_id, folder_id, idempotency_key, sha256, total_bytes, filename, device_label,
			created_at, updated_at, expires_at, bundle_id, role) VALUES (?, 'owner', 'd1', 'f1', ?, ?, 10, 'x', 'phone', 't0', 't0', 't1', ?, ?)`,
			strings.Repeat(fmt.Sprint(i+2), 32), "#b/"+role, still, strings.Repeat("1", 32), role)
	}
	expectReject(t, sqlDB, "UNIQUE constraint", `INSERT INTO upload_sessions(upload_id, user_id, device_id, folder_id, idempotency_key, sha256, total_bytes, filename, device_label,
		created_at, updated_at, expires_at, bundle_id, role) VALUES (?, 'owner', 'd1', 'f1', 'again', ?, 10, 'x', 'phone', 't0', 't0', 't1', ?, 'still')`,
		strings.Repeat("4", 32), still, strings.Repeat("1", 32))
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE upload_sessions SET role = 'thumbnail' WHERE upload_id = ?", strings.Repeat("2", 32))
	mustExec(t, sqlDB, "UPDATE upload_sessions SET received_bytes = 5, updated_at = 't2', expires_at = 't3' WHERE upload_id = ?", strings.Repeat("2", 32))
	var exp string
	if err := sqlDB.QueryRow("SELECT group_concat(expires_at) FROM (SELECT expires_at FROM upload_bundles UNION ALL SELECT expires_at FROM upload_sessions ORDER BY 1)").Scan(&exp); err != nil || exp != "t3,t3,t3" {
		t.Fatalf("expiry after a chunk: %q, %v", exp, err)
	}
	mustExec(t, sqlDB, "DELETE FROM upload_bundles")
	if err := sqlDB.QueryRow("SELECT count(*) FROM upload_sessions").Scan(&n); err != nil || n != 0 {
		t.Fatalf("component sessions left after the bundle: %d, %v", n, err)
	}
}

// TestMigration0012FolderReceiptsDuration: settled capture times are kept,
// every original becomes pending for its duration (the backfill), and the
// folder receipt table holds one row per account, device and key.
func TestMigration0012FolderReceiptsDuration(t *testing.T) {
	ctx := context.Background()
	sqlDB, _ := openTemp(t)
	migrateTo(t, sqlDB, 11)
	addUser(t, sqlDB, "owner", "active")
	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'owner')")
	mustExec(t, sqlDB, "INSERT INTO devices(device_id, user_id, device_name, platform) VALUES ('d1', 'owner', 'phone', 'ios')")
	a := strings.Repeat("a", 64)
	mustExec(t, sqlDB, "INSERT INTO blobs(sha256, bytes, mime_type, relative_path, verified_at) VALUES (?, 10, 'video/quicktime', 'pool/a', 'v')", a)
	mustExec(t, sqlDB, "UPDATE blobs SET capture_state = 'done', captured_at = '2026-10-19T14:03:22+09:00', captured_utc = '2026-10-19T05:03:22Z'")
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var row string
	if err := sqlDB.QueryRow("SELECT capture_state || captured_at || duration_state || COALESCE(duration_ms, '-') FROM blobs").Scan(&row); err != nil ||
		row != "done2026-10-19T14:03:22+09:00pending-" {
		t.Fatalf("blob after migration %q, %v", row, err)
	}
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET duration_ms = 1000")
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET duration_state = 'done', duration_ms = 0")
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET duration_state = 'maybe'")
	mustExec(t, sqlDB, "UPDATE blobs SET duration_state = 'done', duration_ms = 14000")
	var idx int
	if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name IN ('idx_blobs_duration_pending', 'idx_folder_create_receipts_created')").Scan(&idx); err != nil || idx != 2 {
		t.Fatalf("indexes %d, %v", idx, err)
	}
	mustExec(t, sqlDB, "INSERT INTO folder_create_receipts(user_id, device_id, idempotency_key, folder_name, folder_id, created_at) VALUES ('owner', 'd1', 'k', 'Trip', 'f1', 't')")
	expectReject(t, sqlDB, "UNIQUE constraint", "INSERT INTO folder_create_receipts(user_id, device_id, idempotency_key, folder_name, folder_id, created_at) VALUES ('owner', 'd1', 'k', 'Other', 'f1', 't')")
	expectReject(t, sqlDB, "FOREIGN KEY", "INSERT INTO folder_create_receipts(user_id, device_id, idempotency_key, folder_name, folder_id, created_at) VALUES ('owner', 'd1', 'k2', 'x', 'nope', 't')")
	mustExec(t, sqlDB, "DELETE FROM folders WHERE folder_id = 'f1'")
	var n int
	if err := sqlDB.QueryRow("SELECT count(*) FROM folder_create_receipts").Scan(&n); err != nil || n != 0 {
		t.Fatalf("receipts of a deleted folder: %d, %v", n, err)
	}
}

// TestMigration0015MediaInfo: existing originals become pending (the
// backfill) and the columns accept only what the worker writes.
func TestMigration0015MediaInfo(t *testing.T) {
	ctx := context.Background()
	sqlDB, _ := openTemp(t)
	migrateTo(t, sqlDB, 14)
	a := strings.Repeat("a", 64)
	mustExec(t, sqlDB, "INSERT INTO blobs(sha256, bytes, mime_type, relative_path, verified_at) VALUES (?, 10, 'image/jpeg', 'pool/a', 'v')", a)
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	var state string
	var set int
	if err := sqlDB.QueryRow("SELECT media_state, count(camera_make) + count(camera_model) + count(camera_lens) + count(camera_focal_35mm) + count(pixel_width) + count(pixel_height) FROM blobs").Scan(&state, &set); err != nil || state != "pending" || set != 0 {
		t.Fatalf("after migration %q %d, %v", state, set, err)
	}
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET camera_model = 'X'")
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET media_state = 'maybe'")
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET media_state = 'done', camera_model = ''")
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET media_state = 'done', camera_lens = ?", strings.Repeat("x", 65))
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET media_state = 'done', camera_focal_35mm = 0")
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET media_state = 'done', pixel_width = 10")
	expectReject(t, sqlDB, "CHECK constraint", "UPDATE blobs SET media_state = 'done', pixel_width = 0, pixel_height = 10")
	mustExec(t, sqlDB, "UPDATE blobs SET media_state = 'done', camera_make = 'M', camera_model = 'X', camera_lens = 'L', camera_focal_35mm = 24, pixel_width = 4032, pixel_height = 3024")
	var idx int
	if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'idx_blobs_media_pending'").Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("index %d, %v", idx, err)
	}
}
