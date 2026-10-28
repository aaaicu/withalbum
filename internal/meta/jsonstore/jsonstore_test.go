package jsonstore_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/storage"
)

func newVolume(t *testing.T) *storage.Volume {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return vol
}

func open(t *testing.T, vol *storage.Volume) *jsonstore.Store {
	t.Helper()
	s, err := jsonstore.Open(vol)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReopenPreservesState(t *testing.T) {
	ctx := context.Background()
	vol := newVolume(t)
	s := open(t, vol)
	f, err := s.CreateFolder(ctx, "family")
	if err != nil {
		t.Fatal(err)
	}
	a := meta.Asset{ID: "ab" + string(bytes.Repeat([]byte("0"), 62)), Filename: "a.jpg", MimeType: "image/jpeg", Bytes: 3}
	res, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, DeviceID: "d", Asset: a})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.ListEvents(ctx, f.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	r := open(t, vol)
	if got, err := r.GetFolder(ctx, f.ID); err != nil || got != f {
		t.Fatalf("folder after reopen %+v %v", got, err)
	}
	if got, err := r.GetAssetBySHA(ctx, a.ID); err != nil || got != res.Asset {
		t.Fatalf("asset after reopen %+v %v", got, err)
	}
	after, err := r.ListEvents(ctx, f.ID, 0, 10)
	if err != nil || len(after.Events) != 1 || after.Events[0] != before.Events[0] || after.NextCursor != before.NextCursor {
		t.Fatalf("events after reopen %+v %v", after, err)
	}
	// The next event continues the sequence.
	a.ID = "cd" + a.ID[2:]
	if _, err := r.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, Asset: a}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.ListEvents(ctx, f.ID, 1, 10); len(p.Events) != 1 || p.Events[0].Seq != 2 {
		t.Fatalf("seq after reopen %+v", p)
	}
}

// TestFailedSaveRollsBack makes state.json unwritable and checks that failed
// writes leave both memory and the file unchanged.
func TestFailedSaveRollsBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ctx := context.Background()
	vol := newVolume(t)
	s := open(t, vol)
	f, err := s.CreateFolder(ctx, "family")
	if err != nil {
		t.Fatal(err)
	}
	statePath := jsonstore.StatePath(vol.Root())
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	system := filepath.Dir(statePath)
	if err := os.Chmod(system, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(system, 0700) })

	a := meta.Asset{ID: "ef" + string(bytes.Repeat([]byte("1"), 62)), Filename: "a.jpg"}
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, Asset: a}); err == nil {
		t.Fatal("CommitUpload succeeded on read-only .system")
	}
	if _, err := s.CreateFolder(ctx, "other"); err == nil || errors.Is(err, meta.ErrIDGeneration) {
		t.Fatalf("CreateFolder err = %v, want a write error", err)
	}
	if _, err := s.GetAssetBySHA(ctx, a.ID); !errors.Is(err, meta.ErrAssetNotFound) {
		t.Fatalf("asset kept after failed save: %v", err)
	}
	if p, err := s.ListEvents(ctx, f.ID, 0, 10); err != nil || len(p.Events) != 0 || p.HasMore {
		t.Fatalf("events after failed save %+v %v", p, err)
	}
	if list, _ := s.ListFolders(ctx); len(list) != 1 {
		t.Fatalf("folders after failed save %v", list)
	}
	if after, _ := os.ReadFile(statePath); !bytes.Equal(before, after) {
		t.Fatal("state.json changed")
	}
	// After the disk recovers, the sequence continues from 1.
	if err := os.Chmod(system, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, Asset: a}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.ListEvents(ctx, f.ID, 0, 10); len(p.Events) != 1 || p.Events[0].Seq != 1 {
		t.Fatalf("events after recovery %+v", p)
	}
}

func TestOpenRejectsCorruptState(t *testing.T) {
	vol := newVolume(t)
	if err := os.Mkdir(filepath.Join(vol.Root(), ".system"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonstore.StatePath(vol.Root()), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := jsonstore.Open(vol); err == nil {
		t.Fatal("Open accepted a corrupt state.json")
	}
}
