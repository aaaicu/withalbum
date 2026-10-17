package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
)

// Where the storage root comes from, and setup mode (M24).
//
// The storage root is, in this order: WITHALBUM_ROOT; the root the setup
// screen registered, kept in the per-user config file (WITHALBUM_CONFIG,
// default os.UserConfigDir()/WithAlbum/server.json, with its volume id
// pinned); ./withalbum-data.
//
// Setup mode serves only the setup screen (internal/httpapi/setup.go), on the
// plain loopback listener, when the storage is not registered yet (and may
// be chosen: not a root named by the config file, whose absence means its
// disk is missing) or no administrator has been set up. Registering the
// storage runs the normal startup sequence and keeps the storage open and
// locked; creating the administrator then swaps the request handler for the
// normal API built on that same storage, without a restart.

// rootSource is where the storage root came from.
type rootSource int

const (
	sourceDefault rootSource = iota // ./withalbum-data
	sourceEnv                       // WITHALBUM_ROOT
	sourceConfig                    // the config file written by setup
)

type storageTarget struct {
	Root     string
	VolumeID string // WITHALBUM_VOLUME_ID, else the config file's volume id
	Source   rootSource
}

// serverConfig is the config file.
type serverConfig struct {
	Format      int    `json:"format"`
	StorageRoot string `json:"storage_root"`
	VolumeID    string `json:"volume_id"`
}

const configFormat = 1

// configPath is WITHALBUM_CONFIG or the per-user default.
func configPath() (string, error) {
	if p := os.Getenv("WITHALBUM_CONFIG"); p != "" {
		return filepath.Abs(p)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no per-user config directory (%w); set WITHALBUM_ROOT or WITHALBUM_CONFIG", err)
	}
	return filepath.Join(dir, "WithAlbum", "server.json"), nil
}

// readConfig returns nil when there is no config file. A file that exists
// but cannot be read or understood is an error (fail closed).
func readConfig() (*serverConfig, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	var c serverConfig
	if err := json.Unmarshal(b, &c); err != nil || c.Format != configFormat || !filepath.IsAbs(c.StorageRoot) {
		return nil, fmt.Errorf("config file %s is not a valid WithAlbum server config; fix or remove it, or set WITHALBUM_ROOT", path)
	}
	return &c, nil
}

// writeConfig writes the config file atomically (directory 0700, file 0600).
func writeConfig(c serverConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	c.Format = configFormat
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".server-*.json") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return storage.SyncDir(dir)
}

// resolveStorage decides the storage root and its volume pin.
func resolveStorage() (storageTarget, error) {
	vid := os.Getenv("WITHALBUM_VOLUME_ID")
	if root := os.Getenv("WITHALBUM_ROOT"); root != "" {
		abs, err := filepath.Abs(root)
		return storageTarget{Root: abs, VolumeID: vid, Source: sourceEnv}, err
	}
	c, err := readConfig()
	if err != nil {
		return storageTarget{}, err
	}
	if c != nil {
		if vid == "" {
			vid = c.VolumeID
		}
		return storageTarget{Root: c.StorageRoot, VolumeID: vid, Source: sourceConfig}, nil
	}
	abs, err := filepath.Abs("./withalbum-data")
	return storageTarget{Root: abs, VolumeID: vid, Source: sourceDefault}, err
}

// storageRoot returns the absolute storage root (see resolveStorage).
func storageRoot() (string, error) {
	t, err := resolveStorage()
	return t.Root, err
}

// expectedVolumeID is the volume pin of the storage root (see
// resolveStorage); "" accepts any registered volume.
func expectedVolumeID() string {
	t, _ := resolveStorage()
	return t.VolumeID
}

// storageSetupPossible reports whether a failed startup may continue in
// setup mode to register a storage: the root is not registered yet, or the
// default root does not exist. A root from WITHALBUM_ROOT must exist (the
// server never creates it), and a root from the config file was registered
// before, so its absence means the disk is missing: both fail closed.
func storageSetupPossible(t storageTarget, openErr error) bool {
	if t.Source == sourceConfig {
		return false
	}
	if errors.Is(openErr, storage.ErrNotInitialized) {
		return true
	}
	return t.Source == sourceDefault && errors.Is(openErr, storage.ErrRootMissing)
}

// setupListenProblem explains why lc cannot serve the setup screen ("" when
// it can): only the plain HTTP loopback listener does.
func setupListenProblem(lc listenConfig) string {
	scope, err := scopeOf(lc.Addr)
	if err != nil {
		return err.Error()
	}
	if scope != scopeLoopback || lc.TLS || lc.AllowLAN {
		return "the setup screen is served only on this machine over plain loopback HTTP; start once without -tls and -allow-lan (WITHALBUM_LISTEN=127.0.0.1:8787), or use the console: withalbum-server -init-storage and withalbum-server admin init"
	}
	return ""
}

