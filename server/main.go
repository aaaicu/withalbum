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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/legacyimport"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
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

// storageRoot returns the absolute WITHALBUM_ROOT (default ./withalbum-data).
func storageRoot() (string, error) {
	root := os.Getenv("WITHALBUM_ROOT")
	if root == "" {
		root = "./withalbum-data"
	}
	return filepath.Abs(root)
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
	vol, err := storage.Open(root, storage.OpenOptions{ExpectedVolumeID: os.Getenv("WITHALBUM_VOLUME_ID")})
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
		MinFreeBytes: opt.MinFreeBytes, IntegrityWarning: !a.report.Healthy(), Faults: opt.Faults, Auth: auth})
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
	useTLS := flag.Bool("tls", false, "Serve HTTPS with the server's self-signed certificate (created in .system/tls on first use); clients pin its key fingerprint")
	allowLAN := flag.Bool("allow-lan", false, "Allow a non-loopback WITHALBUM_LISTEN address (private LAN address or wildcard). Requires -tls and an active server administrator")
	flag.Parse()
	if *showVersion {
		fmt.Println("withalbum-server " + version)
		return
	}
	root, err := storageRoot()
	if err != nil {
		log.Fatal(err)
	}
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
		return
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
	minFree, err := minFreeBytes(os.Getenv("WITHALBUM_MIN_FREE_BYTES"))
	if err != nil {
		log.Fatal(err)
	}
	a, err := openApp(context.Background(), root, appOptions{VolumeID: os.Getenv("WITHALBUM_VOLUME_ID"), MinFreeBytes: minFree})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	lan, err := a.listenAllowed(context.Background(), lc)
	if err != nil {
		a.Close()
		log.Fatalf("SECURITY: %v", err)
	}
	if free, err := a.vol.FreeBytes(); err != nil {
		log.Printf("free space: unknown (%v); uploads are not pre-checked, a full disk still answers 507", err)
	} else if free <= uint64(minFree) {
		log.Printf("WARNING: only %d bytes free, at or below the reserve WITHALBUM_MIN_FREE_BYTES=%d: uploads are refused with 507", free, minFree)
	} else {
		log.Printf("free space: %d bytes, reserve %d", free, minFree)
	}
	var tlsConfig *tls.Config
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
		urls := serverURLs(lc.Addr, net.InterfaceAddrs)
		writeFingerprints(log.Writer(), id, urls)
		a.api.SetTLSInfo(tlsInfo(id, urls))
		tlsConfig = tlsid.ServerConfig(id)
	}
	handler := a.api.Handler()
	if !lc.TLS {
		handler = loopbackHostOnly(handler)
	}
	server := newHTTPServer(handler, tlsConfig)
	ln, err := net.Listen("tcp", lc.Addr)
	if err != nil {
		a.Close()
		log.Fatal(err)
	}
	// "localhost" is resolved by the system; refuse if it did not bind to a
	// loopback address (for example an altered hosts file).
	if err := checkBoundAddr(ln.Addr(), lan); err != nil {
		ln.Close()
		a.Close()
		log.Fatalf("SECURITY: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	switch {
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
		a.Close()
		log.Fatal(err)
	}
}
