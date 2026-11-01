package meta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"strconv"
)

// Account sync state (M53, GET /api/v1/sync/state): one request tells a
// client which of its folders changed, and a digest per folder lets it
// check that its local index equals the server's view at a cursor.

// FolderDigest is the content digest of a folder for one caller, read in
// the same transaction as the feed position it belongs to.
type FolderDigest struct {
	State FeedState
	// Count is the number of assets in the digest.
	Count int
	// Digest is DigestBuilder's hex SHA-256 over the assets.
	Digest string
}

// SyncDigests computes folder digests (sqlitestore). A server without it
// answers the sync state without digests.
type SyncDigests interface {
	// FolderDigest digests the assets of folderID visible with view, with
	// the likes and comments as userID sees them.
	FolderDigest(ctx context.Context, folderID, userID string, view View) (FolderDigest, error)
}

// DigestBuilder hashes, for each asset of a folder in ascending asset ID
// order, the line
//
//	<asset_id> SP <sha256> SP <like_count> SP <liked_by_me 0|1> SP <comment_count> LF
//
// with the values of the asset JSON (docs/API_v2.yaml SyncState). An empty
// folder is the SHA-256 of nothing. Clients compute the same over their
// index; a difference means the index missed a change.
type DigestBuilder struct {
	h     hash.Hash
	count int
}

// NewDigestBuilder returns an empty builder.
func NewDigestBuilder() *DigestBuilder { return &DigestBuilder{h: sha256.New()} }

// Add appends one asset; callers add them in ascending asset ID order.
func (b *DigestBuilder) Add(assetID, sha string, likes int, likedByMe bool, comments int) {
	liked := "0"
	if likedByMe {
		liked = "1"
	}
	b.h.Write([]byte(assetID + " " + sha + " " + strconv.Itoa(likes) + " " + liked + " " + strconv.Itoa(comments) + "\n"))
	b.count++
}

// Count is the number of assets added.
func (b *DigestBuilder) Count() int { return b.count }

// Sum is the hex digest.
func (b *DigestBuilder) Sum() string { return hex.EncodeToString(b.h.Sum(nil)) }
