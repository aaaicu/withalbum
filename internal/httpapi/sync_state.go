package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Account sync state (M53): GET /api/v1/sync/state tells a client in one
// request where the feed of every folder it can read stands, so it pulls
// only the folders whose cursor moved instead of asking each folder.
//
//   - "folders": per readable folder its sync cursor (the caller's view,
//     the same as GET .../sync's next_cursor at the end of the feed) and,
//     with digests=1, the content digest at that cursor (meta.DigestBuilder)
//     and the asset count. An UPLOADER's folders are not listed.
//   - "albums_tag": changes whenever GET /folders would answer differently
//     (a new or lost album, a name, role or setting).
//   - "tag" (also the ETag): changes whenever a cursor or albums_tag does.
//
// With If-None-Match equal to the current tag the answer is 304. With
// wait=N (1..maxSyncWait seconds) and an unchanged tag the request waits
// up to N seconds for a change (a long poll, checked every
// syncWaitPoll) before 304. At most maxSyncWaiters requests wait at once;
// beyond that the request answers at once as without wait.

const (
	maxSyncWait    = 25
	maxSyncWaiters = 64
	syncWaitPoll   = time.Second
)

// syncWaiters counts the long polls in progress (all servers of the
// process share it; there is one).
var syncWaiters atomic.Int32

type syncFolderJSON struct {
	FolderID string  `json:"folder_id"`
	Cursor   string  `json:"cursor"`
	Digest   *string `json:"digest,omitempty"`
	Count    *int    `json:"count,omitempty"`
}

type syncStateJSON struct {
	Tag       string           `json:"tag"`
	AlbumsTag string           `json:"albums_tag"`
	Folders   []syncFolderJSON `json:"folders"`
}

func (s *Server) syncStateRoutes(rt *router) {
	rt.handle("GET /api/v1/sync/state", s.syncState)
}

func (s *Server) syncState(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	q := r.URL.Query()
	digests := false
	switch q.Get("digests") {
	case "", "0":
	case "1":
		digests = true
	default:
		errorReply(w, 400, "invalid_digests")
		return
	}
	wait := 0
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxSyncWait || strings.TrimLeft(v, "0123456789") != "" {
			errorReply(w, 400, "invalid_wait")
			return
		}
		wait = n
	}
	prev := etagValue(r.Header.Get("If-None-Match"))
	st, err := s.buildSyncState(r.Context(), p, false)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	if prev != "" && st.Tag == prev && wait > 0 {
		if syncWaiters.Add(1) <= maxSyncWaiters {
			st, err = s.waitSyncState(r.Context(), p, prev, time.Duration(wait)*time.Second)
			syncWaiters.Add(-1)
			if err != nil {
				if r.Context().Err() == nil {
					metaReadFailed(w, err)
				}
				return
			}
		} else {
			syncWaiters.Add(-1)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	if prev != "" && st.Tag == prev {
		w.Header().Set("ETag", `"`+st.Tag+`"`)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if digests {
		if st, err = s.buildSyncState(r.Context(), p, true); err != nil {
			metaReadFailed(w, err)
			return
		}
	}
	w.Header().Set("ETag", `"`+st.Tag+`"`)
	jsonReply(w, 200, st)
}

// waitSyncState re-reads the state every syncWaitPoll until its tag is not
// prev or d passed.
func (s *Server) waitSyncState(ctx context.Context, p authz.Principal, prev string, d time.Duration) (syncStateJSON, error) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(syncWaitPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return syncStateJSON{}, ctx.Err()
		case <-deadline.C:
			return s.buildSyncState(ctx, p, false)
		case <-tick.C:
			st, err := s.buildSyncState(ctx, p, false)
			if err != nil || st.Tag != prev {
				return st, err
			}
		}
	}
}

// buildSyncState reads the caller's state; with digests each readable
// folder's cursor is the one its digest was read at.
func (s *Server) buildSyncState(ctx context.Context, p authz.Principal, digests bool) (syncStateJSON, error) {
	list, err := s.meta.ListFoldersFor(ctx, p.UserID)
	if err != nil {
		return syncStateJSON{}, err
	}
	var dg meta.SyncDigests
	if digests {
		dg, _ = s.meta.(meta.SyncDigests)
	}
	albums := sha256.New()
	out := syncStateJSON{Folders: []syncFolderJSON{}}
	for _, fa := range list {
		if !authz.Allowed(p, authz.GetAlbumMetadata, &fa.Access) {
			continue
		}
		b, err := json.Marshal(folderView(p, fa))
		if err != nil {
			return syncStateJSON{}, err
		}
		albums.Write(b)
		albums.Write([]byte{'\n'})
		if !authz.Allowed(p, authz.ListAlbumAssets, &fa.Access) {
			continue
		}
		id := fa.Folder.ID
		view := meta.ViewOf(p, fa.Access)
		f := syncFolderJSON{FolderID: id}
		if dg != nil {
			d, err := dg.FolderDigest(ctx, id, p.UserID, view)
			if errors.Is(err, meta.ErrFolderNotFound) {
				continue
			}
			if err != nil {
				return syncStateJSON{}, err
			}
			f.Cursor = encodeCursor(d.State.Epoch, d.State.LastSeq)
			f.Digest, f.Count = &d.Digest, &d.Count
		} else {
			fs, err := s.meta.FeedState(ctx, id, view)
			if errors.Is(err, meta.ErrFolderNotFound) {
				continue
			}
			if err != nil {
				return syncStateJSON{}, err
			}
			f.Cursor = encodeCursor(fs.Epoch, fs.LastSeq)
		}
		out.Folders = append(out.Folders, f)
	}
	out.AlbumsTag = hex.EncodeToString(albums.Sum(nil))[:32]
	tag := sha256.New()
	tag.Write([]byte(out.AlbumsTag))
	for _, f := range out.Folders {
		tag.Write([]byte("\n" + f.FolderID + " " + f.Cursor))
	}
	out.Tag = hex.EncodeToString(tag.Sum(nil))[:32]
	return out, nil
}

// etagValue is the tag of an If-None-Match header ("" when absent or a
// list): W/ and the quotes are dropped.
func etagValue(h string) string {
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "W/"))
	if len(h) < 2 || h[0] != '"' || h[len(h)-1] != '"' || strings.Contains(h, ",") {
		return ""
	}
	return h[1 : len(h)-1]
}
