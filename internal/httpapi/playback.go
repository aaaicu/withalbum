package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// The playback worker (M50) makes the playback copy of each video asset's
// original (blobstore.MakePlayback: H.264 + AAC MP4, at most 1080p, fast
// start) one at a time, after the commit and at a low priority, and
// records the result for every asset of those bytes
// (meta.PlaybackStore.SetPlayback, which appends asset_updated). Linking a
// video asset queues its original in the commit transaction (a trigger),
// so the upload path is the same for every caller and nothing is lost by
// a restart; the migration queued the existing videos (the backfill;
// newer ones are taken first).
//
// A failed run is tried again after 10 minutes, then 40, and is "failed"
// after the third. A missing tool is "unsupported" (requeued at the next
// start that has one); a copy that is not meaningfully smaller than the
// original is not kept ("unsupported", clients stream the original).
// While the storage is unavailable or short of space the worker pauses;
// on shutdown the running tool is killed and the original stays pending.
// When no video asset in use holds an original any more its copy is
// deleted (the row becomes idle and is queued again by a new link).

const (
	playbackBatch       = 8
	playbackRescan      = time.Minute
	playbackPause       = 2 * time.Second // between conversions, for uploads
	playbackMaxAttempts = 3
	playbackRetryBase   = 10 * time.Minute
	// playbackMaxDuration: longer videos are not converted (too_long).
	playbackMaxDuration = 3 * time.Hour
	// playbackReleaseBatch bounds one ReleasePlayback pass.
	playbackReleaseBatch = 64
)

// PlaybackOriginalChanged is the failure reason when the original changed
// while the tool ran (never expected; see blobstore.ErrOriginalChanged).
const PlaybackOriginalChanged = "original_changed"

// defaultPlaybackLimit is the time a tool gets for a video of durMS
// milliseconds (0: unknown): four times its length plus two minutes,
// between five minutes and two hours; an hour when the length is unknown.
func defaultPlaybackLimit(durMS int64) time.Duration {
	if durMS <= 0 {
		return time.Hour
	}
	d := 2*time.Minute + 4*time.Duration(durMS)*time.Millisecond
	return min(max(d, 5*time.Minute), 2*time.Hour)
}

// playbackRetryAt is when a run that failed attempts times is tried again.
func playbackRetryAt(now time.Time, attempts int) time.Time {
	d := playbackRetryBase
	for i := 1; i < attempts; i++ {
		d *= 4
	}
	return now.Add(d)
}

// kickPlayback wakes the playback worker without waiting.
func (s *Server) kickPlayback() {
	select {
	case s.playbackKick <- struct{}{}:
	default:
	}
}

