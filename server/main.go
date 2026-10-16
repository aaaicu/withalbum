package main

import (
	"context"
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
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
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
		if _, statErr := os.Stat(jsonstore.StatePath(root)); statErr == nil {
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
	blobs, err := blobstore.New(vol)
	if err != nil {
		log.Fatal(err)
	}
	metadata, err := jsonstore.Open(vol)
	if err != nil {
		log.Fatal(err)
	}
	defer metadata.Close()
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
