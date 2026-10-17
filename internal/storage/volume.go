// Package storage guards the storage root so that the server only writes to a
// directory that was explicitly registered with Init.
//
// The main risk it addresses: when an external disk is unmounted, its mount
// path (for example /Volumes/ExternalSSD/WithAlbum) may disappear or be
// recreated on the internal disk. A server that creates its root on demand
// would then silently write photos to the wrong disk. Open refuses roots
// without a marker, and Check re-verifies the marker and the device number
// immediately before writes.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MarkerName is the file in the storage root that identifies an initialized volume.
const MarkerName = ".withalbum-volume.json"

const markerFormat = 1

var (
	ErrRootMissing        = errors.New("storage root does not exist")
	ErrNotDirectory       = errors.New("storage root is not a directory")
	ErrNotInitialized     = errors.New("storage root is not initialized")
	ErrAlreadyInitialized = errors.New("storage root is already initialized")
	ErrInvalidMarker      = errors.New("storage marker is invalid")
	ErrVolumeMismatch     = errors.New("storage volume id does not match")
	ErrNotWritable        = errors.New("storage root is not writable")
	ErrVolumeChanged      = errors.New("storage volume changed since startup")
	// ErrRestorePending means RestoreJournalName exists in the root: a
	// restore was interrupted between swapping directories. Only the restore
	// command may open the volume until it has finished.
	ErrRestorePending = errors.New("an interrupted restore must be finished first")
)

// RestoreJournalName is written into the storage root by the restore command
// (internal/backup) right before it swaps .system, and removed afterwards.
const RestoreJournalName = ".withalbum-restore.json"

// BackupMarkerName identifies an initialized backup target directory
// (internal/backup). It differs from MarkerName so that a backup disk can
// never be opened as a storage root, and vice versa.
const BackupMarkerName = ".withalbum-backup-target.json"

// Marker is the content of MarkerName.
type Marker struct {
	Format    int    `json:"format"`
	VolumeID  string `json:"volume_id"`
	CreatedAt string `json:"created_at"`
}

// OpenOptions configures Open.
type OpenOptions struct {
	// ExpectedVolumeID, when non-empty, pins the volume (WITHALBUM_VOLUME_ID).
	ExpectedVolumeID string
	// AllowPendingRestore opens a root that holds RestoreJournalName. Only
	// the restore command sets it.
	AllowPendingRestore bool
}

// Volume is a verified storage root. It is safe for concurrent use.
type Volume struct {
	root    string // symlink-resolved absolute path
	id      string
	dev     uint64
	hasDev  bool
	marker  string // MarkerName or BackupMarkerName
	lockDir string // directory of the instance lock, relative to root
}

// Root returns the symlink-resolved storage root.
func (v *Volume) Root() string { return v.root }

// ID returns the volume id recorded in the marker.
func (v *Volume) ID() string { return v.id }

// Init writes a new marker into an existing directory. It never creates the
// root itself and never touches existing data other than adding the marker.
func Init(root string) (Marker, error) { return initMarker(root, MarkerName) }

// InitBackupTarget registers an existing directory as a backup target by
// writing BackupMarkerName into it. Like Init it never creates the directory.
func InitBackupTarget(dir string) (Marker, error) { return initMarker(dir, BackupMarkerName) }

