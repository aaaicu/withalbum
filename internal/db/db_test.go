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
	if len(ms) != 3 || LatestVersion() != 3 {
		t.Fatalf("got %d migrations, LatestVersion %d; want 3", len(ms), LatestVersion())
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
		"upload_receipts", "audit_log", "legacy_imports", "legacy_asset_map",
	} {
		var n int
		if err := sqlDB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: count=%d err=%v", table, n, err)
		}
	}
	for _, tc := range [][2]string{{"folder_assets", "uploader_device_label"}, {"folder_changes", "uploader_device_label"}, {"assets", "preview_ready"}} {
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