// switchHandler serves the current handler; setup mode replaces it once.
type switchHandler struct{ h atomic.Pointer[http.Handler] }

func (s *switchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}
func (s *switchHandler) set(h http.Handler) { s.h.Store(&h) }

// setupRun is one run of setup mode.
type setupRun struct {
	target  storageTarget
	opts    appOptions
	handler *switchHandler
	// normal is called once with the opened storage after the switch to
	// normal mode (starts the background workers).
	normal func(*app)

	mu  sync.Mutex
	app *app
}

// current returns the opened storage (nil before registration).
func (s *setupRun) current() *app {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.app
}

// newSetupRun builds setup mode. a is the opened storage when the server
// started with a registered storage but no administrator, otherwise nil.
func newSetupRun(target storageTarget, opts appOptions, a *app, code *httpapi.SetupCode, normal func(*app)) (*setupRun, *httpapi.Setup) {
	s := &setupRun{target: target, opts: opts, handler: &switchHandler{}, normal: normal, app: a}
	cfg := httpapi.SetupConfig{Code: code, Register: s.register, Claim: s.claim, Activate: s.activate, PasswordParams: opts.Auth.PasswordParams}
	if target.Source == sourceEnv {
		cfg.FixedRoot = target.Root
	} else {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.Home = home
		}
		if runtime.GOOS == "darwin" {
			cfg.VolumesDir = "/Volumes"
		}
	}
	if a != nil {
		cfg.Storage = httpapi.DescribeStorage(a.vol.Root())
	}
	setup := httpapi.NewSetup(cfg)
	s.handler.set(loopbackHostOnly(setup.Handler()))
	return s, setup
}

// register implements SetupConfig.Register: storage.Init (an already
// registered root is used as it is), the normal startup sequence, and the
// config file (unless WITHALBUM_ROOT names the root).
func (s *setupRun) register(ctx context.Context, root string) (bool, error) {
	// A closed browser tab must not stop the startup sequence half way.
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.app != nil {
		return false, &httpapi.SetupRefusal{Code: "storage_already_set", Err: errors.New("storage already registered")}
	}
	if m, err := storage.Init(root); err == nil {
		log.Printf("setup: registered the storage root %s (volume_id=%s)", root, m.VolumeID)
	} else if !errors.Is(err, storage.ErrAlreadyInitialized) {
		return false, setupRefusal(err)
	}
	a, err := openApp(ctx, root, s.opts)
	if err != nil {
		return false, setupRefusal(err)
	}
	if s.target.Source != sourceEnv {
		if err := writeConfig(serverConfig{StorageRoot: a.vol.Root(), VolumeID: a.vol.ID()}); err != nil {
			a.Close()
			return false, &httpapi.SetupRefusal{Code: "config_failed", Err: err}
		}
		if p, err := configPath(); err == nil {
			log.Printf("setup: storage root and volume id saved in %s", p)
		}
	}
	claimed, err := a.auth.Users.Initialized(ctx)
	if err != nil {
		a.Close()
		return false, err
	}
	s.app = a
	return claimed, nil
}

func setupRefusal(err error) error {
	code := ""
	switch {
	case errors.Is(err, errUnmigratedState):
		code = "legacy_metadata"
	case errors.Is(err, storage.ErrLocked):
		code = "locked"
	case errors.Is(err, storage.ErrRestorePending):
		code = "restore_pending"
	case errors.Is(err, storage.ErrVolumeMismatch):
		code = "volume_mismatch"
	case errors.Is(err, storage.ErrInvalidMarker):
		code = "invalid_marker"
	case errors.Is(err, storage.ErrNotWritable):
		code = "not_writable"
	case errors.Is(err, storage.ErrRootMissing), errors.Is(err, storage.ErrNotDirectory):
		code = "missing"
	default:
		return err
	}
	return &httpapi.SetupRefusal{Code: code, Err: err}
}

// claim implements SetupConfig.Claim with the same takeover as "admin init".
func (s *setupRun) claim(ctx context.Context, c users.Claim) error {
	a := s.current()
	if a == nil {
		return errors.New("no storage registered")
	}
	c.SetupComplete = true
	ctx = context.WithoutCancel(ctx)
	u, err := a.auth.Users.ClaimBootstrap(ctx, c, users.Actor{Via: "setup"})
	if err != nil {
		return err
	}
	if owned, err := a.auth.Users.OwnedFolderCount(ctx, u.ID); err == nil && owned > 0 {
		log.Printf("setup: administrator %s took over the bootstrap account and keeps owning %d existing folder(s)", u.LoginName, owned)
	}
	return nil
}

// activate implements SetupConfig.Activate: from now on every request is
// answered by the normal API of the opened storage; setup routes are gone.
func (s *setupRun) activate() {
	a := s.current()
	s.handler.set(loopbackHostOnly(a.api.Handler()))
	log.Printf("setup: finished; serving the normal API and dashboard (setup routes removed)")
	if s.normal != nil {
		s.normal(a)
	}
}
