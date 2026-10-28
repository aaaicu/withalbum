package blobstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentPromoteKeepsPoolFile promotes the same bytes from many
// goroutines at once, round after round, while a watcher checks that the
// pool file never disappears once it is in place and that nothing is
// quarantined. Before Promote was serialized per pool prefix, one that saw
// the pool file missing and then present (another Promote's rename in
// between) took the good file for a damaged one, moved it to orphans and
// left the pool path empty for two directory fsyncs; a concurrent upload
// whose commit re-checked the pool file then failed with 500
// (ContractSQLiteStore/ConcurrentUploads, seen once).
func TestConcurrentPromoteKeepsPoolFile(t *testing.T) {
	s, _ := newTestStore(t)
	const rounds, workers = 30, 16
	for round := 0; round < rounds; round++ {
		body := []byte(fmt.Sprintf("round %d of concurrent promotes", round))
		staged := make([]*StagedFile, workers)
		for i := range staged {
			f, err := s.Stage(bytes.NewReader(body), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			staged[i] = f
		}
		sha := staged[0].SHA256
		pool := s.PoolPath(sha)
		var placed, vanished atomic.Bool
		stop := make(chan struct{})
		watcher := make(chan struct{})
		go func() {
			defer close(watcher)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := os.Lstat(pool)
				if err == nil {
					placed.Store(true)
				} else if placed.Load() {
					vanished.Store(true)
				}
			}
		}()
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		start := make(chan struct{})
		for _, f := range staged {
			wg.Add(1)
			go func(f *StagedFile) {
				defer wg.Done()
				<-start
				if err := s.Promote(f, sha); err != nil {
					errs <- err
				}
				f.Remove()
			}(f)
		}
		close(start)
		wg.Wait()
		close(stop)
		<-watcher
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: promote: %v", round, err)
		}
		if vanished.Load() {
			t.Fatalf("round %d: the pool file disappeared while other uploads of the same bytes were promoted", round)
		}
		if ok, err := fileMatches(pool, sha, int64(len(body))); err != nil || !ok {
			t.Fatalf("round %d: pool file matches=%v err=%v", round, ok, err)
		}
	}
	orphans, err := os.ReadDir(filepath.Join(s.root, filepath.FromSlash(OrphansRelDir)))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("%d good pool file(s) were quarantined as damaged, e.g. %s", len(orphans), orphans[0].Name())
	}
}

// TestPromoteKnownBytesLeavesNoLink: storing known bytes makes and removes
// a second name of the pool file (equal directory work); nothing stays, and
// a name left by a crash is removed at startup, while a look-alike that is
// not a second name of the pool file is left alone and reported.
func TestPromoteKnownBytesLeavesNoLink(t *testing.T) {
	s, _ := newTestStore(t)
	body := []byte("known bytes")
	sha := pooled(t, s, body)
	sha2 := pooled(t, s, body)
	if sha2 != sha {
		t.Fatal("hash changed")
	}
	dir := filepath.Dir(s.PoolPath(sha))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != sha {
		t.Fatalf("prefix directory holds %v", entries)
	}
	// A crash between link and unlink.
	left := filepath.Join(dir, LinkPrefix+sha+"-0011223344556677")
	if err := os.Link(s.PoolPath(sha), left); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	fake := filepath.Join(dir, LinkPrefix+sha+"-8899aabbccddeeff")
	if err := os.WriteFile(fake, body, 0400); err != nil {
		t.Fatal(err)
	}
	r, err := s.Reconcile(t.Context(), fakeIndex{sha: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(left); !os.IsNotExist(err) {
		t.Fatalf("leftover link: %v", err)
	}
	if _, err := os.Lstat(fake); err != nil || r.Unexpected != 1 {
		t.Fatalf("look-alike: %v, unexpected %d", err, r.Unexpected)
	}
	if ok, err := fileMatches(s.PoolPath(sha), sha, int64(len(body))); !ok || err != nil || r.Missing != 0 {
		t.Fatalf("pool file after reconcile: %v %v %+v", ok, err, r)
	}
}
