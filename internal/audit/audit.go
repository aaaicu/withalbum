// Package audit writes rows to the audit_log table.
//
// Record runs inside the caller's transaction, so an audited change and its
// audit row commit or roll back together. Entries must never carry secrets:
// Record refuses detail keys that look like passwords, hashes or tokens.
//
// Draft: only account changes (internal/users) are recorded so far. Denied
// attempts, logins and album events follow with the HTTP API.
package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Result values of audit_log.result.
const (
	Allowed = "allowed"
	Denied  = "denied"
	Error   = "error"
)

// Entry is one audit_log row.
type Entry struct {
	ActorUserID string // "" when the actor is the server console (stored as NULL)
	Action      string // e.g. "account.create"
	TargetType  string // e.g. "user"
	TargetID    string
	Result      string // Allowed, Denied or Error
	Details     map[string]any
}

// ErrSecretDetail is returned for a detail key that may hold secret material.
var ErrSecretDetail = errors.New("audit: detail key may contain secret material")

var secretWords = []string{"password", "passwd", "hash", "token", "secret", "salt"}

// Record inserts e inside tx.
func Record(ctx context.Context, tx *sql.Tx, e Entry) error {
	if e.Action == "" || e.TargetType == "" {
		return errors.New("audit: action and target type are required")
	}
	switch e.Result {
	case Allowed, Denied, Error:
	default:
		return fmt.Errorf("audit: invalid result %q", e.Result)
	}
	var details sql.NullString
	if len(e.Details) > 0 {
		for k := range e.Details {
			lk := strings.ToLower(k)
			for _, w := range secretWords {
				if strings.Contains(lk, w) {
					return fmt.Errorf("%w: %q", ErrSecretDetail, k)
				}
			}
		}
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("audit: details: %w", err)
		}
		details = sql.NullString{String: string(b), Valid: true}
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return fmt.Errorf("audit: event id: %w", err)
	}
	actor := sql.NullString{String: e.ActorUserID, Valid: e.ActorUserID != ""}
	target := sql.NullString{String: e.TargetID, Valid: e.TargetID != ""}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_log
		(event_id, actor_user_id, action, target_type, target_id, result, occurred_at, details_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		hex.EncodeToString(id), actor, e.Action, e.TargetType, target, e.Result,
		time.Now().UTC().Format(time.RFC3339Nano), details)
	return err
}
