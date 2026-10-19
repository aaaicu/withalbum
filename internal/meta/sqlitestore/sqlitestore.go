// Package sqlitestore is the meta.MetadataStore backed by the SQLite database
// .system/withalbum.db (schema: internal/db/migrations).
//
// Writes are serialized by SQLite itself: every write method runs in one
// BEGIN IMMEDIATE transaction (internal/db opens all transactions that way),
// so there is no process-wide mutex. Reads never open a transaction, because
// a BEGIN IMMEDIATE would take the write lock; they run as plain queries,
// concurrently with writers under WAL.
//
// meta.Asset.ID and meta.Event.AssetID are the random assets.asset_id; the
// SHA-256 of the original (meta.Asset.SHA256) never leaves the server.
//
// Open makes sure a bootstrap administrator exists: folders imported from
// the PoC (and created by earlier versions) are owned by it. It is created
// locked (its password_hash is not a valid password hash, so it cannot log
// in) and is identified by account_bootstrap.user_id, not by its login name:
// `withalbum-server admin init` (internal/users) takes it over, may rename
// it, and keeps the user_id, so those folders keep their owner.
package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// DBPath returns the database location inside a storage root.
func DBPath(root string) string { return filepath.Join(root, ".system", "withalbum.db") }

const (
	// BootstrapLoginName is the login name the locked bootstrap administrator
	// is created with. After `admin init` the account may have another name;
	// find it through account_bootstrap.user_id.
	BootstrapLoginName = "admin"
	// lockedPasswordHash is deliberately not a PHC string, so no password
	// verifier can ever accept it.
	lockedPasswordHash = "!locked"
)

// Store implements meta.MetadataStore.
type Store struct {
	db      *sql.DB
	ownerID string // users.user_id of the bootstrap administrator
	epoch   string // sync_state.epoch, read at Open (a restore happens offline)
}

// DB returns the underlying database so that other stores of the same
// database (accounts, internal/users) share its connection pool.
func (s *Store) DB() *sql.DB { return s.db }

var (
	_ meta.MetadataStore  = (*Store)(nil)
	_ blobstore.BlobIndex = (*Store)(nil)
)

// Open opens (creating if missing) the database of a verified volume,
// migrates it to the latest schema and ensures the bootstrap administrator.
// It creates only .system inside the volume root.
func Open(ctx context.Context, vol *storage.Volume) (*Store, error) {
	if err := vol.Check(); err != nil {
		return nil, err
	}
	if err := storage.MkdirIn(filepath.Join(vol.Root(), ".system")); err != nil {
		return nil, err
	}
	path := DBPath(vol.Root())
	// SQLite would create the file with the default 0644 mode; metadata
	// (folder and file names) must not be world-readable. An empty file is a
	// valid empty database, and SQLite gives its -wal/-shm files the same mode.
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600); err == nil {
		f.Close()
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	sqlDB, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	s := &Store{db: sqlDB}
	if s.ownerID, err = ensureBootstrapOwner(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("bootstrap owner: %w", err)
	}
	if err := sqlDB.QueryRowContext(ctx, "SELECT epoch FROM sync_state WHERE singleton = 1").Scan(&s.epoch); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("sync epoch: %w", err)
	}
	return s, nil
}

// Close implements meta.MetadataStore.
func (s *Store) Close() error { return s.db.Close() }

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("%w: %w", meta.ErrIDGeneration, err)
	}
	return hex.EncodeToString(b), nil
}

// now is fixed width (RFC 3339 with nanoseconds), so that text order is time
// order. Rows imported from the PoC keep their variable-width RFC3339Nano.
func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }

