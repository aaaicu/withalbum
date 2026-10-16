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
)

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
}

// Volume is a verified storage root. It is safe for concurrent use.
type Volume struct {
	root   string // symlink-resolved absolute path
	id     string
	dev    uint64
	hasDev bool
}

// Root returns the symlink-resolved storage root.
func (v *Volume) Root() string { return v.root }

// ID returns the volume id recorded in the marker.
func (v *Volume) ID() string { return v.id }

// Init writes a new marker into an existing directory. It never creates the
// root itself and never touches existing data other than adding the marker.
func Init(root string) (Marker, error) {
	resolved, err := resolveDir(root)
	if err != nil {
		return Marker{}, err
	}
	markerPath := filepath.Join(resolved, MarkerName)
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
func Open(root string, opt OpenOptions) (*Volume, error) {
	resolved, err := resolveDir(root)
	if err != nil {
		return nil, err
	}
	m, err := readMarker(resolved)
	if err != nil {
		return nil, err
	}
	if want := strings.ToLower(strings.TrimSpace(opt.ExpectedVolumeID)); want != "" && want != m.VolumeID {
		return nil, ErrVolumeMismatch
	}
	if err := probeWritable(resolved); err != nil {
		return nil, err
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	v := &Volume{root: resolved, id: m.VolumeID}
	v.dev, v.hasDev = devOf(fi)
	return v, nil
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
	m, err := readMarker(v.root)
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

func readMarker(root string) (Marker, error) {
	data, err := os.ReadFile(filepath.Join(root, MarkerName))
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
