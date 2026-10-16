package db

import (
	"database/sql"
	"strings"
	"testing"
)

// mustExec runs q and fails the test on error.
func mustExec(t *testing.T, sqlDB *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := sqlDB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// expectReject runs q and requires an error whose text contains want.
func expectReject(t *testing.T, sqlDB *sql.DB, want, q string, args ...any) {
	t.Helper()
	_, err := sqlDB.Exec(q, args...)
	if err == nil {
		t.Fatalf("%s: accepted, want rejection containing %q", q, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: error %q does not contain %q", q, err, want)
	}
}

func addUser(t *testing.T, sqlDB *sql.DB, id, status string) {
	t.Helper()
	mustExec(t, sqlDB, "INSERT INTO users(user_id, login_name, display_name, password_hash, status) VALUES (?, ?, ?, 'x', ?)",
		id, id, id, status)
}

func TestTriggerOwnerCannotBeMember(t *testing.T) {
	sqlDB := openMigrated(t)
	addUser(t, sqlDB, "owner", "active")
	addUser(t, sqlDB, "other", "active")
	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'owner')")

	expectReject(t, sqlDB, "owner cannot be a member",
		"INSERT INTO folder_members(folder_id, user_id, role) VALUES ('f1', 'owner', 'VIEWER')")

	mustExec(t, sqlDB, "INSERT INTO folder_members(folder_id, user_id, role) VALUES ('f1', 'other', 'VIEWER')")
	expectReject(t, sqlDB, "owner cannot be a member",
		"UPDATE folder_members SET user_id='owner' WHERE folder_id='f1' AND user_id='other'")
}

func TestTriggerOwnerMustBeActive(t *testing.T) {
	sqlDB := openMigrated(t)
	addUser(t, sqlDB, "active", "active")
	addUser(t, sqlDB, "disabled", "disabled")

	expectReject(t, sqlDB, "owner must be active",
		"INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'disabled')")

	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'active')")
	expectReject(t, sqlDB, "owner must be active",
		"UPDATE folders SET owner_user_id='disabled' WHERE folder_id='f1'")
}

func TestTriggerCannotDisableOwner(t *testing.T) {
	sqlDB := openMigrated(t)
	addUser(t, sqlDB, "owner", "active")
	addUser(t, sqlDB, "plain", "active")
	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'owner')")

	expectReject(t, sqlDB, "transfer owned albums before disabling account",
		"UPDATE users SET status='disabled' WHERE user_id='owner'")
	// A user who owns nothing can be disabled.
	mustExec(t, sqlDB, "UPDATE users SET status='disabled' WHERE user_id='plain'")
}

func TestForeignKeysEnforced(t *testing.T) {
	sqlDB := openMigrated(t)
	addUser(t, sqlDB, "u1", "active")
	mustExec(t, sqlDB, "INSERT INTO folders(folder_id, folder_name, owner_user_id) VALUES ('f1', 'Trip', 'u1')")

	expectReject(t, sqlDB, "FOREIGN KEY constraint failed",
		"INSERT INTO folder_members(folder_id, user_id, role) VALUES ('f1', 'ghost', 'VIEWER')")
	expectReject(t, sqlDB, "FOREIGN KEY constraint failed",
		"INSERT INTO devices(device_id, user_id, platform, device_name) VALUES ('d1', 'ghost', 'ios', 'phone')")
	expectReject(t, sqlDB, "FOREIGN KEY constraint failed",
		"INSERT INTO assets(asset_id, original_sha256, original_filename, media_kind) VALUES ('a1', ?, 'x.jpg', 'image')",
		strings.Repeat("a", 64))
	// ON DELETE RESTRICT: an owner row cannot be deleted while it owns a folder.
	expectReject(t, sqlDB, "FOREIGN KEY constraint failed", "DELETE FROM users WHERE user_id='u1'")
	// Migration 0002: legacy_asset_map must point at a real asset.
	expectReject(t, sqlDB, "FOREIGN KEY constraint failed",
		"INSERT INTO legacy_asset_map(legacy_sha256, asset_id) VALUES (?, 'missing')", strings.Repeat("b", 64))
}
