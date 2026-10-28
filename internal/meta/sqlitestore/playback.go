package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aaaicu/withalbum/internal/meta"
)

// Playback copies (migration 0016, meta.PlaybackStore). Rows are queued by
// triggers when a video asset is linked to a folder; the worker settles
// them here.

// heldByVideo is true when a video asset in use holds blob_playback p.
const heldByVideo = `EXISTS (SELECT 1 FROM assets a JOIN folder_assets fa ON fa.asset_id = a.asset_id
	WHERE a.original_sha256 = p.sha256 AND a.media_kind = 'video' AND fa.removed_at IS NULL)`

// maxPlaybackSide bounds the stored size of a copy (the column CHECK).
const maxPlaybackSide = 8192

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// shownPlayback is the status a client sees for a stored status.
func shownPlayback(status string) string {
	if status == meta.PlaybackIdle {
		return meta.PlaybackPending
	}
	return status
}

// setPlayback fills a.Playback from the blobPlayback column.
func setPlayback(a *meta.Asset, col sql.NullString) error {
	if !col.Valid {
		return nil
	}
	var p struct {
		S    string
		B    *int64
		W, H *int
	}
	if err := json.Unmarshal([]byte(col.String), &p); err != nil {
		return fmt.Errorf("asset %s: playback: %w", a.ID, err)
	}
	a.Playback = &meta.Playback{Status: shownPlayback(p.S)}
	if p.S == meta.PlaybackReady && p.B != nil {
		a.Playback.Bytes = *p.B
		if p.W != nil && p.H != nil {
			a.Playback.Width, a.Playback.Height = *p.W, *p.H
		}
	}
	return nil
}

