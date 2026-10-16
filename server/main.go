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
	root  string
	state diskState
}

func newStore(root string) (*store, error) {
	for _, p := range []string{".system/incoming", ".system/pool", ".system/previews"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0700); err != nil {
			return nil, err
		}
	}
	s := &store{root: root, state: diskState{
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
		jsonReply(w, 200, map[string]string{"status": "ok", "version": version})
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
	if !assetExists {
		dest := poolPath(s.root, sha)
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
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
	if err := s.saveLocked(); err != nil {
		delete(s.state.Links[folderID], sha)
		s.state.Events[folderID] = s.state.Events[folderID][:len(s.state.Events[folderID])-1]
		s.state.NextSeq[folderID] = oldSeq
		if !assetExists {
			delete(s.state.Assets, sha)
		}
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
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
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
func main() {
	showVersion := flag.Bool("version", false, "Print WithAlbum Server version and exit")
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
	s, err := newStore(root)
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
	log.Printf("EXPERIMENTAL / NO AUTH: listen=%s root=%s", addr, root)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
