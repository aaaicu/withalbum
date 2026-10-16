package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aaaicu/withalbum/internal/storage"
)

// This is a LAN/localhost demonstration server, NOT a production service.
// In particular, it intentionally has NO AUTHENTICATION. Never publish its port.
var _ = gif.GIF{}
var _ = png.Encoder{}

const maxUploadSize int64 = 512 << 20

// Set with -ldflags "-X main.version=<version>" by the release builder.
var version = "dev"

//go:embed web/index.html
var dashboardHTML []byte

// Keep this prototype strictly on the local machine until authentication, ACLs,
// rate limiting and TLS/reverse-proxy security have been implemented.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

type Folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}
type Asset struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mime_type"`
	Bytes        int64  `json:"bytes"`
	PreviewReady bool   `json:"preview_ready"`
	UploadedAt   string `json:"uploaded_at"`
}
type Link struct {
	AssetID          string `json:"asset_id"`
	UploaderDeviceID string `json:"uploader_device_id"`
	AddedAt          string `json:"added_at"`
}
type Event struct {
	Seq              int64  `json:"seq"`
	Kind             string `json:"kind"`
	AssetID          string `json:"asset_id"`
	FolderID         string `json:"folder_id"`
	UploaderDeviceID string `json:"uploader_device_id"`
	OccurredAt       string `json:"occurred_at"`
}
type diskState struct {
	Folders map[string]Folder          `json:"folders"`
	Assets  map[string]Asset           `json:"assets"`
	Links   map[string]map[string]Link `json:"links"`
	Events  map[string][]Event         `json:"events"`
	NextSeq map[string]int64           `json:"next_seq"`
}
type store struct {
	mu    sync.Mutex
	vol   *storage.Volume
	root  string
	state diskState
}