// RunPlayback runs the playback worker until ctx ends. Without a
// PlaybackStore it returns at once.
func (s *Server) RunPlayback(ctx context.Context) {
	if s.playback == nil {
		return
	}
	t := time.NewTicker(playbackRescan)
	defer t.Stop()
	prepared := false
	for {
		if !prepared {
			if err := s.PreparePlayback(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Printf("playback: startup check: %v", err)
				}
			} else {
				prepared = true
			}
		}
		if prepared {
			if _, err := s.ReleasePlayback(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("playback: release: %v", err)
			}
			for {
				n, err := s.ProcessPlayback(ctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("playback: %v", err)
				}
				if err != nil || n == 0 {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.playbackKick:
		case <-t.C:
		}
	}
}

// PreparePlayback runs once before the first conversion: it deletes copies
// the database does not report ready (a crash between the file and the
// commit), requeues ready copies whose file is missing or not of the
// recorded size (deleted, or a restored database), and, when this server
// has a tool, requeues the originals that were unsupported for want of
// one.
func (s *Server) PreparePlayback(ctx context.Context) error {
	if err := s.vol.Check(); err != nil {
		return err
	}
	if _, err := s.blobs.SweepPlayback(ctx, func(sha string) (bool, error) {
		st, err := s.playback.PlaybackStatus(ctx, sha)
		return st == meta.PlaybackReady, err
	}); err != nil {
		return err
	}
	after := ""
	for {
		ready, err := s.playback.ReadyPlayback(ctx, after, 256)
		if err != nil {
			return err
		}
		for _, j := range ready {
			n, err := s.blobs.PlaybackSize(j.SHA256)
			if err != nil {
				return err
			}
			if n == j.Bytes {
				continue
			}
			if err := s.vol.Check(); err != nil {
				return err
			}
			if n > 0 {
				// Another copy than the one recorded (a restored
				// database): make it again.
				if err := s.blobs.RemovePlayback(j.SHA256); err != nil {
					return err
				}
			}
			if _, err := s.playback.RequeuePlayback(ctx, j.SHA256, meta.PlaybackReady, meta.PlaybackFileMissing); err != nil {
				return err
			}
		}
		if len(ready) < 256 {
			break
		}
		after = ready[len(ready)-1].SHA256
	}
	if s.blobs.Transcoder() != nil {
		if _, err := s.playback.RequeueUnsupported(ctx, meta.PlaybackNoConverter); err != nil {
			return err
		}
	}
	return nil
}

// ReleasePlayback deletes the copies of originals that no video asset in
// use holds any more and returns how many it released.
func (s *Server) ReleasePlayback(ctx context.Context) (int, error) {
	shas, err := s.playback.UnreferencedPlayback(ctx, playbackReleaseBatch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sha := range shas {
		if err := s.vol.Check(); err != nil {
			return n, err
		}
		// The database first: once it says idle nothing serves the file,
		// and a link made afterwards queues a new copy (this worker makes
		// it, after this deletion). A crash before the deletion leaves a
		// file the next start sweeps away.
		ok, err := s.playback.ReleasePlayback(ctx, sha)
		if err != nil {
			return n, err
		}
		if !ok {
			continue
		}
		if err := s.blobs.RemovePlayback(sha); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ProcessPlayback converts one batch of pending originals and returns how
// many it settled (retries included). It stops at a storage problem or a
// lack of free space, leaving the rest pending.
func (s *Server) ProcessPlayback(ctx context.Context) (int, error) {
	if s.playback == nil {
		return 0, nil
	}
	jobs, err := s.playback.PendingPlayback(ctx, s.playbackNow(), playbackBatch)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	done := 0
	ran := false // the tool ran for the previous job
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		if ran {
			select {
			case <-ctx.Done():
				return done, ctx.Err()
			case <-time.After(s.playbackGap):
			}
		}
		if err := s.vol.Check(); err != nil {
			return done, err
		}
		if !s.playbackRoom(j.Bytes) {
			return done, errors.New("not enough free space for a playback copy; waiting")
		}
		r, err := s.convertPlayback(ctx, j)
		if err != nil {
			return done, err
		}
		ran = s.blobs.Transcoder() != nil && r.Reason != meta.PlaybackTooLong
		if _, err := s.playback.SetPlayback(context.WithoutCancel(ctx), r); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// playbackRoom reports whether the volume can take a copy of an original
// of size bytes (at most its size) above the upload reserve.
func (s *Server) playbackRoom(size int64) bool {
	free, err := s.freeBytes()
	if errors.Is(err, storage.ErrFreeSpaceUnsupported) {
		return true
	}
	return err == nil && free > uint64(s.minFree)+uint64(max(size, 0))
}

// convertPlayback runs the tool for j and returns the result to record.
// An error means "stop and try later" (shutdown, storage).
func (s *Server) convertPlayback(ctx context.Context, j meta.PlaybackJob) (meta.PlaybackResult, error) {
	r := meta.PlaybackResult{SHA256: j.SHA256, Attempts: j.Attempts}
	if time.Duration(j.DurationMS)*time.Millisecond > playbackMaxDuration {
		r.Status, r.Reason = meta.PlaybackUnsupported, meta.PlaybackTooLong
		return r, nil
	}
	f, err := s.blobs.MakePlayback(ctx, j.SHA256, s.playbackLimit(j.DurationMS))
	switch {
	case err == nil:
		r.Status, r.Bytes, r.Width, r.Height = meta.PlaybackReady, f.Bytes, f.Width, f.Height
		return r, nil
	case errors.Is(err, blobstore.ErrNoTranscoder):
		r.Status, r.Reason = meta.PlaybackUnsupported, meta.PlaybackNoConverter
		return r, nil
	case errors.Is(err, blobstore.ErrPlaybackNotSmaller):
		r.Status, r.Reason = meta.PlaybackUnsupported, meta.PlaybackOriginalSmall
		return r, nil
	case ctx.Err() != nil:
		return r, ctx.Err()
	case s.vol.Check() != nil, storage.IsNoSpace(err):
		return r, err // the volume went away or is full: try again later
	case errors.Is(err, blobstore.ErrOriginalChanged):
		log.Printf("playback: %s: %v", j.SHA256, err)
		r.Status, r.Reason, r.Attempts = meta.PlaybackFailed, PlaybackOriginalChanged, j.Attempts+1
		return r, nil
	}
	log.Printf("playback: %s (%s): %v", j.SHA256, j.MimeType, err)
	r.Attempts = j.Attempts + 1
	r.Reason = meta.PlaybackConvertError
	switch {
	case errors.Is(err, blobstore.ErrTranscodeTimeout):
		r.Reason = meta.PlaybackTimeout
	case errors.Is(err, blobstore.ErrPlaybackOutput):
		r.Reason = meta.PlaybackBadOutput
	}
	if r.Attempts >= playbackMaxAttempts {
		r.Status = meta.PlaybackFailed
	} else {
		r.Status, r.RetryAt = meta.PlaybackPending, playbackRetryAt(s.playbackNow(), r.Attempts)
	}
	return r, nil
}

// playbackJSON is the playback copy state of a video asset.
type playbackJSON struct {
	// Status: pending (being made; stream the original meanwhile, an
	// asset_updated event follows), ready (GET .../playback), failed or
	// unsupported (stream the original).
	Status   string  `json:"status"`
	Bytes    *int64  `json:"bytes"`
	Width    *int    `json:"width"`
	Height   *int    `json:"height"`
	MimeType *string `json:"mime_type"`
}

// playbackView is the playback object of an asset: null for everything
// but a video asset (a live photo's own original is its still).
func playbackView(as meta.Asset) *playbackJSON {
	if as.MediaKind != meta.MediaVideo {
		return nil
	}
	p := as.Playback
	if p == nil {
		// A video without a row (a store without playback copies): no copy
		// will come.
		return &playbackJSON{Status: meta.PlaybackUnsupported}
	}
	v := &playbackJSON{Status: p.Status}
	if p.Status == meta.PlaybackReady {
		b, w, h, m := p.Bytes, p.Width, p.Height, "video/mp4"
		v.Bytes, v.Width, v.Height, v.MimeType = &b, &w, &h, &m
	}
	return v
}

// getPlayback serves the playback copy of a video asset linked to the
// folder in the path, like the original (same route policy, Range,
// If-Range, attachment, sandbox CSP), as video/mp4. 404 playback_not_ready
// while there is none.
func (s *Server) getPlayback(w http.ResponseWriter, r *http.Request, a albumRequest) {
	asset, err := s.meta.GetFolderAsset(r.Context(), a.Folder.Folder.ID, r.PathValue("assetID"), a.view())
	if errors.Is(err, meta.ErrAssetNotFound) {
		errorReply(w, 404, "asset_not_found")
		return
	}
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	if asset.MediaKind != meta.MediaVideo || asset.Playback == nil || asset.Playback.Status != meta.PlaybackReady {
		errorReply(w, 404, "playback_not_ready")
		return
	}
	path := s.blobs.PlaybackPath(asset.SHA256)
	if ok, err := s.blobs.PlaybackExists(asset.SHA256); err != nil || !ok {
		// Derived data that went missing: make it again.
		log.Printf("playback: copy of %s is missing (%v); making it again", asset.SHA256, err)
		if s.playback != nil && s.vol.Check() == nil {
			if _, err := s.playback.RequeuePlayback(r.Context(), asset.SHA256, meta.PlaybackReady, meta.PlaybackFileMissing); err != nil {
				log.Printf("playback: requeue %s: %v", asset.SHA256, err)
			}
			s.kickPlayback()
		}
		errorReply(w, 404, "playback_not_ready")
		return
	}
	serveOriginal(w, r, path, "video/mp4", playbackFilename(asset.Filename))
}

// playbackFilename is the download name of a copy: the original's name
// with the extension .mp4.
func playbackFilename(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if base == "" || base == "." || base == ".." {
		return ""
	}
	return base + ".mp4"
}

// playbackDefaults fills the worker's clock and limits.
func (s *Server) playbackDefaults() {
	s.playbackNow = time.Now
	s.playbackLimit = defaultPlaybackLimit
	s.playbackGap = playbackPause
	if s.playback == nil {
		s.playback, _ = s.meta.(meta.PlaybackStore)
	}
}