// ensureBootstrapOwner returns the bootstrap administrator's user_id,
// inserting the account on first use.
func ensureBootstrapOwner(ctx context.Context, sqlDB *sql.DB) (string, error) {
	var id string
	err := sqlDB.QueryRowContext(ctx, bootstrapQuery).Scan(&id)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// Re-check under the write lock: another process may have inserted it.
	if id, err = EnsureBootstrapOwnerTx(ctx, tx); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

const bootstrapQuery = "SELECT user_id FROM account_bootstrap WHERE singleton = 1"

// EnsureBootstrapOwnerTx returns the bootstrap administrator's user_id inside
// tx, inserting the locked account (login BootstrapLoginName) and its
// account_bootstrap row if they do not exist yet. The caller commits. The
// legacy import uses it so that imported folders get the same owner the
// server would create. If the login name is already used by another account
// the insert fails (UNIQUE) and nothing is created.
func EnsureBootstrapOwnerTx(ctx context.Context, tx *sql.Tx) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, bootstrapQuery).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if id, err = newID(); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users
		(user_id, login_name, display_name, password_hash, server_role, can_create_albums, status, created_at)
		VALUES (?, ?, 'Administrator', ?, 'SERVER_ADMIN', 1, 'active', ?)`,
		id, BootstrapLoginName, lockedPasswordHash, now()); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO account_bootstrap (singleton, user_id) VALUES (1, ?)", id); err != nil {
		return "", err
	}
	return id, nil
}

// classify wraps SQLite errors that callers must tell apart: lock
// contention beyond busy_timeout (meta.ErrBusy), a full disk
// (storage.ErrNoSpace) and I/O errors (storage.ErrIO). Other errors are
// returned unchanged.
func classify(err error) error {
	var se *sqlite.Error
	if err == nil || !errors.As(err, &se) {
		return err
	}
	switch se.Code() & 0xff { // primary result code
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return fmt.Errorf("%w: %w", meta.ErrBusy, err)
	case sqlite3.SQLITE_FULL:
		return fmt.Errorf("%w: %w", storage.ErrNoSpace, err)
	case sqlite3.SQLITE_IOERR:
		return fmt.Errorf("%w: %w", storage.ErrIO, err)
	}
	return err
}

// CreateFolder implements meta.MetadataStore.
func (s *Store) CreateFolder(ctx context.Context, req meta.NewFolder) (meta.Folder, error) {
	f, err := s.createFolder(ctx, req)
	return f, classify(err)
}

func (s *Store) createFolder(ctx context.Context, req meta.NewFolder) (meta.Folder, error) {
	id, err := newID()
	if err != nil {
		return meta.Folder{}, err
	}
	folder := meta.Folder{ID: id, Name: req.Name, CreatedAt: now()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.Folder{}, err
	}
	defer tx.Rollback()
	if req.Authorize != nil {
		p, err := principalTx(ctx, tx, req.OwnerID, req.SessionID)
		if err != nil {
			return meta.Folder{}, err
		}
		if err := req.Authorize(p, authz.AlbumAccess{}); err != nil {
			return meta.Folder{}, fmt.Errorf("authorize: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO folders (folder_id, folder_name, owner_user_id, created_at) VALUES (?, ?, ?, ?)",
		folder.ID, folder.Name, req.OwnerID, folder.CreatedAt); err != nil {
		return meta.Folder{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO folder_sequences (folder_id, last_seq) VALUES (?, 0)", folder.ID); err != nil {
		return meta.Folder{}, err
	}
	if err := tx.Commit(); err != nil {
		return meta.Folder{}, err
	}
	return folder, nil
}

// BootstrapOwnerID returns the user_id of the bootstrap administrator, the
// owner of every folder created before accounts existed.
func (s *Store) BootstrapOwnerID() string { return s.ownerID }

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// principalTx reads the account (and, if sessionID is set, whether that
// session and its device are still live) as stored now. An unknown account
// gives the zero Principal.
func principalTx(ctx context.Context, q queryer, userID, sessionID string) (authz.Principal, error) {
	var p authz.Principal
	var role, status, hashPrefix string
	err := q.QueryRowContext(ctx, "SELECT user_id, server_role, can_create_albums, status, substr(password_hash, 1, 1) FROM users WHERE user_id = ?", userID).
		Scan(&p.UserID, &role, &p.CanCreateAlbums, &status, &hashPrefix)
	if errors.Is(err, sql.ErrNoRows) {
		return authz.Principal{}, nil
	}
	if err != nil {
		return authz.Principal{}, err
	}
	p.ServerRole = authz.ServerRole(role)
	p.Active = status == "active" && hashPrefix != password.LockedPrefix
	if p.Active && sessionID != "" {
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_sessions s JOIN devices d ON d.device_id = s.device_id
			WHERE s.session_id = ? AND d.user_id = ? AND s.revoked_at IS NULL AND d.revoked_at IS NULL)`, sessionID, userID).Scan(&p.Active); err != nil {
			return authz.Principal{}, err
		}
	}
	return p, nil
}

