package audit

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aaaicu/withalbum/internal/db"
)

func TestRecord(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "withalbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Record(ctx, tx, Entry{Action: "account.create", TargetType: "user", TargetID: "u1", Result: Allowed,
		Details: map[string]any{"login": "bob", "via": "console"}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Entry{
		{Action: "x", TargetType: "user", Result: "maybe"},
		{Action: "", TargetType: "user", Result: Allowed},
		{Action: "x", TargetType: "user", Result: Allowed, Details: map[string]any{"new_password": "p"}},
		{Action: "x", TargetType: "user", Result: Allowed, Details: map[string]any{"PasswordHash": "h"}},
		{Action: "x", TargetType: "user", Result: Allowed, Details: map[string]any{"refresh_token": "t"}},
	} {
		if err := Record(ctx, tx, bad); err == nil {
			t.Fatalf("Record(%+v) accepted", bad)
		} else if len(bad.Details) > 0 && !errors.Is(err, ErrSecretDetail) {
			t.Fatalf("Record(%+v) = %v, want ErrSecretDetail", bad, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var actor sql.NullString
	var action, target, result, details string
	if err := sqlDB.QueryRow("SELECT actor_user_id, action, target_id, result, details_json FROM audit_log").Scan(&actor, &action, &target, &result, &details); err != nil {
		t.Fatal(err)
	}
	if actor.Valid || action != "account.create" || target != "u1" || result != Allowed || details != `{"login":"bob","via":"console"}` {
		t.Fatalf("row = %v %s %s %s %s", actor, action, target, result, details)
	}
}
