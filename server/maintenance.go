package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/aaaicu/withalbum/internal/meta"
)

// maintenanceInterval is how often the running server prunes old rows.
const maintenanceInterval = 6 * time.Hour

// prune deletes session rows revoked or expired more than
// session.SessionRetention ago, used refresh tokens past their expiry,
// upload receipts older than meta.ReceiptTTL and expired upload sessions
// with their partial files, and purges what was deleted or removed more
// than meta.TrashRetention ago (the album trash, deleted albums and the
// originals no other album holds). It writes nothing when the storage
// volume does not verify.
func (a *app) prune(ctx context.Context, now time.Time) error {
	if err := a.vol.Check(); err != nil {
		return fmt.Errorf("storage unavailable, nothing pruned: %w", err)
	}
	r, err := a.auth.Sessions.Prune(ctx)
	if err != nil {
		return fmt.Errorf("prune sessions: %w", err)
	}
	n, err := a.meta.PruneUploadReceipts(ctx, now.Add(-meta.ReceiptTTL))
	if err != nil {
		return fmt.Errorf("prune upload receipts: %w", err)
	}
	u, err := a.api.PruneUploads(ctx, now)
	if err != nil {
		return fmt.Errorf("prune upload sessions: %w", err)
	}
	if r.Sessions+r.UsedRefresh+n+int64(u) > 0 {
		log.Printf("maintenance: pruned %d old session(s), %d used refresh token(s), %d upload receipt(s), %d expired upload session(s)",
			r.Sessions, r.UsedRefresh, n, u)
	}
	p, err := a.api.PurgeExpired(ctx, now)
	if p.TrashPurged+p.FoldersPurged > 0 {
		log.Printf("maintenance: purged %d removal(s) from album trashes, %d deleted album(s), %d original(s) no album holds",
			p.TrashPurged, p.FoldersPurged, p.OriginalsPurged)
	}
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	return nil
}

// maintain prunes once now and then every interval until ctx ends.
func (a *app) maintain(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := a.prune(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("maintenance: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