// PendingPlayback implements meta.PlaybackStore.
func (s *Store) PendingPlayback(ctx context.Context, now time.Time, limit int) ([]meta.PlaybackJob, error) {
	if limit <= 0 {
		return nil, meta.ErrInvalidLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.sha256, b.mime_type, b.bytes, COALESCE(b.duration_ms, 0), p.attempts
		FROM blob_playback p JOIN blobs b ON b.sha256 = p.sha256
		WHERE p.status = 'pending' AND (p.next_attempt_at IS NULL OR p.next_attempt_at <= ?) AND `+heldByVideo+`
		ORDER BY p.queued_at DESC, p.sha256 LIMIT ?`, stamp(now), limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	out := []meta.PlaybackJob{}
	for rows.Next() {
		var j meta.PlaybackJob
		if err := rows.Scan(&j.SHA256, &j.MimeType, &j.Bytes, &j.DurationMS, &j.Attempts); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, classify(rows.Err())
}

// validPlaybackResult rejects what the worker never writes.
func validPlaybackResult(r meta.PlaybackResult) error {
	if len(r.SHA256) != 64 || len(r.Reason) > 32 || r.Attempts < 0 {
		return errors.New("sqlitestore: an invalid playback result")
	}
	switch r.Status {
	case meta.PlaybackReady:
		if r.Bytes <= 0 || r.Width <= 0 || r.Height <= 0 || r.Width > maxPlaybackSide || r.Height > maxPlaybackSide {
			return errors.New("sqlitestore: a ready playback copy without its size")
		}
	case meta.PlaybackFailed, meta.PlaybackUnsupported:
		if r.Reason == "" {
			return errors.New("sqlitestore: a playback result without its reason")
		}
	case meta.PlaybackPending:
		if r.RetryAt.IsZero() {
			return errors.New("sqlitestore: a playback retry without its time")
		}
	default:
		return fmt.Errorf("sqlitestore: unknown playback status %q", r.Status)
	}
	return nil
}

// SetPlayback implements meta.PlaybackStore.
func (s *Store) SetPlayback(ctx context.Context, r meta.PlaybackResult) (int, error) {
	if err := validPlaybackResult(r); err != nil {
		return 0, err
	}
	n, err := s.setPlaybackResult(ctx, r)
	return n, classify(err)
}

func (s *Store) setPlaybackResult(ctx context.Context, r meta.PlaybackResult) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, "SELECT status FROM blob_playback WHERE sha256 = ?", r.SHA256).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || err == nil && status != meta.PlaybackPending {
		return 0, nil // released or settled meanwhile
	}
	if err != nil {
		return 0, err
	}
	ts := now()
	switch r.Status {
	case meta.PlaybackReady:
		_, err = tx.ExecContext(ctx, `UPDATE blob_playback SET status = 'ready', reason = NULL, next_attempt_at = NULL, bytes = ?, width = ?, height = ?, updated_at = ?
			WHERE sha256 = ?`, r.Bytes, r.Width, r.Height, ts, r.SHA256)
	case meta.PlaybackPending:
		_, err = tx.ExecContext(ctx, "UPDATE blob_playback SET reason = ?, attempts = ?, next_attempt_at = ?, updated_at = ? WHERE sha256 = ?",
			nullable(r.Reason), r.Attempts, stamp(r.RetryAt), ts, r.SHA256)
	default:
		_, err = tx.ExecContext(ctx, "UPDATE blob_playback SET status = ?, reason = ?, attempts = ?, next_attempt_at = NULL, updated_at = ? WHERE sha256 = ?",
			r.Status, r.Reason, r.Attempts, ts, r.SHA256)
	}
	if err != nil {
		return 0, err
	}
	events := 0
	if r.Status != meta.PlaybackPending {
		if events, err = playbackEvents(ctx, tx, r.SHA256, ts); err != nil {
			return 0, err
		}
	}
	return events, tx.Commit()
}

// playbackEvents appends asset_updated to every folder holding a video
// asset in use of sha, with the uploader label of the link (like its
// asset_added), and returns how many.
func playbackEvents(ctx context.Context, tx *sql.Tx, sha, ts string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT fa.folder_id, fa.asset_id, fa.uploader_device_label FROM assets a JOIN folder_assets fa ON fa.asset_id = a.asset_id
		WHERE fa.removed_at IS NULL AND a.original_sha256 = ? AND a.media_kind = 'video' ORDER BY fa.folder_id, fa.asset_id`, sha)
	if err != nil {
		return 0, err
	}
	type link struct {
		folder, asset string
		device        sql.NullString
	}
	var links []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.folder, &l.asset, &l.device); err != nil {
			rows.Close()
			return 0, err
		}
		links = append(links, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, l := range links {
		seq, err := nextSeq(ctx, tx, l.folder)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
			VALUES (?, ?, ?, ?, ?, ?)`, l.folder, seq, meta.EventAssetUpdated, l.asset, ts, l.device); err != nil {
			return 0, err
		}
	}
	return len(links), nil
}

// RequeuePlayback implements meta.PlaybackStore.
func (s *Store) RequeuePlayback(ctx context.Context, sha, from, reason string) (bool, error) {
	ok, err := s.requeue(ctx, []string{sha}, from, reason)
	return ok > 0, classify(err)
}

// RequeueUnsupported implements meta.PlaybackStore.
func (s *Store) RequeueUnsupported(ctx context.Context, reason string) (int, error) {
	// A read first: most starts have nothing to requeue and must not take
	// the write lock for it.
	rows, err := s.db.QueryContext(ctx, "SELECT sha256 FROM blob_playback WHERE status = 'unsupported' AND reason = ? ORDER BY sha256", reason)
	if err != nil {
		return 0, classify(err)
	}
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return 0, err
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(shas) == 0 {
		return 0, classify(err)
	}
	n, err := s.requeue(ctx, shas, meta.PlaybackUnsupported, "")
	return n, classify(err)
}

func (s *Store) requeue(ctx context.Context, shas []string, from, reason string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ts := now()
	n := 0
	for _, sha := range shas {
		res, err := tx.ExecContext(ctx, `UPDATE blob_playback SET status = 'pending', reason = ?, attempts = 0, next_attempt_at = NULL, bytes = NULL, width = NULL, height = NULL,
			queued_at = ?, updated_at = ? WHERE sha256 = ? AND status = ?`, nullable(reason), ts, ts, sha, from)
		if err != nil {
			return 0, err
		}
		changed, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed == 0 {
			continue
		}
		n++
		if shownPlayback(from) != meta.PlaybackPending {
			if _, err := playbackEvents(ctx, tx, sha, ts); err != nil {
				return 0, err
			}
		}
	}
	return n, tx.Commit()
}

// UnreferencedPlayback implements meta.PlaybackStore.
func (s *Store) UnreferencedPlayback(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, meta.ErrInvalidLimit
	}
	return s.playbackList(ctx, "SELECT p.sha256 FROM blob_playback p WHERE p.status <> 'idle' AND NOT "+heldByVideo+" ORDER BY p.sha256 LIMIT ?", limit)
}

// ReadyPlayback implements meta.PlaybackStore.
func (s *Store) ReadyPlayback(ctx context.Context, after string, limit int) ([]meta.PlaybackJob, error) {
	if limit <= 0 {
		return nil, meta.ErrInvalidLimit
	}
	rows, err := s.db.QueryContext(ctx, "SELECT sha256, bytes FROM blob_playback WHERE status = 'ready' AND sha256 > ? ORDER BY sha256 LIMIT ?", after, limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	out := []meta.PlaybackJob{}
	for rows.Next() {
		var j meta.PlaybackJob
		if err := rows.Scan(&j.SHA256, &j.Bytes); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, classify(rows.Err())
}

func (s *Store) playbackList(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, classify(rows.Err())
}

// ReleasePlayback implements meta.PlaybackStore.
func (s *Store) ReleasePlayback(ctx context.Context, sha string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, classify(err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE blob_playback AS p SET status = 'idle', reason = NULL, attempts = 0, next_attempt_at = NULL, bytes = NULL, width = NULL, height = NULL,
		updated_at = ? WHERE p.sha256 = ? AND p.status <> 'idle' AND NOT `+heldByVideo, now(), sha)
	if err != nil {
		return false, classify(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, classify(tx.Commit())
}

// PlaybackStatus implements meta.PlaybackStore.
func (s *Store) PlaybackStatus(ctx context.Context, sha string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, "SELECT status FROM blob_playback WHERE sha256 = ?", sha).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return status, classify(err)
}

// PlaybackStats implements meta.PlaybackStore.
func (s *Store) PlaybackStats(ctx context.Context) (meta.PlaybackStats, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT status, count(*), COALESCE(SUM(bytes), 0) FROM blob_playback GROUP BY status")
	if err != nil {
		return meta.PlaybackStats{}, classify(err)
	}
	defer rows.Close()
	var st meta.PlaybackStats
	for rows.Next() {
		var status string
		var n, b int64
		if err := rows.Scan(&status, &n, &b); err != nil {
			return meta.PlaybackStats{}, err
		}
		switch status {
		case meta.PlaybackPending:
			st.Pending += n
		case meta.PlaybackReady:
			st.Ready, st.ReadyBytes = n, b
		case meta.PlaybackFailed:
			st.Failed = n
		case meta.PlaybackUnsupported:
			st.Unsupported = n
		}
	}
	return st, classify(rows.Err())
}