// mkdirIn creates dir with os.Mkdir (never MkdirAll), so a missing parent such
// as an unmounted volume root fails instead of being recreated.
func mkdirIn(dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

// newStore uses a verified volume and only creates .system/* inside it.
func newStore(vol *storage.Volume) (*store, error) {
	if err := vol.Check(); err != nil {
		return nil, err
	}
	root := vol.Root()
	for _, p := range []string{".system", ".system/incoming", ".system/pool", ".system/previews"} {
		if err := mkdirIn(filepath.Join(root, p)); err != nil {
			return nil, err
		}
	}
	s := &store{vol: vol, root: root, state: diskState{
		Folders: map[string]Folder{}, Assets: map[string]Asset{},
		Links: map[string]map[string]Link{}, Events: map[string][]Event{}, NextSeq: map[string]int64{},
	}}
	b, err := os.ReadFile(filepath.Join(root, ".system", "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	if s.state.Folders == nil {
		s.state.Folders = map[string]Folder{}
	}
	if s.state.Assets == nil {
		s.state.Assets = map[string]Asset{}
	}
	if s.state.Links == nil {
		s.state.Links = map[string]map[string]Link{}
	}
	if s.state.Events == nil {
		s.state.Events = map[string][]Event{}
	}
	if s.state.NextSeq == nil {
		s.state.NextSeq = map[string]int64{}
	}
	return s, nil
}
func (s *store) saveLocked() error {
	path := filepath.Join(s.root, ".system", "state.json")
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.state); err != nil {
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
	return os.Rename(tmp.Name(), path)
}
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func jsonReply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func errorReply(w http.ResponseWriter, status int, code string) {
	jsonReply(w, status, map[string]string{"error": code})
}
func (s *store) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'")
		_, _ = w.Write(dashboardHTML)
	})
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		// Report only ok/unavailable; never expose paths or volume ids here.
		storageState := "ok"
		if err := s.vol.Check(); err != nil {
			storageState = "unavailable"
		}
		jsonReply(w, 200, map[string]string{"status": "ok", "version": version, "storage": storageState})
	})
	mux.HandleFunc("GET /api/v1/folders", s.getFolders)
	mux.HandleFunc("POST /api/v1/folders", s.createFolder)
	mux.HandleFunc("POST /api/v1/folders/{folderID}/assets", s.upload)
	mux.HandleFunc("GET /api/v1/folders/{folderID}/sync", s.syncEvents)
	mux.HandleFunc("GET /api/v1/assets/{assetID}/original", s.download(false))
	mux.HandleFunc("GET /api/v1/assets/{assetID}/preview", s.download(true))
	return mux
}
func (s *store) getFolders(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Folder, 0, len(s.state.Folders))
	for _, v := range s.state.Folders {
		result = append(result, v)
	}
	jsonReply(w, 200, map[string]any{"folders": result})
}
func (s *store) createFolder(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&request); err != nil {
		errorReply(w, 400, "invalid_json")
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if len(request.Name) == 0 || len([]rune(request.Name)) > 80 {
		errorReply(w, 400, "invalid_folder_name")
		return
	}
	id, err := newID()
	if err != nil {
		errorReply(w, 500, "random_failure")
		return
	}
	folder := Folder{ID: id, Name: request.Name, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.storageReady(w) {
		return
	}
	s.state.Folders[id] = folder
	s.state.Links[id] = map[string]Link{}
	if err := s.saveLocked(); err != nil {
		delete(s.state.Folders, id)
		delete(s.state.Links, id)
		errorReply(w, 500, "metadata_write_failed")
		return
	}
	jsonReply(w, 201, folder)
}

// storageReady re-verifies the volume right before a write. On failure it
// replies 503 storage_unavailable and the caller must not write anything.
func (s *store) storageReady(w http.ResponseWriter) bool {
	if err := s.vol.Check(); err != nil {
		log.Printf("storage unavailable, refusing write: %v", err)
		errorReply(w, 503, "storage_unavailable")
		return false
	}
	return true
}
func poolPath(root, sha string) string { return filepath.Join(root, ".system", "pool", sha[:2], sha) }
func previewPath(root, sha string) string {
	return filepath.Join(root, ".system", "previews", sha[:2], sha+".jpg")
}
func (s *store) upload(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("folderID")
	s.mu.Lock()
	_, found := s.state.Folders[folderID]
	s.mu.Unlock()
	if !found {
		errorReply(w, 404, "folder_not_found")
		return
	}
	// Fail fast before spooling the body to a volume that is already gone.
	if !s.storageReady(w) {
		return
	}

	temp, err := os.CreateTemp(filepath.Join(s.root, ".system", "incoming"), "upload-*")
	if err != nil {
		errorReply(w, 500, "temp_failed")
		return
	}
	defer os.Remove(temp.Name())
	h := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(temp, h), io.LimitReader(r.Body, maxUploadSize+1))
	if copyErr != nil {
		temp.Close()
		errorReply(w, 400, "upload_failed")
		return
	}
	if count > maxUploadSize {
		temp.Close()
		errorReply(w, 413, "file_too_large")
		return
	}
	if count == 0 {
		temp.Close()
		errorReply(w, 400, "empty_file")
		return
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		errorReply(w, 500, "file_sync_failed")
		return
	}
	if err := temp.Close(); err != nil {
		errorReply(w, 500, "temp_close_failed")
		return
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if declared := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Content-SHA256"))); declared != "" && declared != sha {
		errorReply(w, 422, "hash_mismatch")
		return
	}
	filename := r.Header.Get("X-Filename")
	// The experimental local dashboard percent-encodes non-ASCII filenames for headers.
	if decoded, err := url.QueryUnescape(filename); err == nil {
		filename = decoded
	}
	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if name == "" || name == "." || name == "/" || len(name) > 240 {
		name = sha
	}
	fp, err := os.Open(temp.Name())
	if err != nil {
		errorReply(w, 500, "temp_read_failed")
		return
	}
	sniff := make([]byte, 512)
	n, _ := fp.Read(sniff)
	fp.Close()
	mime := http.DetectContentType(sniff[:n])
	deviceID := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if len(deviceID) > 100 {
		errorReply(w, 400, "invalid_device_id")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Folders[folderID]; !exists {
		errorReply(w, 404, "folder_not_found")
		return
	}
	if link, exists := s.state.Links[folderID][sha]; exists {
		jsonReply(w, 200, map[string]any{"result": "already_present", "asset_id": sha, "folder_id": folderID, "sha256": sha, "preview_ready": s.state.Assets[link.AssetID].PreviewReady})
		return
	}
	asset, assetExists := s.state.Assets[sha]
	result := "linked"
	if !s.storageReady(w) {
		return
	}
	if !assetExists {
		dest := poolPath(s.root, sha)
		if err := mkdirIn(filepath.Dir(dest)); err != nil {
			errorReply(w, 500, "storage_failed")
			return
		}
		if err := os.Rename(temp.Name(), dest); err != nil {
			errorReply(w, 500, "storage_failed")
			return
		}
		_ = os.Chmod(dest, 0400)
		previewReady := makeJPEGPreview(dest, previewPath(s.root, sha)) == nil
		asset = Asset{ID: sha, Filename: name, MimeType: mime, Bytes: count, PreviewReady: previewReady, UploadedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		s.state.Assets[sha] = asset
		result = "created"
	}
	oldSeq := s.state.NextSeq[folderID]
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s.state.Links[folderID][sha] = Link{AssetID: sha, UploaderDeviceID: deviceID, AddedAt: now}
	s.state.NextSeq[folderID]++
	s.state.Events[folderID] = append(s.state.Events[folderID], Event{
		Seq: s.state.NextSeq[folderID], Kind: "asset_added", AssetID: sha, FolderID: folderID,
		UploaderDeviceID: deviceID, OccurredAt: now,
	})
	rollback := func() {
		delete(s.state.Links[folderID], sha)
		s.state.Events[folderID] = s.state.Events[folderID][:len(s.state.Events[folderID])-1]
		s.state.NextSeq[folderID] = oldSeq
		if !assetExists {
			delete(s.state.Assets, sha)
		}
	}
	// Re-check: the pool rename and preview above may have taken a while.
	if !s.storageReady(w) {
		rollback()
		return
	}
	if err := s.saveLocked(); err != nil {
		rollback()
		errorReply(w, 500, "metadata_write_failed")
		return
	}
	jsonReply(w, 201, map[string]any{"result": result, "asset_id": sha, "folder_id": folderID, "sha256": sha, "preview_ready": asset.PreviewReady})
}
func makeJPEGPreview(src, dst string) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()
	cfg, _, err := image.DecodeConfig(file)
	if err != nil {
		return err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return errors.New("unsupported dimensions")
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	original, _, err := image.Decode(file)
	if err != nil {
		return err
	}
	bounds := original.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	const maxSide = 1600
	newW, newH := w, h
	if w >= h && w > maxSide {
		newW = maxSide
		newH = max(1, h*maxSide/w)
	}
	if h > w && h > maxSide {
		newH = maxSide
		newW = max(1, w*maxSide/h)
	}
	small := image.NewRGBA(image.Rect(0, 0, newW, newH))
	// Simple nearest-neighbor scaler. Replace with a proper media pipeline in production.
	for y := 0; y < newH; y++ {
		for x := 0; x < newW; x++ {
			small.Set(x, y, original.At(bounds.Min.X+x*w/newW, bounds.Min.Y+y*h/newH))
		}
	}
	if err := mkdirIn(filepath.Dir(dst)); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), "preview-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err := jpeg.Encode(out, small, &jpeg.Options{Quality: 78}); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}
func (s *store) syncEvents(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("folderID")
	cursor, err := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	if r.URL.Query().Get("cursor") == "" {
		cursor = 0
		err = nil
	}
	if err != nil || cursor < 0 {
		errorReply(w, 400, "invalid_cursor")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Folders[folderID]; !ok {
		errorReply(w, 404, "folder_not_found")
		return
	}
	const pageSize = 100
	events := make([]Event, 0, pageSize)
	for _, e := range s.state.Events[folderID] {
		if e.Seq > cursor {
			events = append(events, e)
			if len(events) == pageSize {
				break
			}
		}
	}
	next := cursor
	if len(events) != 0 {
		next = events[len(events)-1].Seq
	}
	assets := make(map[string]Asset)
	for _, e := range events {
		assets[e.AssetID] = s.state.Assets[e.AssetID]
	}
	jsonReply(w, 200, map[string]any{"folder_id": folderID, "events": events, "assets": assets,
		"next_cursor": next, "has_more": next < s.state.NextSeq[folderID]})
}
func (s *store) download(preview bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("assetID")
		if len(sha) != 64 {
			errorReply(w, 400, "invalid_asset_id")
			return
		}
		if _, err := hex.DecodeString(sha); err != nil {
			errorReply(w, 400, "invalid_asset_id")
			return
		}
		s.mu.Lock()
		asset, found := s.state.Assets[sha]
		s.mu.Unlock()
		if !found {
			errorReply(w, 404, "asset_not_found")
			return
		}
		path := poolPath(s.root, sha)
		if preview {
			if !asset.PreviewReady {
				errorReply(w, 404, "preview_not_ready")
				return
			}
			path = previewPath(s.root, sha)
			w.Header().Set("Content-Type", "image/jpeg")
		} else {
			w.Header().Set("Content-Type", asset.MimeType)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, path)
	}
}

// openStorageHint explains a storage.Open failure in operator terms.
func openStorageHint(root string, err error) string {
	switch {
	case errors.Is(err, storage.ErrRootMissing):
		return fmt.Sprintf("storage root %q does not exist. If it is on an external disk, check that the disk is mounted. The server never creates the storage root.", root)
	case errors.Is(err, storage.ErrNotDirectory):
		return fmt.Sprintf("storage root %q is not a directory.", root)
	case errors.Is(err, storage.ErrNotInitialized):
		if _, statErr := os.Stat(filepath.Join(root, ".system", "state.json")); statErr == nil {
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
	s, err := newStore(vol)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("WITHALBUM_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	if !loopbackOnly(addr) {
		log.Fatalf("SECURITY: refusing non-loopback address %q (authentication has not been implemented)", addr)
	}
	server := &http.Server{Addr: addr, Handler: s.mux(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
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
