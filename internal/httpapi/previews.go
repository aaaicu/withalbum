package httpapi

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/meta"
)

// The preview worker makes the previews an external converter handles
// (blobstore.Converter: HEIC and other stills with sips, videos with
// ffmpeg). Uploads of such types commit with preview status "pending" and
// wake the worker; the worker takes pending originals from the database
// (so nothing is lost by a restart or a crash), converts them one at a
// time, and records "ready", "failed" or "unsupported" for every asset of
// those bytes in one transaction that also appends an asset_updated event
// to each folder holding one, so syncing clients learn about it.

// previewBatch is how many pending originals one ProcessPreviews call takes.
const previewBatch = 16

// previewRescan is how often the worker looks for pending previews without
// being woken (pending rows left by a crash, or a busy database).
const previewRescan = time.Minute

// kickPreviews wakes the preview worker without waiting.
func (s *Server) kickPreviews() {
	select {
	case s.previewKick <- struct{}{}:
	default:
	}
}

// RunPreviews runs the preview worker until ctx ends.
func (s *Server) RunPreviews(ctx context.Context) {
	t := time.NewTicker(previewRescan)
	defer t.Stop()
	for {
		for {
			n, err := s.ProcessPreviews(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("previews: %v", err)
			}
			if err != nil || n < previewBatch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.previewKick:
		case <-t.C:
		}
	}
}

// ProcessPreviews converts one batch of pending previews and returns how
// many originals it settled. A storage problem stops the batch and leaves
// the rest pending.
func (s *Server) ProcessPreviews(ctx context.Context) (int, error) {
	jobs, err := s.meta.PendingPreviews(ctx, previewBatch)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		if err := s.vol.Check(); err != nil {
			return done, err
		}
		status := meta.PreviewStatusReady
		err := s.blobs.MakeConvertedPreview(ctx, j.SHA256, j.MimeType)
		switch {
		case err == nil:
		case errors.Is(err, blobstore.ErrNoConverter):
			status = meta.PreviewStatusUnsupported
		case ctx.Err() != nil:
			return done, ctx.Err()
		case s.vol.Check() != nil:
			// The volume went away: try again once it is back.
			return done, err
		default:
			// ErrConvert, or an unreadable original: the preview failed for
			// good (the original itself is kept as it is).
			log.Printf("previews: %s (%s): %v", j.SHA256, j.MimeType, err)
			status = meta.PreviewStatusFailed
		}
		if _, err := s.meta.SetPreviewStatus(ctx, j.SHA256, status); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}
