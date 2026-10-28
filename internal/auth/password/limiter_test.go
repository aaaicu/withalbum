package password

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterVerify(t *testing.T) {
	l, err := NewLimiter(1, time.Second, testParams)
	if err != nil {
		t.Fatal(err)
	}
	h, err := l.Hash(context.Background(), "a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct {
		pw, enc string
		want    bool
	}{
		{"a long enough password", h, true},
		{"wrong password here", h, false},
		{"a long enough password", "", false},
		{"a long enough password", "!locked", false},
		{"a long enough password", "$argon2id$garbage", false},
		// The dummy hash's own password must never log in.
		{"withalbum dummy password for timing", "", false},
	} {
		got, err := l.Verify(ctx, c.pw, c.enc)
		if err != nil || got != c.want {
			t.Errorf("Verify(%q, %q) = %v, %v; want %v", c.pw, c.enc, got, err, c.want)
		}
	}
	if _, err := l.Hash(ctx, "short"); !errors.Is(err, ErrTooShort) {
		t.Fatalf("policy not applied: %v", err)
	}
}

func TestLimiterBoundsConcurrency(t *testing.T) {
	l, err := NewLimiter(2, time.Minute, testParams)
	if err != nil {
		t.Fatal(err)
	}
	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := l.acquire(context.Background()); err != nil {
				t.Error(err)
				return
			}
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			running.Add(-1)
			l.release()
		})
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("%d concurrent computations, limit 2", peak.Load())
	}
}

func TestLimiterBusy(t *testing.T) {
	l, err := NewLimiter(1, 20*time.Millisecond, testParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verify(context.Background(), "x", ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("Verify with no free slot: %v", err)
	}
	if _, err := l.Hash(context.Background(), "a long enough password"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Hash with no free slot: %v", err)
	}
	l.release()
	if _, err := l.Verify(context.Background(), "x", ""); err != nil {
		t.Fatal(err)
	}
}
