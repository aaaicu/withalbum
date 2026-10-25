package meta

import (
	"context"
	"errors"
)

// Likes, comments and mentions on the assets of a folder (migration 0014).
//
// Everything is per folder and asset: an asset linked to several folders
// has a like set and a thread in each. People are shown only while they
// can read the folder: a liker, comment author or mentioned account that
// is no longer the owner, a CONTRIBUTOR or a VIEWER, or whose account
// cannot log in, is left out of like lists and counts, shown as no author
// (AuthorName ""), or as a plain-text mention (Mention.Reader false).
// Who may write is decided by the caller through the Authorizer of every
// write, as for the other album writes.

// Social limits enforced by the store as well as by the HTTP API.
const (
	// MaxCommentBytes bounds the stored source text of a comment
	// (mention tokens included).
	MaxCommentBytes = 16 << 10
	// MaxMentions bounds the accounts one comment mentions.
	MaxMentions = 20
)

// Social errors.
var (
	// ErrCommentNotFound: no comment with this ID on this asset of this
	// folder.
	ErrCommentNotFound = errors.New("comment not found")
	// ErrMentionNotReader: a mentioned account cannot read the folder (or
	// cannot log in). Nothing was written.
	ErrMentionNotReader = errors.New("mentioned account cannot read the folder")
)

// SocialSummary is what an asset shows of its likes and comments to one
// reader.
type SocialSummary struct {
	LikeCount    int  // likes of current readers
	LikedByMe    bool // the reader's own like
	CommentCount int  // every comment of the asset in this folder
}

// Like is one current reader's like.
type Like struct {
	UserID      string
	DisplayName string
	LikedAt     string
}

// Mention is an account a comment mentions.
type Mention struct {
	UserID string
	// WrittenName is the display name when the comment was written.
	WrittenName string
	// Reader: the account can read the folder now and can log in; then
	// Name is its current display name, else "".
	Reader bool
	Name   string
}

// Comment is one comment as stored, with names resolved now.
type Comment struct {
	ID       string
	FolderID string
	AssetID  string
	AuthorID string
	// AuthorName is the author's display name, "" when the author cannot
	// read the folder now or cannot log in.
	AuthorName string
	// Body is the source text: mentions are "<@user_id>" tokens of
	// Mentions.
	Body      string
	Mentions  []Mention
	CreatedAt string
	EditedAt  string // "" if never edited
}

// SocialCursor is a position in a list ordered by (At, ID).
type SocialCursor struct {
	At string
	ID string
}

// CommentPage is one page of a thread, oldest first.
type CommentPage struct {
	Comments []Comment // never nil
	HasMore  bool
	Next     SocialCursor // position of the last comment, zero if empty
}

// Activity kinds.
const (
	ActivityMention = "mention" // a comment mentioning the reader
	ActivityComment = "comment" // a comment on the reader's upload
	ActivityLike    = "like"    // a like on the reader's upload
)

// Activity is one item of a reader's activity.
type Activity struct {
	ID         string // opaque, unique
	Kind       string // Activity*
	FolderID   string
	FolderName string
	AssetID    string
	ActorID    string
	// ActorName: as Comment.AuthorName (always set for likes).
	ActorName string
	At        string
	Comment   *Comment // ActivityMention and ActivityComment
}

// ActivityPage is one page of activity, newest first.
type ActivityPage struct {
	Items   []Activity // never nil
	HasMore bool
	Next    SocialCursor
}

// CommentChange is a comment write. AlbumChange.TargetID is the asset;
// Authorize re-checks the caller on the folder inside the transaction.
type CommentChange struct {
	AlbumChange
	CommentID string   // edit, delete
	Body      string   // create, edit: validated source text
	Mentions  []string // create, edit: the accounts of the body's tokens
	// Key is the author's Idempotency-Key (create; "" for none). A key
	// already used by the author on the same asset of the same folder
	// returns that comment (replayed) if it still exists; on another asset
	// ErrIdempotencyMismatch.
	Key string
	// CheckMentions, if set, is called inside the transaction with the
	// current display names of Mentions (every one a reader, else
	// ErrMentionNotReader first); its error aborts the write.
	CheckMentions func(names map[string]string) error
	// AuthorizeComment re-checks the caller against the stored comment's
	// author inside the transaction (edit, delete; required there).
	AuthorizeComment func(authorID string) error
}

// Social stores likes and comments. Every write is one transaction that
// re-checks the caller, requires the asset to be in the folder
// (ErrAssetNotFound otherwise), appends its change event when something
// changed and writes an audit row (never the comment text).
type Social interface {
	// SetLike adds (liked) or removes c.ActorID's like on asset c.TargetID
	// and returns the caller's summary afterwards. Setting the current
	// state changes nothing (changed false, no event).
	SetLike(ctx context.Context, c AlbumChange, liked bool) (sum SocialSummary, changed bool, err error)
	// ListLikes lists the current readers' likes on the asset, oldest
	// first. ErrAssetNotFound.
	ListLikes(ctx context.Context, folderID, assetID string) ([]Like, error)
	// SocialSummaries returns the summary of each given asset of the folder
	// for userID (assets without likes or comments are absent).
	SocialSummaries(ctx context.Context, folderID, userID string, assetIDs []string) (map[string]SocialSummary, error)
	// CreateComment stores a new comment and its mentions
	// (EventCommentAdded).
	CreateComment(ctx context.Context, c CommentChange) (cm Comment, replayed bool, err error)
	// EditComment replaces the body and mentions of c.CommentID
	// (EventCommentUpdated, edited_at), or changes nothing if they are the
	// same. ErrCommentNotFound.
	EditComment(ctx context.Context, c CommentChange) (Comment, error)
	// DeleteComment deletes c.CommentID and its mentions
	// (EventCommentRemoved). ErrCommentNotFound.
	DeleteComment(ctx context.Context, c CommentChange) error
	// ListComments returns up to limit comments of the asset after the
	// cursor, oldest first. ErrAssetNotFound, ErrInvalidLimit.
	ListComments(ctx context.Context, folderID, assetID string, after SocialCursor, limit int) (CommentPage, error)
	// Mentionable lists the folder's current readers whose accounts can
	// log in: the owner first, then members by join time.
	Mentionable(ctx context.Context, folderID string) ([]Member, error)
	// ListActivity returns, newest first and before the cursor (zero: from
	// the newest), the comments mentioning userID, the comments and likes
	// of others on assets userID uploaded, in the folders userID can read
	// now (not archived) and on assets still in them. A comment that both
	// mentions userID and is on its upload is one ActivityMention.
	ListActivity(ctx context.Context, userID string, before SocialCursor, limit int) (ActivityPage, error)
}
