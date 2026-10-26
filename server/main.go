package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/legacyimport"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/relay"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/tlsid"
)

// This is a development server, NOT a production service. Every route
// except the dashboard page, health, login and refresh requires a session,
// and folder and asset routes are authorized per request (album ACL). Plain
// HTTP is served on loopback only; a LAN listener needs -allow-lan, -tls and
// a usable administrator (listen.go). Never publish its port to the
// internet.

// Set with -ldflags "-X main.version=<version>" by the release builder.
var version = "dev"

// openStorageHint explains a storage.Open failure in operator terms.
func openStorageHint(root string, err error) string {
	switch {
	case errors.Is(err, storage.ErrRootMissing):
		return fmt.Sprintf("storage root %q does not exist. If it is on an external disk, check that the disk is mounted. The server never creates the storage root.", root)
	case errors.Is(err, storage.ErrNotDirectory):
		return fmt.Sprintf("storage root %q is not a directory.", root)
	case errors.Is(err, storage.ErrNotInitialized):
		_, stateErr := os.Stat(jsonstore.StatePath(root))
		_, dbErr := os.Stat(sqlitestore.DBPath(root))
		if stateErr == nil || dbErr == nil {
			return fmt.Sprintf("existing WithAlbum data detected in %q but the volume is not registered. Confirm this is the right disk, then run: withalbum-server -init-storage (existing data is kept).", root)
		}
		return fmt.Sprintf("storage root %q is not initialized. Confirm the path is on the intended disk, then run once: withalbum-server -init-storage", root)
	case errors.Is(err, storage.ErrInvalidMarker):
		return fmt.Sprintf("volume marker %s in %q is unreadable or unsupported; refusing to start.", storage.MarkerName, root)
	case errors.Is(err, storage.ErrVolumeMismatch):
		return fmt.Sprintf("storage root %q holds a different volume than WITHALBUM_VOLUME_ID; refusing to start. Check which disk is mounted.", root)
	case errors.Is(err, storage.ErrDiskMismatch):
		return fmt.Sprintf("SECURITY: the volume marker %s in %q was created on another disk: this is a copy, not the registered library disk. Refusing to start so that nothing is written to the wrong disk. "+
			"Connect the original disk. Only if you deliberately copied or cloned the library to this disk and no longer use the old one, stop every WithAlbum process and run once: withalbum-server -adopt-disk", storage.MarkerName, root)
	case errors.Is(err, storage.ErrDiskUnknown):
		return fmt.Sprintf("SECURITY: the volume marker in %q records the disk it belongs to, but the UUID of the disk now holding %q cannot be read; refusing to start. Check the disk; if this is intended (for example a file system without a UUID), run: withalbum-server -adopt-disk", root, root)
	case errors.Is(err, storage.ErrRestorePending):
		return fmt.Sprintf("a restore into %q was interrupted (%s exists). Finish it by running the same withalbum-server restore -from ... command again; nothing else may use the storage until then.", root, storage.RestoreJournalName)
	case errors.Is(err, storage.ErrNotWritable):
		return fmt.Sprintf("storage root %q is not writable (read-only mount or permissions).", root)
	}
	return fmt.Sprintf("cannot open storage root %q.", root)
}

// errUnmigratedState means the volume holds PoC metadata (.system/state.json)
// that has not been imported into SQLite. The server refuses to start rather
// than ignore or modify that data.
var errUnmigratedState = errors.New("unmigrated legacy metadata (.system/state.json)")

// fileExists reports whether path exists. Errors other than "not exist" are
// returned so that an unreadable path fails closed.
func fileExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// openMetadata picks the metadata store for a verified volume:
//
//   - .system/withalbum.db exists: open it (SQLite).
//   - only .system/state.json exists: refuse with errUnmigratedState and
//     touch nothing; importing it is a separate, explicit step.
//   - neither exists: new install, create the SQLite database.
//
// If both exist, state.json must already be recorded as imported
// (legacy_imports holds its SHA-256); otherwise it is refused as well, so
// data in an unimported state.json is never silently ignored.
func openMetadata(ctx context.Context, vol *storage.Volume) (meta.MetadataStore, error) {
	statePath := jsonstore.StatePath(vol.Root())
	hasDB, err := fileExists(sqlitestore.DBPath(vol.Root()))
	if err != nil {
		return nil, err
	}
	hasState, err := fileExists(statePath)
	if err != nil {
		return nil, err
	}
	if hasState && !hasDB {
		return nil, errUnmigratedState
	}
	store, err := sqlitestore.Open(ctx, vol)
	if err != nil {
		return nil, err
	}
	if hasState {
		b, err := os.ReadFile(statePath)
		if err != nil {
			store.Close()
			return nil, err
		}
		sum := sha256.Sum256(b)
		imported, err := store.HasLegacyImport(ctx, hex.EncodeToString(sum[:]))
		if err != nil || !imported {
			store.Close()
			return nil, errors.Join(errUnmigratedState, err)
		}
	}
	return store, nil
}

