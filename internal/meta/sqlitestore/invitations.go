package sqlitestore

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/meta"
)

// Invitation history (meta.Memberships.InvitationHistory): the pending
// invitations of an invitee or a folder, and optionally the ones answered
// recently. Both lists are small (one pending invitation per folder and
// account, a 30-day window), so they are read whole, ordered and paged
// here, with the expiry decided on the same clock for every row.

// historyEntry is an invitation with its place in the history order:
// section 0 (pending) before 1 (answered), then key (created_at of a
// pending invitation, the answer time of an answered one) and ID, both
// descending.
type historyEntry struct {
	section int
	key, id string
}

func compareEntries(a, b historyEntry) int {
	if c := cmp.Compare(a.section, b.section); c != 0 {
		return c
	}
	if c := cmp.Compare(b.key, a.key); c != 0 {
		return c
	}
	return cmp.Compare(b.id, a.id)
}

func (e historyEntry) cursor() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(e.section) + "\x1f" + e.key + "\x1f" + e.id))
}

func parseHistoryCursor(s string) (historyEntry, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return historyEntry{}, meta.ErrInvalidCursor
	}
	parts := strings.Split(string(b), "\x1f")
	if len(parts) != 3 || (parts[0] != "0" && parts[0] != "1") || parts[1] == "" || parts[2] == "" {
		return historyEntry{}, meta.ErrInvalidCursor
	}
	return historyEntry{section: int(parts[0][0] - '0'), key: parts[1], id: parts[2]}, nil
}

// InvitationHistory implements meta.Memberships.
func (s *Store) InvitationHistory(ctx context.Context, q meta.InvitationQuery) (meta.InvitationPage, error) {
	if q.Limit <= 0 {
		return meta.InvitationPage{}, meta.ErrInvalidLimit
	}
	if (q.InviteeID == "") == (q.FolderID == "") {
		return meta.InvitationPage{}, errors.New("sqlitestore: InvitationHistory needs an invitee or a folder")
	}
	var after *historyEntry
	if q.After != "" {
		e, err := parseHistoryCursor(q.After)
		if err != nil {
			return meta.InvitationPage{}, err
		}
		after = &e
	}
	if q.Now.IsZero() {
		q.Now = time.Now()
	}
	// An invitation stops being pending at most InvitationTTL after it was
	// created, so nothing created earlier can be pending or answered in
	// the window.
	from := q.Now
	if q.Recent && q.Since.Before(from) {
		from = q.Since
	}
	where, arg := "i.invited_user_id = ?", q.InviteeID
	if q.FolderID != "" {
		where, arg = "i.folder_id = ?", q.FolderID
	}
	where += " AND i.created_at >= ?"
	if !q.Recent {
		where += " AND i.status = 'pending'"
	}
	rows, err := s.db.QueryContext(ctx, invitationQuery+where, arg, from.Add(-meta.InvitationTTL).UTC().Format(timeLayout))
	if err != nil {
		return meta.InvitationPage{}, err
	}
	defer rows.Close()
	since := q.Since.UTC().Format(timeLayout)
	type item struct {
		e   historyEntry
		inv meta.Invitation
	}
	var items []item
	for rows.Next() {
		r, err := scanInvitation(rows, q.Now)
		if err != nil {
			return meta.InvitationPage{}, err
		}
		reason := r.gone()
		if reason == "" {
			items = append(items, item{historyEntry{0, r.CreatedAt, r.ID}, r.Invitation})
			continue
		}
		if !q.Recent || reason == meta.ReasonReplaced {
			continue
		}
		if r.Status == meta.InvitationPending {
			// Still stored pending although its folder was archived,
			// deleted or changed owner: shown as revoked, time unknown.
			r.Status, r.Reason = meta.InvitationRevoked, reason
		}
		key := r.AnsweredAt
		if key == "" {
			key = r.CreatedAt // answered before the time was recorded
		}
		if key < since {
			continue
		}
		items = append(items, item{historyEntry{1, key, r.ID}, r.Invitation})
	}
	if err := rows.Err(); err != nil {
		return meta.InvitationPage{}, err
	}
	slices.SortFunc(items, func(a, b item) int { return compareEntries(a.e, b.e) })
	start := 0
	if after != nil {
		start, _ = slices.BinarySearchFunc(items, *after, func(it item, t historyEntry) int { return compareEntries(it.e, t) })
		if start < len(items) && compareEntries(items[start].e, *after) == 0 {
			start++
		}
	}
	page := meta.InvitationPage{Invitations: []meta.Invitation{}}
	for i := start; i < len(items) && len(page.Invitations) < q.Limit; i++ {
		page.Invitations = append(page.Invitations, items[i].inv)
		if len(page.Invitations) == q.Limit && i+1 < len(items) {
			page.NextCursor = items[i].e.cursor()
		}
	}
	return page, nil
}
