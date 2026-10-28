package httpapi

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/aaaicu/withalbum/internal/audit"
)

// Authorization denials (403 forbidden, 404 folder_not_found of an album
// route, and refusals at upload commit) are written to the audit log as a
// summary: the first denial of an account on a route with a status is
// recorded at once (action "access.denied"); further identical denials in
// the next DenialWindow are only counted, and the count is written with the
// next recorded row for that key ("suppressed_before"). Writes are thus
// bounded per account and route, so probing album IDs cannot flood the
// database. Unauthenticated requests (401) are never recorded: there is no
// account to attribute them to (login failures have their own entries).
// Counts not yet written are lost when the server stops.

// DenialWindow is how long identical denials are summarized.
const DenialWindow = 10 * time.Minute

// maxDenialKeys bounds the summary table; when full, entries older than the
// window are dropped and, if none are, the new denial is only logged.
const maxDenialKeys = 4096

type denialKey struct {
	user, route string
	status      int
}

type denialEntry struct {
	since      time.Time
	suppressed int
}

type denialLog struct {
	mu   sync.Mutex
	seen map[denialKey]*denialEntry
	db   *sql.DB
	now  func() time.Time
}

func newDenialLog(db *sql.DB) *denialLog {
	return &denialLog{seen: map[denialKey]*denialEntry{}, db: db, now: time.Now}
}

// take decides whether a denial is written now and returns how many
// identical ones were suppressed before it.
func (d *denialLog) take(k denialKey) (write bool, suppressed int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if e := d.seen[k]; e != nil {
		if now.Sub(e.since) < DenialWindow {
			e.suppressed++
			return false, 0
		}
		suppressed = e.suppressed
	} else if len(d.seen) >= maxDenialKeys {
		for key, e := range d.seen {
			if now.Sub(e.since) >= DenialWindow {
				delete(d.seen, key)
			}
		}
		if len(d.seen) >= maxDenialKeys {
			return false, 0
		}
	}
	d.seen[k] = &denialEntry{since: now}
	return true, suppressed
}

// maxDeniedID bounds the requested folder ID kept in a denial entry.
const maxDeniedID = 64

// denied records an authorization denial of userID on route (a declared
// route pattern). folderID is the requested folder, if any. A failure to
// write is logged and never changes the response.
func (s *Server) denied(r *http.Request, userID, route string, status int, folderID string) {
	if s.auth == nil || s.denials == nil || userID == "" {
		return
	}
	write, suppressed := s.denials.take(denialKey{user: userID, route: route, status: status})
	if !write {
		return
	}
	details := map[string]any{"status": status, "ip": clientIP(r), "suppressed_before": suppressed}
	if folderID != "" {
		details["folder_id"] = truncateID(folderID)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	tx, err := s.denials.db.BeginTx(ctx, nil)
	if err == nil {
		err = audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "access.denied", TargetType: "route", TargetID: route,
			Result: audit.Denied, Details: details})
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	if err != nil {
		log.Printf("audit access denial failed: %v", err)
	}
}

func truncateID(id string) string {
	if len(id) > maxDeniedID {
		return id[:maxDeniedID]
	}
	return id
}