// runMigrateLegacy implements "withalbum-server migrate-legacy [-apply] [-json]"
// and returns the process exit code: 0 success, 1 failure, 2 usage error.
func runMigrateLegacy(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate-legacy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "Import .system/state.json into SQLite. Without it only checks and reports (nothing is written)")
	asJSON := fs.Bool("json", false, "Print the report as JSON instead of a summary")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: withalbum-server migrate-legacy [-apply] [-json]")
		fmt.Fprintln(stderr, "Imports the metadata of an earlier version (WITHALBUM_ROOT/.system/state.json) into .system/withalbum.db.")
		fmt.Fprintln(stderr, "Stop every WithAlbum server using this storage first.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	root, err := storageRoot()
	if err != nil {
		fmt.Fprintf(stderr, "migrate-legacy: %v\n", err)
		return 1
	}
	vol, err := storage.Open(root, storage.OpenOptions{ExpectedVolumeID: expectedVolumeID()})
	if err != nil {
		fmt.Fprintf(stderr, "migrate-legacy: STORAGE: %s (%v)\n", openStorageHint(root, err), err)
		return 1
	}
	if *apply {
		lock, err := vol.Lock()
		if err != nil {
			fmt.Fprintf(stderr, "migrate-legacy: %s (%v)\n", lockHint(root, err), err)
			return 1
		}
		defer lock.Release()
	}
	report, err := legacyimport.Run(context.Background(), vol, legacyimport.Options{Apply: *apply, Progress: stderr})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(report); encErr != nil {
			fmt.Fprintf(stderr, "migrate-legacy: %v\n", encErr)
			return 1
		}
	} else {
		report.WriteSummary(stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "migrate-legacy: FAILED: %v\n", err)
		if *apply && !errors.Is(err, legacyimport.ErrNoLegacyState) {
			fmt.Fprintln(stderr, "migrate-legacy: the contents of state.json and the original files are never modified. Fix the cause and run -apply again; it continues or starts over safely.")
		}
		return 1
	}
	return 0
}

// runAdoptDisk implements -adopt-disk: the marker's disk UUID becomes the
// one of the disk now holding root. It takes the instance lock.
func runAdoptDisk(root, volumeID string, stdout, stderr io.Writer) int {
	vol, err := storage.Open(root, storage.OpenOptions{ExpectedVolumeID: volumeID, IgnoreDiskUUID: true})
	if err != nil {
		fmt.Fprintf(stderr, "adopt-disk: STORAGE: %s (%v)\n", openStorageHint(root, err), err)
		return 1
	}
	lock, err := vol.Lock()
	if err != nil {
		fmt.Fprintf(stderr, "adopt-disk: %s (%v)\n", lockHint(root, err), err)
		return 1
	}
	defer lock.Release()
	old, cur, err := vol.RebindDiskUUID()
	if err != nil {
		fmt.Fprintf(stderr, "adopt-disk: %v\n", err)
		return 1
	}
	show := func(u string) string {
		if u == "" {
			return "(none)"
		}
		return u
	}
	log.Printf("storage: adopt-disk: the volume marker of %s now names disk %s (was %s)", root, show(cur), show(old))
	fmt.Fprintf(stdout, "adopt-disk: %s volume_id=%s disk_uuid %s -> %s\n", root, vol.ID(), show(old), show(cur))
	return 0
}