// folderAccessQuery selects one non-archived folder and the relation of the
// user given as the first argument. It is completed by a WHERE clause.
const folderAccessQuery = `SELECT f.folder_id, f.folder_name, f.created_at, f.owner_user_id, m.role,
		(SELECT count(*) FROM users u WHERE u.user_id = f.owner_user_id AND ` + activeUser + `) +
		(SELECT count(*) FROM folder_members mm JOIN users u ON u.user_id = mm.user_id WHERE mm.folder_id = f.folder_id AND ` + activeUser + `)
	FROM folders f LEFT JOIN folder_members m ON m.folder_id = f.folder_id AND m.user_id = ?
	WHERE f.archived_at IS NULL AND `

// activeUser is the condition on users u for an account that is active and
// can log in (the password is not a locked marker).
const activeUser = "u.status = 'active' AND substr(u.password_hash, 1, 1) <> '" + password.LockedPrefix + "'"

type scanner interface{ Scan(dest ...any) error }

func scanFolderAccess(row scanner) (meta.FolderAccess, error) {
	var fa meta.FolderAccess
	var role sql.NullString
	if err := row.Scan(&fa.Folder.ID, &fa.Folder.Name, &fa.Folder.CreatedAt, &fa.Access.OwnerID, &role, &fa.MemberCount); err != nil {
		return meta.FolderAccess{}, err
	}
	fa.Access.HasMembership = role.Valid
	fa.Access.MemberRole = authz.AlbumRole(role.String)
	return fa, nil
}

// GetFolderAccess implements meta.MetadataStore.
func (s *Store) GetFolderAccess(ctx context.Context, folderID, userID string) (meta.FolderAccess, error) {
	return getFolderAccess(ctx, s.db, folderID, userID)
}

func getFolderAccess(ctx context.Context, q queryer, folderID, userID string) (meta.FolderAccess, error) {
	fa, err := scanFolderAccess(q.QueryRowContext(ctx, folderAccessQuery+"f.folder_id = ?", userID, folderID))
	if errors.Is(err, sql.ErrNoRows) {
		return meta.FolderAccess{}, meta.ErrFolderNotFound
	}
	return fa, err
}

// ListFoldersFor implements meta.MetadataStore.
func (s *Store) ListFoldersFor(ctx context.Context, userID string) ([]meta.FolderAccess, error) {
	rows, err := s.db.QueryContext(ctx, folderAccessQuery+"(f.owner_user_id = ? OR m.user_id IS NOT NULL) ORDER BY f.created_at, f.folder_id", userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []meta.FolderAccess{}
	for rows.Next() {
		fa, err := scanFolderAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fa)
	}
	return out, rows.Err()
}

// ListFolders implements meta.MetadataStore.
func (s *Store) ListFolders(ctx context.Context) ([]meta.Folder, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT folder_id, folder_name, created_at FROM folders ORDER BY created_at, folder_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	folders := []meta.Folder{}
	for rows.Next() {
		var f meta.Folder
		if err := rows.Scan(&f.ID, &f.Name, &f.CreatedAt); err != nil {
			return nil, err
		}
		folders = append(folders, f)
	}
	return folders, rows.Err()
}