func initMarker(root, name string) (Marker, error) {
	resolved, err := resolveDir(root)
	if err != nil {
		return Marker{}, err
	}
	if _, err := os.Lstat(filepath.Join(resolved, otherMarker(name))); err == nil {
		return Marker{}, fmt.Errorf("%w: the directory already holds %s", ErrAlreadyInitialized, otherMarker(name))
	}
	markerPath := filepath.Join(resolved, name)
	if _, err := os.Lstat(markerPath); err == nil {
		return Marker{}, ErrAlreadyInitialized
	} else if !errors.Is(err, os.ErrNotExist) {
		return Marker{}, err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return Marker{}, err
	}
	m := Marker{Format: markerFormat, VolumeID: hex.EncodeToString(b), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Marker{}, err
	}
	data = append(data, '\n')
	// os.CreateTemp creates the file with mode 0600.
	tmp, err := os.CreateTemp(resolved, ".withalbum-volume-*.tmp")
	if err != nil {
		return Marker{}, fmt.Errorf("%w: %w", ErrNotWritable, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return Marker{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Marker{}, err
	}
	if err := tmp.Close(); err != nil {
		return Marker{}, err
	}
	if err := os.Rename(tmp.Name(), markerPath); err != nil {
		return Marker{}, err
	}
	if err := syncDir(resolved); err != nil {
		return Marker{}, err
	}
	return m, nil
}

// Open verifies that root is an initialized, writable storage root and
// records its device number for later Check calls. It creates nothing in root
// except a short-lived probe file.
//
// A root holding RestoreJournalName is refused with ErrRestorePending unless
// opt.AllowPendingRestore is set.
func Open(root string, opt OpenOptions) (*Volume, error) {
	v, err := open(root, MarkerName, ".system", opt.ExpectedVolumeID, true)
	if err != nil {
		return nil, err
	}
	if !opt.AllowPendingRestore {
		if _, err := os.Lstat(filepath.Join(v.root, RestoreJournalName)); err == nil {
			return nil, ErrRestorePending
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return v, nil
}

// OpenBackupTarget verifies a directory registered with InitBackupTarget.
// writable=false skips the write probe, for reading a backup from a
// read-only disk. Its instance lock (Lock) lives in the directory itself.
func OpenBackupTarget(dir string, writable bool) (*Volume, error) {
	return open(dir, BackupMarkerName, ".", "", writable)
}

func open(root, marker, lockDir, expectedID string, writable bool) (*Volume, error) {
	resolved, err := resolveDir(root)
	if err != nil {
		return nil, err
	}
	m, err := readMarker(resolved, marker)
	if err != nil {
		return nil, err
	}
	if want := strings.ToLower(strings.TrimSpace(expectedID)); want != "" && want != m.VolumeID {
		return nil, ErrVolumeMismatch
	}
	if writable {
		if err := probeWritable(resolved); err != nil {
			return nil, err
		}
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	v := &Volume{root: resolved, id: m.VolumeID, marker: marker, lockDir: lockDir}
	v.dev, v.hasDev = devOf(fi)
	return v, nil
}

func otherMarker(name string) string {
	if name == MarkerName {
		return BackupMarkerName
	}
	return MarkerName
}

// SameDevice reports whether two existing paths live on the same file
// system (st_dev). known is false where device numbers are unavailable.
func SameDevice(a, b string) (same, known bool, err error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, false, err
	}
	da, okA := devOf(fa)
	db, okB := devOf(fb)
	if !okA || !okB {
		return false, false, nil
	}
	return da == db, true, nil
}

// Check re-verifies that the root still holds the same marker and lives on the
// same device as at Open. Call it immediately before writing to the root.
func (v *Volume) Check() error {
	fi, err := os.Stat(v.root)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVolumeChanged, err)
	}
	if !fi.IsDir() {
		return ErrVolumeChanged
	}
	if v.hasDev {
		if dev, ok := devOf(fi); !ok || dev != v.dev {
			return ErrVolumeChanged
		}
	}
	m, err := readMarker(v.root, v.marker)
	if err != nil {
		if errors.Is(err, ErrNotInitialized) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrVolumeChanged, err)
	}
	if m.VolumeID != v.id {
		return ErrVolumeChanged
	}
	return nil
}

func resolveDir(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrRootMissing
	}
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrRootMissing
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", ErrNotDirectory
	}
	return resolved, nil
}

func readMarker(root, name string) (Marker, error) {
	data, err := os.ReadFile(filepath.Join(root, name))
	if errors.Is(err, os.ErrNotExist) {
		return Marker{}, ErrNotInitialized
	}
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return Marker{}, fmt.Errorf("%w: %w", ErrInvalidMarker, err)
	}
	if m.Format != markerFormat {
		return Marker{}, fmt.Errorf("%w: unsupported format %d", ErrInvalidMarker, m.Format)
	}
	if !validID(m.VolumeID) {
		return Marker{}, fmt.Errorf("%w: malformed volume_id", ErrInvalidMarker)
	}
	return m, nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// probeWritable creates, syncs and removes a temporary file. It probes .system
// when it already exists (that is where the server writes), otherwise the
// root itself, so that Open never creates .system.
func probeWritable(root string) error {
	dir := root
	if fi, err := os.Stat(filepath.Join(root, ".system")); err == nil && fi.IsDir() {
		dir = filepath.Join(root, ".system")
	}
	f, err := os.CreateTemp(dir, ".withalbum-probe-*")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotWritable, err)
	}
	name := f.Name()
	_, werr := f.Write([]byte{0})
	serr := f.Sync()
	cerr := f.Close()
	rerr := os.Remove(name)
	if err := errors.Join(werr, serr, cerr, rerr); err != nil {
		return fmt.Errorf("%w: %w", ErrNotWritable, err)
	}
	return nil
}

// MkdirIn creates dir with os.Mkdir (never MkdirAll), so a missing parent such
// as an unmounted volume root fails instead of being recreated. An existing
// directory is not an error.
func MkdirIn(dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}
