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
	"sync/atomic"
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
	// ErrDiskMismatch means the marker records the UUID of another disk:
	// the marker (or the whole library) was copied to this disk. Refused
	// until an operator runs -adopt-disk.
	ErrDiskMismatch = errors.New("storage marker was created on another disk (disk UUID does not match)")
	// ErrDiskUnknown means the marker records a disk UUID but the UUID of
	// the disk now holding the root cannot be read.
	ErrDiskUnknown = errors.New("cannot read the disk UUID that the storage marker requires")
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
	// DiskUUID is the UUID of the file system the marker was created on
	// (macOS volume UUID, Linux /dev/disk/by-uuid), empty where unknown.
	// Open refuses the root when it lives on a disk with another UUID, so
	// a marker copied to another disk is not mistaken for the original.
	// Markers written before it existed get it on the next server start
	// (AdoptDiskUUID). Older versions ignore the field.
	DiskUUID string `json:"disk_uuid,omitempty"`
}

// diskUUID looks up the disk identity of a path (a variable for tests).
var diskUUID = platformDiskUUID

// OpenOptions configures Open.
type OpenOptions struct {
	// ExpectedVolumeID, when non-empty, pins the volume (WITHALBUM_VOLUME_ID).
	ExpectedVolumeID string
	// AllowPendingRestore opens a root that holds RestoreJournalName. Only
	// the restore command sets it.
	AllowPendingRestore bool
	// IgnoreDiskUUID opens a root whose marker records another disk UUID.
	// Only the -adopt-disk command sets it, to record the current one.
	IgnoreDiskUUID bool
}

// Volume is a verified storage root. It is safe for concurrent use.
type Volume struct {
	root    string // symlink-resolved absolute path
	id      string
	dev     uint64
	hasDev  bool
	marker  string // MarkerName or BackupMarkerName
	lockDir string // directory of the instance lock, relative to root
	// markerDisk is the disk UUID recorded in the marker at Open, curDisk
	// the UUID of the disk holding the root then ("" where unknown).
	markerDisk, curDisk string
	// lost holds the first Check failure. A volume that disappeared once
	// stays unavailable until the process opens it again: after a detach
	// the open database and the instance lock belong to the old mount, even
	// when the disk comes back with the same device number.
	lost atomic.Pointer[error]
}

// DiskUUID returns the UUID of the disk holding the root as read at Open
// ("" where unknown) and the one recorded in the marker.
func (v *Volume) DiskUUID() (current, recorded string) { return v.curDisk, v.markerDisk }

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
	if name == MarkerName {
		// Best effort: a disk without a readable UUID is still usable.
		if u, err := diskUUID(resolved); err == nil {
			m.DiskUUID = u
		}
	}
	if err := writeMarker(resolved, name, m); err != nil {
		return Marker{}, err
	}
	return m, nil
}

// writeMarker atomically writes m as dir/name (temp file, fsync, rename,
// directory fsync). The file has mode 0600.
func writeMarker(dir, name string, m Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// os.CreateTemp creates the file with mode 0600.
	tmp, err := os.CreateTemp(dir, ".withalbum-volume-*.tmp")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotWritable, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

// Open verifies that root is an initialized, writable storage root and
// records its device number for later Check calls. It creates nothing in root
// except a short-lived probe file.
//
// A root holding RestoreJournalName is refused with ErrRestorePending unless
// opt.AllowPendingRestore is set.
func Open(root string, opt OpenOptions) (*Volume, error) {
	v, err := open(root, MarkerName, ".system", opt.ExpectedVolumeID, true, opt.IgnoreDiskUUID)
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
	return open(dir, BackupMarkerName, ".", "", writable, false)
}

func open(root, marker, lockDir, expectedID string, writable, ignoreDisk bool) (*Volume, error) {
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
	var cur string
	if marker == MarkerName {
		var derr error
		cur, derr = diskUUID(resolved)
		if m.DiskUUID != "" && !ignoreDisk {
			switch {
			case derr != nil:
				return nil, fmt.Errorf("%w: %w", ErrDiskUnknown, derr)
			case cur == "":
				return nil, ErrDiskUnknown
			case !strings.EqualFold(cur, m.DiskUUID):
				return nil, fmt.Errorf("%w: the marker records disk %s, the root is on disk %s", ErrDiskMismatch, m.DiskUUID, cur)
			}
		}
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
	v := &Volume{root: resolved, id: m.VolumeID, marker: marker, lockDir: lockDir, markerDisk: m.DiskUUID, curDisk: cur}
	v.dev, v.hasDev = devOf(fi)
	return v, nil
}

func otherMarker(name string) string {
	if name == MarkerName {
		return BackupMarkerName
	}
	return MarkerName
}

// AdoptDiskUUID records the UUID of the disk holding the root in a marker
// written before markers had one, and returns it. It does nothing (and
// returns "") when the marker already has one or the disk has no readable
// UUID. The caller holds the instance lock.
func (v *Volume) AdoptDiskUUID() (string, error) {
	if v.marker != MarkerName || v.markerDisk != "" || v.curDisk == "" {
		return "", nil
	}
	if err := v.rewriteDisk(v.curDisk); err != nil {
		return "", err
	}
	return v.curDisk, nil
}

// RebindDiskUUID replaces the disk UUID in the marker with the one of the
// disk now holding the root ("" where it has none) and returns the old and
// new values. It is the operator's explicit decision that this disk is now
// the library (-adopt-disk, after a deliberate copy or clone). The caller
// holds the instance lock.
func (v *Volume) RebindDiskUUID() (old, cur string, err error) {
	if v.marker != MarkerName {
		return "", "", errors.New("storage: not a storage root")
	}
	old, cur = v.markerDisk, v.curDisk
	if err := v.rewriteDisk(cur); err != nil {
		return "", "", err
	}
	return old, cur, nil
}

func (v *Volume) rewriteDisk(uuid string) error {
	if err := v.Check(); err != nil {
		return err
	}
	m, err := readMarker(v.root, v.marker)
	if err != nil {
		return err
	}
	if m.VolumeID != v.id {
		return ErrVolumeChanged
	}
	m.DiskUUID = uuid
	if err := writeMarker(v.root, v.marker, m); err != nil {
		return err
	}
	v.markerDisk = uuid
	return nil
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
//
// Once Check has failed it keeps failing (ErrVolumeChanged), also when the
// disk is reconnected: database handles and the instance lock opened on the
// detached mount do not come back with it, so writing could resume without
// the lock and through stale handles. Restarting the process opens the
// volume again.
func (v *Volume) Check() error {
	if first := v.lost.Load(); first != nil {
		return fmt.Errorf("%w: the storage became unavailable earlier (%v); restart once the disk is connected again", ErrVolumeChanged, *first)
	}
	err := v.check()
	if err != nil {
		v.lost.CompareAndSwap(nil, &err)
	}
	return err
}

func (v *Volume) check() error {
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