// lockHint explains a storage.Volume.Lock failure.
func lockHint(root string, err error) string {
	if errors.Is(err, storage.ErrLocked) {
		return fmt.Sprintf("LOCKED: another WithAlbum server, migrate-legacy -apply or admin command is using %q (its process id is in .system/%s). Stop it first.", root, storage.LockName)
	}
	return fmt.Sprintf("cannot take the instance lock in %q.", root)
}

// minFreeBytes parses WITHALBUM_MIN_FREE_BYTES (bytes, default 1 GiB).
func minFreeBytes(value string) (int64, error) {
	if value == "" {
		return httpapi.DefaultMinFreeBytes, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("WITHALBUM_MIN_FREE_BYTES must be a non-negative number of bytes, got %q", value)
	}
	return n, nil
}

// appOptions configures openApp.
type appOptions struct {
	VolumeID     string // WITHALBUM_VOLUME_ID; empty: any registered volume
	MinFreeBytes int64
	Faults       httpapi.FaultInjector // tests only
	Auth         httpapi.AuthOptions   // zero: production defaults
	// Converters are the external preview tools (blobstore.FindConverters;
	// nil: none, HEIC and video previews are unsupported).
	Converters []blobstore.Converter
	// Transcoder makes the playback copies of videos
	// (blobstore.FindTranscoder; nil: none, videos are streamed as they
	// are).
	Transcoder blobstore.Transcoder
}

// app is a started server: a verified volume, its instance lock, the
// metadata store, the reconciled blob store and the HTTP API.
type app struct {
	vol    *storage.Volume
	lock   *storage.InstanceLock
	meta   meta.MetadataStore
	blobs  *blobstore.Store
	report *blobstore.ReconcileReport
	auth   *httpapi.Auth
	api    *httpapi.Server
}

// Close releases the metadata store and then the instance lock.
func (a *app) Close() error {
	return errors.Join(a.meta.Close(), a.lock.Release())
}

// openApp runs the startup sequence. Every step fails closed with an error
// meant for the operator:
//
//  1. verify the volume (marker, device),
//  2. take the instance lock, so no other server or import runs on it,
//  3. open the metadata (refusing unmigrated legacy data),
//  4. reconcile .system with the metadata after a possible crash: remove
//     unfinished uploads, quarantine unreferenced pool files to
//     .system/orphans, report missing or damaged originals,
//  5. build the HTTP API with its login and session services.
func openApp(ctx context.Context, root string, opt appOptions) (*app, error) {
	vol, err := storage.Open(root, storage.OpenOptions{ExpectedVolumeID: opt.VolumeID})
	if err != nil {
		return nil, fmt.Errorf("STORAGE: %s (%w)", openStorageHint(root, err), err)
	}
	lock, err := vol.Lock()
	if err != nil {
		return nil, fmt.Errorf("%s (%w)", lockHint(root, err), err)
	}
	a := &app{vol: vol, lock: lock}
	fail := func(err error) (*app, error) {
		if a.meta != nil {
			a.meta.Close()
		}
		lock.Release()
		return nil, err
	}
	// A marker written before markers recorded their disk gets the UUID of
	// the disk it is on now; from then on a copy on another disk is refused.
	if u, err := vol.AdoptDiskUUID(); err != nil {
		return fail(fmt.Errorf("STORAGE: cannot record the disk UUID in %s: %w", storage.MarkerName, err))
	} else if u != "" {
		log.Printf("storage: recorded disk UUID %s in the volume marker (first start with the disk identity check); a copy of this marker on another disk is refused from now on", u)
	}
	// Decide on the metadata store before creating anything else in .system.
	if a.meta, err = openMetadata(ctx, vol); err != nil {
		if errors.Is(err, errUnmigratedState) {
			return fail(fmt.Errorf("METADATA: %q holds metadata from an earlier version (.system/state.json) that has not been migrated to SQLite. "+
				"This version does not read it and refuses to start; nothing was changed. "+
				"Stop the previous version, then run: withalbum-server migrate-legacy (check only) and withalbum-server migrate-legacy -apply. "+
				"See docs/MIGRATION_legacy_state.md. (%w)", root, err))
		}
		return fail(fmt.Errorf("METADATA: cannot open %s: %w", sqlitestore.DBPath(root), err))
	}
	if a.blobs, err = blobstore.New(vol); err != nil {
		return fail(err)
	}
	index, ok := a.meta.(blobstore.BlobIndex)
	if !ok {
		return fail(fmt.Errorf("metadata store %T cannot list blobs", a.meta))
	}
	if a.report, err = a.blobs.Reconcile(ctx, index); err != nil {
		return fail(fmt.Errorf("RECOVERY: startup reconciliation failed; refusing to start: %w", err))
	}
	logReconcile(a.report)
	a.blobs.SetConverters(opt.Converters)
	a.blobs.SetTranscoder(opt.Transcoder)
	dbm, ok := a.meta.(interface{ DB() *sql.DB })
	if !ok {
		return fail(fmt.Errorf("metadata store %T has no account database", a.meta))
	}
	auth, err := httpapi.NewAuth(dbm.DB(), opt.Auth)
	if err != nil {
		return fail(fmt.Errorf("AUTH: %w", err))
	}
	a.auth = auth
	a.api = httpapi.New(httpapi.Config{Volume: vol, Meta: a.meta, Blobs: a.blobs, Version: version,
		MinFreeBytes: opt.MinFreeBytes, IntegrityWarning: !a.report.Healthy(), Faults: opt.Faults, Auth: auth,
		StartupCheck: &httpapi.StartupCheck{Blobs: a.report.Blobs, Missing: a.report.Missing, SizeMismatch: a.report.SizeMismatch,
			Orphans: a.report.Orphans, Unexpected: a.report.Unexpected}})
	rec, err := a.api.RecoverUploads(ctx)
	if err != nil {
		return fail(fmt.Errorf("RECOVERY: upload sessions: %w", err))
	}
	if rec.Dropped+rec.Restarted+rec.Truncated+rec.OrphansRemoved+rec.Unexpected > 0 {
		log.Printf("recovery: upload sessions=%d dropped=%d bundle_components_restarted=%d truncated=%d orphan_parts_removed=%d unexpected=%d",
			rec.Sessions, rec.Dropped, rec.Restarted, rec.Truncated, rec.OrphansRemoved, rec.Unexpected)
	}
	return a, nil
}

