package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

// This is a LAN/localhost demonstration server, NOT a production service.
// In particular, it intentionally has NO AUTHENTICATION. Never publish its port.

// Set with -ldflags "-X main.version=<version>" by the release builder.
var version = "dev"

// Keep this prototype strictly on the local machine until authentication, ACLs,
// rate limiting and TLS/reverse-proxy security have been implemented.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

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

func main() {
	showVersion := flag.Bool("version", false, "Print WithAlbum Server version and exit")
	initStorage := flag.Bool("init-storage", false, "Register the existing WITHALBUM_ROOT directory as the storage volume and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("withalbum-server " + version)
		return
	}
	root := os.Getenv("WITHALBUM_ROOT")
	if root == "" {
		root = "./withalbum-data"
	}
	root, err := filepath.Abs(root)
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
	vol, err := storage.Open(root, storage.OpenOptions{ExpectedVolumeID: os.Getenv("WITHALBUM_VOLUME_ID")})
	if err != nil {
		log.Fatalf("STORAGE: %s (%v)", openStorageHint(root, err), err)
	}
	// Decide on the metadata store before creating anything else in .system.
	metadata, err := openMetadata(context.Background(), vol)
	if errors.Is(err, errUnmigratedState) {
		log.Fatalf("METADATA: %q holds metadata from an earlier version (.system/state.json) that has not been migrated to SQLite. "+
			"This version does not read it and refuses to start; nothing was changed. "+
			"Keep running the previous version until a migration command (migrate-legacy) is available.", root)
	}
	if err != nil {
		log.Fatalf("METADATA: cannot open %s: %v", sqlitestore.DBPath(root), err)
	}
	defer metadata.Close()
	blobs, err := blobstore.New(vol)
	if err != nil {
		log.Fatal(err)
	}
	api := httpapi.New(httpapi.Config{Volume: vol, Meta: metadata, Blobs: blobs, Version: version})
	addr := os.Getenv("WITHALBUM_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	if !loopbackOnly(addr) {
		log.Fatalf("SECURITY: refusing non-loopback address %q (authentication has not been implemented)", addr)
	}
	server := &http.Server{Addr: addr, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
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
	log.Printf("EXPERIMENTAL / NO AUTH: listen=%s root=%s volume_id=%s", addr, vol.Root(), vol.ID())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
