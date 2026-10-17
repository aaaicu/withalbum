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
// session.SessionRetention ago, used refresh tokens past their expiry and
// upload receipts older than meta.ReceiptTTL. It writes nothing when the
// storage volume does not verify.
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
	if r.Sessions+r.UsedRefresh+n > 0 {
		log.Printf("maintenance: pruned %d old session(s), %d used refresh token(s), %d upload receipt(s)", r.Sessions, r.UsedRefresh, n)
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
