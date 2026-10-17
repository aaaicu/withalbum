// Package jsonstore is the frozen PoC metadata store that keeps all metadata
// in memory and rewrites .system/state.json (temp file, fsync, rename) on
// every change. A single mutex serializes all access.
//
// The server never uses it: state.json is only read by the one-time import
// (internal/legacyimport). It remains so that tests can produce state.json
// files exactly as earlier versions wrote them. It has no accounts, owners
// or memberships and therefore no longer is the PoC method of the same name. In
// its data an asset's ID is the SHA-256 of the bytes, and CommitUpload takes
// the hash from CommitRequest.Asset.ID.
package jsonstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// StatePath returns the state file location inside a storage root.
func StatePath(root string) string { return filepath.Join(root, ".system", "state.json") }

type diskState struct {
	Folders map[string]meta.Folder          `json:"folders"`
	Assets  map[string]meta.Asset           `json:"assets"`
	Links   map[string]map[string]meta.Link `json:"links"`
	Events  map[string][]meta.Event         `json:"events"`
	NextSeq map[string]int64                `json:"next_seq"`
}

// Store is the PoC state.json store.
type Store struct {
	mu    sync.Mutex
	path  string
	state diskState
}

// Open loads state.json from a verified volume. It creates only .system inside
// the volume root; a missing state file means an empty store.
func Open(vol *storage.Volume) (*Store, error) {
	if err := vol.Check(); err != nil {
		return nil, err
	}
	if err := storage.MkdirIn(filepath.Join(vol.Root(), ".system")); err != nil {
		return nil, err
	}
	s := &Store{path: StatePath(vol.Root()), state: diskState{
		Folders: map[string]meta.Folder{}, Assets: map[string]meta.Asset{},
		Links: map[string]map[string]meta.Link{}, Events: map[string][]meta.Event{}, NextSeq: map[string]int64{},
	}}
	b, err := os.ReadFile(s.path)
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
		s.state.Folders = map[string]meta.Folder{}
	}
	if s.state.Assets == nil {
		s.state.Assets = map[string]meta.Asset{}
	}
	if s.state.Links == nil {
		s.state.Links = map[string]map[string]meta.Link{}
	}
	if s.state.Events == nil {
		s.state.Events = map[string][]meta.Event{}
	}
	if s.state.NextSeq == nil {
		s.state.NextSeq = map[string]int64{}
	}
	return s, nil
}

// Close is the PoC method of the same name. Every change is already on disk.
func (s *Store) Close() error { return nil }

func (s *Store) saveLocked() error {
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
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
	return os.Rename(tmp.Name(), s.path)
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// CreateFolder is the PoC method of the same name.
func (s *Store) CreateFolder(_ context.Context, name string) (meta.Folder, error) {
	id, err := newID()
	if err != nil {
		return meta.Folder{}, fmt.Errorf("%w: %w", meta.ErrIDGeneration, err)
	}
	folder := meta.Folder{ID: id, Name: name, CreatedAt: now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Folders[id] = folder
	s.state.Links[id] = map[string]meta.Link{}
	if err := s.saveLocked(); err != nil {
		delete(s.state.Folders, id)
		delete(s.state.Links, id)
		return meta.Folder{}, err
	}
	return folder, nil
}

// ListFolders is the PoC method of the same name.
func (s *Store) ListFolders(context.Context) ([]meta.Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]meta.Folder, 0, len(s.state.Folders))
	for _, v := range s.state.Folders {
		result = append(result, v)
	}
	return result, nil
}

// GetFolder is the PoC method of the same name.
func (s *Store) GetFolder(_ context.Context, id string) (meta.Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.state.Folders[id]
	if !ok {
		return meta.Folder{}, meta.ErrFolderNotFound
	}
	return f, nil
}

// GetAssetBySHA is the PoC method of the same name.
func (s *Store) GetAssetBySHA(_ context.Context, sha string) (meta.Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Assets[sha]
	if !ok {
		return meta.Asset{}, meta.ErrAssetNotFound
	}
	return a, nil
}

// CommitUpload is the PoC method of the same name. On a failed save the in-memory
// state is rolled back so memory and state.json stay identical.
func (s *Store) CommitUpload(_ context.Context, req meta.CommitRequest) (meta.CommitResult, error) {
	folderID, sha := req.FolderID, req.Asset.ID
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Folders[folderID]; !exists {
		return meta.CommitResult{}, meta.ErrFolderNotFound
	}
	if link, exists := s.state.Links[folderID][sha]; exists {
		return meta.CommitResult{Result: meta.ResultAlreadyPresent, Asset: s.state.Assets[link.AssetID]}, nil
	}
	if req.BeforeCommit != nil {
		if err := req.BeforeCommit(); err != nil {
			return meta.CommitResult{}, fmt.Errorf("before commit: %w", err)
		}
	}
	ts := now()
	asset, assetExists := s.state.Assets[sha]
	result := meta.ResultLinked
	if !assetExists {
		asset = req.Asset
		asset.UploadedAt = ts
		s.state.Assets[sha] = asset
		result = meta.ResultCreated
	}
	if s.state.Links[folderID] == nil {
		s.state.Links[folderID] = map[string]meta.Link{}
	}
	oldSeq := s.state.NextSeq[folderID]
	s.state.Links[folderID][sha] = meta.Link{AssetID: sha, UploaderDeviceID: req.DeviceID, AddedAt: ts}
	s.state.NextSeq[folderID]++
	s.state.Events[folderID] = append(s.state.Events[folderID], meta.Event{
		Seq: s.state.NextSeq[folderID], Kind: meta.EventAssetAdded, AssetID: sha, FolderID: folderID,
		UploaderDeviceID: req.DeviceID, OccurredAt: ts,
	})
	if err := s.saveLocked(); err != nil {
		delete(s.state.Links[folderID], sha)
		s.state.Events[folderID] = s.state.Events[folderID][:len(s.state.Events[folderID])-1]
		s.state.NextSeq[folderID] = oldSeq
		if !assetExists {
			delete(s.state.Assets, sha)
		}
		return meta.CommitResult{}, err
	}
	return meta.CommitResult{Result: result, Asset: asset}, nil
}

// ListEvents is the PoC method of the same name.
func (s *Store) ListEvents(_ context.Context, folderID string, cursor int64, limit int) (meta.EventPage, error) {
	if limit <= 0 {
		return meta.EventPage{}, meta.ErrInvalidLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Folders[folderID]; !ok {
		return meta.EventPage{}, meta.ErrFolderNotFound
	}
	events := make([]meta.Event, 0, limit)
	for _, e := range s.state.Events[folderID] {
		if e.Seq > cursor {
			events = append(events, e)
			if len(events) == limit {
				break
			}
		}
	}
	next := cursor
	if len(events) != 0 {
		next = events[len(events)-1].Seq
	}
	assets := make(map[string]meta.Asset)
	for _, e := range events {
		assets[e.AssetID] = s.state.Assets[e.AssetID]
	}
	return meta.EventPage{Events: events, Assets: assets, NextCursor: next, HasMore: next < s.state.NextSeq[folderID]}, nil
}