// logReconcile writes the startup reconciliation result. Missing or damaged
// originals are errors an operator must look at; the server still starts so
// that the remaining library stays available.
func logReconcile(r *blobstore.ReconcileReport) {
	log.Printf("recovery: %s", r.Summary())
	if r.Orphans > 0 {
		log.Printf("recovery: moved %d unreferenced original(s) to %s (left by an interrupted upload; nothing was deleted), e.g. %v",
			r.Orphans, blobstore.OrphansRelDir, r.OrphanSample)
	}
	if r.Missing > 0 {
		log.Printf("INTEGRITY ERROR: %d original(s) recorded in the database are missing from the pool, e.g. %v. Uploading the same file again restores it.", r.Missing, r.MissingSample)
	}
	if r.SizeMismatch > 0 {
		log.Printf("INTEGRITY ERROR: %d original(s) in the pool have the wrong size (damaged), e.g. %v. They were left untouched.", r.SizeMismatch, r.SizeMismatchSample)
	}
	if r.Unexpected > 0 {
		log.Printf("INTEGRITY WARNING: %d unexpected entries in .system/pool or .system/incoming were left untouched, e.g. %v", r.Unexpected, r.UnexpectedSample)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate-legacy" {
		os.Exit(runMigrateLegacy(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		os.Exit(runAdmin(os.Args[2:], consoleIO()))
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "verify":
			os.Exit(runVerify(os.Args[2:], os.Stdout, os.Stderr))
		case "backup":
			os.Exit(runBackup(os.Args[2:], os.Stdout, os.Stderr))
		case "restore":
			os.Exit(runRestore(os.Args[2:], os.Stdout, os.Stderr))
		case "tls":
			os.Exit(runTLSInfo(os.Args[2:], os.Stdout, os.Stderr))
		}
	}
	showVersion := flag.Bool("version", false, "Print WithAlbum Server version and exit")
	initStorage := flag.Bool("init-storage", false, "Register the existing WITHALBUM_ROOT directory as the storage volume and exit")
	adoptDisk := flag.Bool("adopt-disk", false, "Record the disk now holding WITHALBUM_ROOT as the library disk (after a deliberate copy or clone to a new disk) and exit; the server must be stopped")
	useTLS := flag.Bool("tls", false, "Serve HTTPS with the server's self-signed certificate (created in .system/tls on first use); clients pin its key fingerprint")
	allowLAN := flag.Bool("allow-lan", false, "Allow a non-loopback WITHALBUM_LISTEN address (private LAN address or wildcard). Requires -tls and an active server administrator")
	funnelFlag := flag.String("funnel-url", "", "Also serve apps through Tailscale Funnel at this public https://<machine>.<tailnet>.ts.net address (default: WITHALBUM_FUNNEL_URL; see docs/FUNNEL.md). Opens a second listener on loopback only; requires -tls and an active server administrator")
	funnelListenFlag := flag.String("funnel-listen", "", "Loopback address of the listener the Funnel TCP forwarder connects to (default: WITHALBUM_FUNNEL_LISTEN, else 127.0.0.1:8788)")
	relayFlag := flag.String("relay", "", "Also serve apps through the relay at host:port over an outbound connection (default: WITHALBUM_RELAY; see docs/RELAY_DESIGN.md). Requires -tls and an active server administrator")
	flag.Parse()
	if *showVersion {
		fmt.Println("withalbum-server " + version)
		return
	}
	if err := refuseRoot(euid(), os.Getenv); err != nil {
		log.Fatal(err)
	}
	target, err := resolveStorage()
	if err != nil {
		log.Fatal(err)
	}
	root := target.Root
	if *initStorage {
		m, err := storage.Init(root)
		if err != nil {
			if errors.Is(err, storage.ErrAlreadyInitialized) {
				log.Fatalf("storage root %q is already initialized; nothing changed.", root)
			}
			if errors.Is(err, storage.ErrRootMissing) || errors.Is(err, storage.ErrNotDirectory) {
				log.Fatalf("%s Create the directory on the intended disk first.", openStorageHint(root, err))
			}
			log.Fatalf("init storage: %v", err)
		}
		fmt.Printf("Initialized storage root %s\nvolume_id=%s\nOptional: set WITHALBUM_VOLUME_ID=%s to refuse any other volume.\n", root, m.VolumeID, m.VolumeID)
		if m.DiskUUID != "" {
			fmt.Printf("disk_uuid=%s (a copy of the marker on another disk is refused)\n", m.DiskUUID)
		} else {
			fmt.Println("disk_uuid: unknown for this file system; only the marker and the device number identify the volume")
		}
		return
	}
	if *adoptDisk {
		os.Exit(runAdoptDisk(root, target.VolumeID, os.Stdout, os.Stderr))
	}
	// Validate the configuration before touching the storage.
	lc := listenConfig{Addr: listenAddr(), AllowLAN: *allowLAN, TLS: *useTLS}
	if flag.NArg() != 0 {
		log.Fatalf("unexpected arguments %q", flag.Args())
	}
	// The administrator condition needs the database; check the rest first.
	if _, err := checkListen(lc, 1); err != nil {
		log.Fatalf("SECURITY: %v", err)
	}
	relayAddr := relayAddrSetting(*relayFlag)
	if err := checkRelay(lc, relayAddr); err != nil {
		log.Fatalf("SECURITY: %v", err)
	}
	funnelListen := funnelListenSetting(*funnelListenFlag)
	public, err := checkFunnel(lc, funnelURLSetting(*funnelFlag), funnelListen)
	if err != nil {
		log.Fatalf("SECURITY: %v", err)
	}
	minFree, err := minFreeBytes(os.Getenv("WITHALBUM_MIN_FREE_BYTES"))
	if err != nil {
		log.Fatal(err)
	}
	converters := blobstore.FindConverters(os.Getenv)
	for _, c := range converters {
		log.Printf("previews: %s at %s for %v", c.Name, c.Path, c.Types)
	}
	if len(converters) == 0 {
		log.Printf("previews: no external converter (sips, ffmpeg); HEIC and video originals are stored without a preview")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	transcoder := blobstore.FindTranscoder(ctx, os.Getenv)
	if transcoder != nil {
		log.Printf("playback copies: %s", transcoder.Name())
	} else {
		log.Printf("playback copies: no tool (avconvert, ffmpeg with an H.264 encoder) or WITHALBUM_PLAYBACK=off; videos are streamed as they are")
	}
	opts := appOptions{VolumeID: target.VolumeID, MinFreeBytes: minFree, Converters: converters, Transcoder: transcoder, Faults: chaosFaults()}
	a, err := openApp(ctx, root, opts)
	// Setup mode (server/setup.go) when the storage can still be chosen, or
	// when it has no administrator, and only on the plain loopback listener.
	setupMode := false
	if err != nil {
		if !storageSetupPossible(target, err) {
			if target.Source == sourceConfig {
				if p, perr := configPath(); perr == nil {
					log.Fatalf("%v\nThis storage root was registered by the setup screen and is kept in %s. If it is on an external disk, connect the disk and start again; to use another storage, set WITHALBUM_ROOT or remove that file.", err, p)
				}
			}
			log.Fatal(err)
		}
		if problem := setupListenProblem(lc); problem != "" {
			log.Fatalf("%v\nSETUP: %s", err, problem)
		}
		log.Printf("setup: no registered storage yet; starting setup mode (%v)", err)
		a, setupMode = nil, true
	} else {
		claimed, err := a.auth.Users.Initialized(ctx)
		if err != nil {
			a.Close()
			log.Fatalf("cannot read the administrator state: %v", err)
		}
		if !claimed {
			if problem := setupListenProblem(lc); problem == "" {
				setupMode = true
				log.Printf("setup: the storage %s has no administrator yet; starting setup mode", a.vol.Root())
			} else {
				log.Printf("NOTE: no administrator has been set up yet; %s", problem)
			}
		}
	}
	// normal starts the background work of an opened storage. The
	// playback worker runs an external tool for minutes; it is waited for
	// at shutdown (playbackDone) so that the tool is killed before the
	// process exits and never outlives it.
	playbackDone := make(chan struct{})
	var playbackStarted atomic.Bool
	normal := func(a *app) {
		go a.maintain(ctx, maintenanceInterval)
		go a.api.RunPreviews(ctx)
		go a.api.RunCaptures(ctx)
		if playbackStarted.CompareAndSwap(false, true) {
			go func() {
				defer close(playbackDone)
				a.api.RunPlayback(ctx)
			}()
		}
	}
	waitPlayback := func() {
		if !playbackStarted.Load() {
			return
		}
		select {
		case <-playbackDone:
		case <-time.After(15 * time.Second):
			log.Printf("shutdown: the playback worker did not stop in time")
		}
	}
	var (
		handler   http.Handler
		tlsConfig *tls.Config
		lan       bool
		setup     *setupRun
		code      string
		codeEnd   time.Time
		relayc    *relay.Client
	)
	if setupMode {
		sc, display, err := httpapi.NewSetupCode(time.Now())
		if err != nil {
			log.Fatalf("setup code: %v", err)
		}
		setup, _ = newSetupRun(target, opts, a, sc, normal)
		handler, code, codeEnd = setup.handler, display, sc.Expires()
	} else {
		if lan, err = a.listenAllowed(ctx, lc); err != nil {
			a.Close()
			log.Fatalf("SECURITY: %v", err)
		}
		if free, err := a.vol.FreeBytes(); err != nil {
			log.Printf("free space: unknown (%v); uploads are not pre-checked, a full disk still answers 507", err)
		} else if free <= uint64(minFree) {
			log.Printf("WARNING: only %d bytes free, at or below the reserve WITHALBUM_MIN_FREE_BYTES=%d: uploads are refused with 507", free, minFree) // #nosec G706 -- integers only
		} else {
			log.Printf("free space: %d bytes, reserve %d", free, minFree) // #nosec G706 -- integers only
		}
		if lc.TLS {
			id, err := tlsid.LoadOrCreate(a.vol.Root(), certHosts(lc.Addr), time.Now())
			if err != nil {
				a.Close()
				log.Fatalf("TLS: %v (file %s; it must be a regular file with mode 0600. Never delete it casually: a new key means pairing every device again)", err, tlsid.Path(a.vol.Root()))
			}
			switch {
			case id.Created:
				log.Printf("TLS: created a new key pair in %s (mode 0600). Pair devices with the fingerprint below.", tlsid.Path(a.vol.Root()))
			case id.Renewed:
				log.Printf("TLS: re-issued the certificate with the same key; the pin did not change.")
			}
			urls := pairingURLs(lc.Addr, public, net.InterfaceAddrs)
			var rr remoteRoutes
			if relayAddr != "" {
				if relayc, err = a.setupRelay(ctx, relayAddr, id); err != nil {
					a.Close()
					log.Fatalf("SECURITY: %v", err)
				}
				rr.Relay = relayTargetFor(id, relayAddr)
			}
			if public != "" {
				if err := a.setupFunnel(ctx, public); err != nil {
					a.Close()
					log.Fatalf("SECURITY: %v", err)
				}
				rr.Public = public
			}
			writeFingerprints(log.Writer(), id, urls, rr)
			a.api.SetTLSInfo(tlsInfo(id, urls, rr))
			tlsConfig = tlsid.ServerConfig(id)
		}
		handler = a.api.Handler()
		if !lc.TLS {
			handler = loopbackHostOnly(handler)
		}
	}
	// closeApp releases the opened storage (in setup mode: if registered).
	closeApp := func() {
		if setup != nil {
			a = setup.current()
		}
		if a != nil {
			if ctx.Err() != nil {
				waitPlayback() // shutting down
			}
			a.Close()
		}
	}
	defer closeApp()
	server := newHTTPServer(handler, tlsConfig)
	ln, err := net.Listen("tcp", lc.Addr)
	if err != nil {
		closeApp()
		log.Fatal(err)
	}
	// "localhost" is resolved by the system; refuse if it did not bind to a
	// loopback address (for example an altered hosts file).
	if err := checkBoundAddr(ln.Addr(), lan); err != nil {
		ln.Close()
		closeApp()
		log.Fatalf("SECURITY: %v", err)
	}
	var funnelLn net.Listener
	if public != "" && !setupMode {
		if funnelLn, err = listenFunnel(funnelListen); err != nil {
			ln.Close()
			closeApp()
			log.Fatalf("SECURITY: funnel listener: %v", err)
		}
	}
	if !setupMode {
		normal(a)
	}
	var funnelServer *http.Server
	if funnelLn != nil {
		funnelServer = serveFunnel(funnelLn, handler, tlsConfig)
		log.Printf("funnel: serving apps for %s on %s (loopback only; app routes only). Forward it without terminating TLS: %s", public, funnelLn.Addr(), funnelCommand(public, funnelLn.Addr().String()))
		log.Printf("funnel: this route is reachable by anyone on the internet who knows the address; logins are rate-limited, administration is refused. Tailscale sees when and how much data flows, never its content.")
	}
	var relayServer *http.Server
	if relayc != nil {
		relayServer = serveRelay(ctx, relayc, handler, tlsConfig)
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if relayServer != nil {
			if err := relayServer.Shutdown(shutCtx); err != nil {
				log.Printf("shutdown (relay): %v", err)
			}
		}
		if funnelServer != nil {
			if err := funnelServer.Shutdown(shutCtx); err != nil {
				log.Printf("shutdown (funnel): %v", err)
			}
		}
		if err := server.Shutdown(shutCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	switch {
	case setupMode:
		log.Printf("EXPERIMENTAL / SETUP MODE, LOOPBACK ONLY, PLAIN HTTP: listen=%s storage=%s", ln.Addr(), root)
		log.Print(httpapi.SetupBanner("http://"+ln.Addr().String()+"/setup", code, codeEnd))
	case lan:
		log.Printf("EXPERIMENTAL / LAN, TLS ONLY: listen=%s root=%s volume_id=%s. Do not forward this port from the internet.", ln.Addr(), a.vol.Root(), a.vol.ID())
		if scope, _ := scopeOf(lc.Addr); scope == scopeWildcard {
			log.Printf("WARNING: listening on every interface (%s); make sure no interface is reachable from the internet.", lc.Addr)
		}
	case lc.TLS:
		log.Printf("EXPERIMENTAL / LOOPBACK ONLY, TLS: listen=%s root=%s volume_id=%s", ln.Addr(), a.vol.Root(), a.vol.ID())
	default:
		log.Printf("EXPERIMENTAL / LOOPBACK ONLY, PLAIN HTTP: listen=%s root=%s volume_id=%s", ln.Addr(), a.vol.Root(), a.vol.ID())
	}
	if lc.TLS {
		err = server.ServeTLS(ln, "", "")
	} else {
		err = server.Serve(ln)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		closeApp()
		log.Fatal(err)
	}
}