// GetFolder implements meta.MetadataStore.
func (s *Store) GetFolder(ctx context.Context, id string) (meta.Folder, error) {
	var f meta.Folder
	err := s.db.QueryRowContext(ctx, "SELECT folder_id, folder_name, created_at FROM folders WHERE folder_id = ?", id).
		Scan(&f.ID, &f.Name, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.Folder{}, meta.ErrFolderNotFound
	}
	return f, err
}

// assetColumns selects a meta.Asset (scanAsset) from assets a JOIN blobs b.
const assetColumns = "a.asset_id, a.original_sha256, a.original_filename, b.mime_type, b.bytes, a.preview_ready, a.preview_state, a.created_at, b.captured_at, " + uploaderName

// uploaderName selects the display name of the uploader of assets a while
// the account is active and can log in; NULL otherwise (no uploader,
// disabled, locked).
const uploaderName = "(SELECT u.display_name FROM users u WHERE u.user_id = a.uploaded_by_user_id AND " + activeUser + ")"

func scanAsset(row scanner) (meta.Asset, error) {
	var a meta.Asset
	var state, captured, uploader sql.NullString
	err := row.Scan(&a.ID, &a.SHA256, &a.Filename, &a.MimeType, &a.Bytes, &a.PreviewReady, &state, &a.UploadedAt, &captured, &uploader)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.Asset{}, meta.ErrAssetNotFound
	}
	a.PreviewStatus = previewStatus(a.PreviewReady, state)
	a.CapturedAt, a.UploaderName = captured.String, uploader.String
	return a, err
}

// previewStatus derives meta.Asset.PreviewStatus from assets.preview_ready
// and assets.preview_state (migration 0008: NULL means as preview_ready
// says; rows without a preview from before it count as unsupported).
func previewStatus(ready bool, state sql.NullString) string {
	switch {
	case ready:
		return meta.PreviewStatusReady
	case state.Valid:
		return state.String
	}
	return meta.PreviewStatusUnsupported
}

// previewColumns returns the preview_ready and preview_state values that
// store a committed asset's preview status. An empty status follows
// PreviewReady.
func previewColumns(a meta.Asset) (bool, any, error) {
	switch a.PreviewStatus {
	case "":
		if a.PreviewReady {
			return true, nil, nil
		}
		return false, nil, nil
	case meta.PreviewStatusReady:
		return true, nil, nil
	case meta.PreviewStatusPending, meta.PreviewStatusFailed, meta.PreviewStatusUnsupported:
		return false, a.PreviewStatus, nil
	}
	return false, nil, fmt.Errorf("sqlitestore: unknown preview status %q", a.PreviewStatus)
}

// GetAssetBySHA implements meta.MetadataStore.
func (s *Store) GetAssetBySHA(ctx context.Context, sha string) (meta.Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, "SELECT "+assetColumns+` FROM assets a JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE a.original_sha256 = ? ORDER BY a.rowid LIMIT 1`, sha))
}

// GetFolderAsset implements meta.MetadataStore.
func (s *Store) GetFolderAsset(ctx context.Context, folderID, assetID string) (meta.Asset, error) {
	return scanAsset(s.db.QueryRowContext(ctx, "SELECT "+assetColumns+` FROM folder_assets fa
		JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE fa.folder_id = ? AND fa.asset_id = ? AND fa.removed_at IS NULL`, folderID, assetID))
}

