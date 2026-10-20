package httpapi

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/aaaicu/withalbum/internal/capturetime"
	"github.com/aaaicu/withalbum/internal/meta"
)

// The capture-time worker reads when each original was taken and, for a
// movie, how long it plays (internal/capturetime) and records both for
// every asset of those bytes (meta.SetCaptureTimes, which appends
// asset_updated events when it found something). It runs after the commit, not inside it: the upload path stays the
// same for new and already stored bytes (an upload-only caller must not
// learn from the response time whether the bytes were known), and a slow
// disk never holds up an upload. Pending originals are read from the
// database, so uploads made while it was busy, originals stored before
// this feature (the migration marks them all pending: the backfill) and
// work interrupted by a restart are all picked up the same way.

const (
	// captureBatch is how many originals one ProcessCaptures call reads.
	captureBatch = 64
	// captureRescan is how often the worker looks for pending originals
	// without being woken (after a busy database or a missing volume).
	captureRescan = time.Minute
	// capturePause separates full batches during a backfill, so that the
	// worker leaves the disk and the database write lock to uploads.
	capturePause = 100 * time.Millisecond
)

// kickCaptures wakes the capture-time worker without waiting.
func (s *Server) kickCaptures() {
	select {
	case s.captureKick <- struct{}{}:
	default:
	}
}

// RunCaptures runs the capture-time worker until ctx ends.
func (s *Server) RunCaptures(ctx context.Context) {
	t := time.NewTicker(captureRescan)
	defer t.Stop()
	for {
		for {
			n, err := s.ProcessCaptures(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("capture times: %v", err)
			}
			if err != nil || n < captureBatch {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(capturePause):
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.captureKick:
		case <-t.C:
		}
	}
}

// ProcessCaptures reads the capture times of one batch of pending
// originals and records them in one transaction. It returns how many
// originals it settled. If the volume is unavailable it records nothing
// for the original it was reading and stops (they stay pending).
func (s *Server) ProcessCaptures(ctx context.Context) (int, error) {
	shas, err := s.meta.PendingCaptures(ctx, captureBatch)
	if err != nil || len(shas) == 0 {
		return 0, err
	}
	results := make([]meta.CaptureResult, 0, len(shas))
	var stop error
	for _, sha := range shas {
		if err := ctx.Err(); err != nil {
			stop = err
			break
		}
		if err := s.vol.Check(); err != nil {
			stop = err
			break
		}
		t, ms, err := s.readCaptureTime(sha)
		if err != nil {
			if s.vol.Check() != nil {
				stop = err // the volume went away: try again once it is back
				break
			}
			// An unreadable original (missing, damaged disk sector): its
			// capture time stays unknown; integrity checks report the file.
			log.Printf("capture times: %s: %v", sha, err)
		}
		r := meta.CaptureResult{SHA256: sha, CapturedAt: t.String(), DurationMS: ms}
		if in, ok := t.Instant(); ok {
			r.CapturedUTC = in.Format(time.RFC3339)
		}
		results = append(results, r)
	}
	if len(results) > 0 {
		if _, err := s.meta.SetCaptureTimes(context.WithoutCancel(ctx), results); err != nil {
			return 0, err
		}
	}
	return len(results), stop
}

// readCaptureTime reads the capture time and the duration (0: none) of
// the pool file of sha. The file is only read (capturetime bounds how
// much).
func (s *Server) readCaptureTime(sha string) (capturetime.Time, int64, error) {
	f, err := os.Open(s.blobs.PoolPath(sha))
	if err != nil {
		return capturetime.Time{}, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return capturetime.Time{}, 0, err
	}
	if !fi.Mode().IsRegular() {
		return capturetime.Time{}, 0, errors.New("pool entry is not a regular file")
	}
	t, err := capturetime.Extract(f, fi.Size())
	if err != nil {
		return capturetime.Time{}, 0, err
	}
	ms, err := capturetime.Duration(f, fi.Size())
	if err != nil {
		return capturetime.Time{}, 0, err
	}
	return t, ms, nil
}