// MediaKind maps a sniffed MIME type to assets.media_kind.
func MediaKind(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	}
	return "other"
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CommitUpload implements meta.MetadataStore. Everything happens in one
// BEGIN IMMEDIATE transaction: authorization re-check, blob row (upsert),
// asset row with a random asset_id, folder link, folder sequence bump and
// the change event. Either all of it is committed or none of it.
//
// Every upload that adds bytes to a folder gets its own asset, even when the
// blob is already stored for another folder: filename, uploader and asset_id
// of one folder never show up in another.
func (s *Store) CommitUpload(ctx context.Context, req meta.CommitRequest) (meta.CommitResult, error) {
	res, err := s.commitUpload(ctx, req)
	return res, classify(err)
}

func (s *Store) commitUpload(ctx context.Context, req meta.CommitRequest) (meta.CommitResult, error) {
	in := req.Asset
	sha := in.SHA256
	newAssetID, err := newID()
	if err != nil {
		return meta.CommitResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.CommitResult{}, err
	}
	defer tx.Rollback()

	// The transaction holds the database write lock from its first
	// statement (BEGIN IMMEDIATE), and accounts, sessions and memberships
	// are written to this same database, so what is read here cannot change
	// until COMMIT: checking now is checking right before the commit.
	access, err := getFolderAccess(ctx, tx, req.FolderID, req.UserID)
	if err != nil {
		return meta.CommitResult{}, err
	}
	if req.Authorize != nil {
		p, err := principalTx(ctx, tx, req.UserID, req.SessionID)
		if err != nil {
			return meta.CommitResult{}, err
		}
		if err := req.Authorize(p, access.Access); err != nil {
			return meta.CommitResult{}, fmt.Errorf("authorize: %w", err)
		}
	}

	previewReady, previewState, err := previewColumns(in)
	if err != nil {
		return meta.CommitResult{}, err
	}
	idem := req.Idempotency
	if req.UploadSessionID != "" && idem == nil {
		return meta.CommitResult{}, errors.New("sqlitestore: an upload session commit needs its idempotency key")
	}
	// completeSession marks the upload session these bytes came from as
	// completed, in this transaction; every outcome below calls it before
	// COMMIT.
	completeSession := func(ts string) error {
		if req.UploadSessionID == "" {
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE upload_sessions SET status = 'completed', received_bytes = total_bytes, updated_at = ?, expires_at = ?
			WHERE upload_id = ? AND user_id = ? AND device_id = ? AND folder_id = ? AND status = 'open' AND expires_at > ?`,
			ts, expiry(ts), req.UploadSessionID, req.UserID, idem.DeviceID, req.FolderID, ts)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return meta.ErrUploadNotFound
		}
		return nil
	}
	if idem != nil {
		if req.UserID == "" || idem.DeviceID == "" || idem.Key == "" {
			return meta.CommitResult{}, errors.New("sqlitestore: idempotent commit needs user, device and key")
		}
		// The same request again (a retry after a lost response): answer
		// what was committed then, write nothing.
		rc, err := lookupReceipt(ctx, tx, req.UserID, req.FolderID, *idem)
		switch {
		case err == nil && rc.SHA256 != sha:
			return meta.CommitResult{}, meta.ErrIdempotencyMismatch
		case err == nil:
			a, err := assetByID(ctx, tx, rc.AssetID)
			if err != nil {
				return meta.CommitResult{}, err
			}
			if req.UploadSessionID != "" {
				if err := completeSession(now()); err != nil {
					return meta.CommitResult{}, err
				}
				if err := tx.Commit(); err != nil {
					return meta.CommitResult{}, err
				}
			}
			return meta.CommitResult{Result: rc.Result, Asset: a, Replayed: true}, nil
		case !errors.Is(err, meta.ErrReceiptNotFound):
			return meta.CommitResult{}, err
		}
	}
	ts := now()

	existing, err := scanAsset(tx.QueryRowContext(ctx, "SELECT "+assetColumns+` FROM folder_assets fa
		JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE fa.folder_id = ? AND a.original_sha256 = ? AND fa.removed_at IS NULL ORDER BY fa.rowid LIMIT 1`, req.FolderID, sha))
	if err == nil {
		if idem == nil {
			return meta.CommitResult{Result: meta.ResultAlreadyPresent, Asset: existing}, nil
		}
		// Nothing changes in the folder, but the receipt is written (and
		// synced) like for a new asset.
		if err := insertReceipt(ctx, tx, req, meta.ResultAlreadyPresent, existing.ID, ts); err != nil {
			return meta.CommitResult{}, err
		}
		if err := completeSession(ts); err != nil {
			return meta.CommitResult{}, err
		}
		if req.BeforeCommit != nil {
			if err := req.BeforeCommit(); err != nil {
				return meta.CommitResult{}, fmt.Errorf("before commit: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return meta.CommitResult{}, err
		}
		return meta.CommitResult{Result: meta.ResultAlreadyPresent, Asset: existing}, nil
	}
	if !errors.Is(err, meta.ErrAssetNotFound) {
		return meta.CommitResult{}, err
	}

	result := meta.ResultLinked
	ins, err := tx.ExecContext(ctx, `INSERT INTO blobs (sha256, bytes, mime_type, relative_path, verified_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(sha256) DO NOTHING`,
		sha, in.Bytes, in.MimeType, blobstore.PoolRelPath(sha), ts, ts)
	if err != nil {
		return meta.CommitResult{}, fmt.Errorf("insert blob: %w", err)
	}
	if n, err := ins.RowsAffected(); err != nil {
		return meta.CommitResult{}, err
	} else if n == 1 {
		result = meta.ResultCreated
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO assets (asset_id, original_sha256, original_filename, media_kind, preview_ready, preview_state, uploaded_by_user_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		newAssetID, sha, in.Filename, MediaKind(in.MimeType), previewReady, previewState, nullable(req.UserID), ts); err != nil {
		return meta.CommitResult{}, fmt.Errorf("insert asset: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO folder_assets (folder_id, asset_id, added_by_user_id, added_at, uploader_device_label)
		VALUES (?, ?, ?, ?, ?)`,
		req.FolderID, newAssetID, nullable(req.UserID), ts, req.DeviceID); err != nil {
		return meta.CommitResult{}, fmt.Errorf("link asset: %w", err)
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO folder_sequences (folder_id, last_seq) VALUES (?, 1)
		ON CONFLICT(folder_id) DO UPDATE SET last_seq = last_seq + 1 RETURNING last_seq`, req.FolderID).Scan(&seq); err != nil {
		return meta.CommitResult{}, fmt.Errorf("next seq: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
		VALUES (?, ?, ?, ?, ?, ?)`,
		req.FolderID, seq, meta.EventAssetAdded, newAssetID, ts, req.DeviceID); err != nil {
		return meta.CommitResult{}, fmt.Errorf("insert change: %w", err)
	}
	if idem != nil {
		// ResultLinked is stored as created: the API never tells the two apart.
		if err := insertReceipt(ctx, tx, req, meta.ResultCreated, newAssetID, ts); err != nil {
			return meta.CommitResult{}, err
		}
	}
	if err := completeSession(ts); err != nil {
		return meta.CommitResult{}, err
	}
	// Re-read so the result reflects the stored rows (an already present
	// blob row keeps its own size and type).
	asset, err := assetByID(ctx, tx, newAssetID)
	if err != nil {
		return meta.CommitResult{}, err
	}
	if req.BeforeCommit != nil {
		if err := req.BeforeCommit(); err != nil {
			return meta.CommitResult{}, fmt.Errorf("before commit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return meta.CommitResult{}, err
	}
	return meta.CommitResult{Result: result, Asset: asset}, nil
}

// ListEvents implements meta.MetadataStore. It runs plain queries, not a
// transaction. The feed state is read first; events are append-only and
// each page is one statement, so a concurrent commit or removal can only add
// events after the page, which HasMore then reports.
func (s *Store) ListEvents(ctx context.Context, folderID string, cursor int64, limit int) (meta.EventPage, error) {
	if limit <= 0 {
		return meta.EventPage{}, meta.ErrInvalidLimit
	}
	state, err := s.FeedState(ctx, folderID)
	if err != nil {
		return meta.EventPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.seq, c.event_type, c.asset_id, COALESCE(c.uploader_device_label, ''), c.occurred_at,
			a.original_sha256, a.original_filename, b.mime_type, b.bytes, a.preview_ready, a.preview_state, a.created_at,
			b.captured_at, `+uploaderName+`, fa.asset_id IS NOT NULL AND fa.removed_at IS NULL
		FROM folder_changes c
		LEFT JOIN assets a ON a.asset_id = c.asset_id
		LEFT JOIN blobs b ON b.sha256 = a.original_sha256
		LEFT JOIN folder_assets fa ON fa.folder_id = c.folder_id AND fa.asset_id = c.asset_id
		WHERE c.folder_id = ? AND c.seq > ? ORDER BY c.seq LIMIT ?`, folderID, cursor, limit)
	if err != nil {
		return meta.EventPage{}, err
	}
	defer rows.Close()
	page := meta.EventPage{Events: []meta.Event{}, Assets: map[string]meta.Asset{}, NextCursor: cursor, State: state}
	for rows.Next() {
		e := meta.Event{FolderID: folderID}
		var sha, filename, mime, uploadedAt, state, captured, uploader sql.NullString
		var size sql.NullInt64
		var preview sql.NullBool
		var linked bool
		if err := rows.Scan(&e.Seq, &e.Kind, &e.AssetID, &e.UploaderDeviceID, &e.OccurredAt,
			&sha, &filename, &mime, &size, &preview, &state, &uploadedAt, &captured, &uploader, &linked); err != nil {
			return meta.EventPage{}, err
		}
		if !sha.Valid || !mime.Valid {
			// folder_changes.asset_id has no foreign key; never skip an
			// event silently, since clients would see a gap in seq.
			return meta.EventPage{}, fmt.Errorf("folder %s event %d references a missing asset or blob", folderID, e.Seq)
		}
		page.Events = append(page.Events, e)
		if (e.Kind == meta.EventAssetAdded || e.Kind == meta.EventAssetUpdated) && linked {
			page.Assets[e.AssetID] = meta.Asset{ID: e.AssetID, SHA256: sha.String, Filename: filename.String, MimeType: mime.String,
				Bytes: size.Int64, PreviewReady: preview.Bool, PreviewStatus: previewStatus(preview.Bool, state), UploadedAt: uploadedAt.String,
				CapturedAt: captured.String, UploaderName: uploader.String}
		}
		page.NextCursor = e.Seq
	}
	if err := rows.Err(); err != nil {
		return meta.EventPage{}, err
	}
	rows.Close()
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM folder_changes WHERE folder_id = ? AND seq > ?)",
		folderID, page.NextCursor).Scan(&page.HasMore); err != nil {
		return meta.EventPage{}, err
	}
	return page, nil
}

// ForEachBlob implements blobstore.BlobIndex: it calls fn for every blobs
// row in ascending sha256 order. It is a plain query (no transaction).
func (s *Store) ForEachBlob(ctx context.Context, fn func(sha string, size int64) error) error {
	rows, err := s.db.QueryContext(ctx, "SELECT sha256, bytes FROM blobs ORDER BY sha256")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		var size int64
		if err := rows.Scan(&sha, &size); err != nil {
			return err
		}
		if err := fn(sha, size); err != nil {
			return err
		}
	}
	return rows.Err()
}

// HasLegacyImport reports whether a state.json whose bytes hash to sourceSHA256
// was already imported into this database (a legacy_imports row exists).
func (s *Store) HasLegacyImport(ctx context.Context, sourceSHA256 string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM legacy_imports WHERE source_sha256 = ?)", sourceSHA256).Scan(&ok)
	return ok, err
}
