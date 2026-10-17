package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTest(cfg Config) (*Limiter, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	cfg.Now = c.now
	return New(cfg), c
}

func TestIPBucket(t *testing.T) {
	l, c := newTest(Config{IPBurst: 3, IPInterval: 10 * time.Second})
	for i := range 3 {
		if ok, _ := l.Allow("1.1.1.1", fmt.Sprint("user", i)); !ok {
			t.Fatalf("attempt %d refused", i)
		}
	}
	ok, wait := l.Allow("1.1.1.1", "other")
	if ok || wait < time.Second {
		t.Fatalf("burst exceeded: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("2.2.2.2", "other"); !ok {
		t.Fatal("other IP throttled")
	}
	c.t = c.t.Add(10 * time.Second)
	if ok, _ := l.Allow("1.1.1.1", "other"); !ok {
		t.Fatal("bucket did not refill")
	}
}

func TestAccountFailures(t *testing.T) {
	l, c := newTest(Config{IPBurst: 1000, PairFailures: 3, AccountFailures: 5, Window: time.Minute})
	blocked := false
	for i := range 3 {
		if ok, _ := l.Allow("1.1.1.1", "Alice"); !ok {
			t.Fatalf("attempt %d refused", i)
		}
		blocked = l.Failure("1.1.1.1", "Alice")
	}
	if !blocked {
		t.Fatal("third failure did not report a new block")
	}
	// Case-folded, existing or not: same key.
	if ok, wait := l.Allow("1.1.1.1", "alice"); ok || wait <= 0 || wait > time.Minute {
		t.Fatalf("pair not blocked: %v %v", ok, wait)
	}
	// From other addresses the account is still usable until the global limit.
	if ok, _ := l.Allow("3.3.3.3", "alice"); !ok {
		t.Fatal("other IP blocked by pair limit")
	}
	l.Failure("3.3.3.3", "alice")
	if l.Failure("4.4.4.4", "alice") != true {
		t.Fatal("fifth failure did not block the account")
	}
	if ok, _ := l.Allow("5.5.5.5", "alice"); ok {
		t.Fatal("account not blocked globally")
	}
	if ok, _ := l.Allow("5.5.5.5", "bob"); !ok {
		t.Fatal("other account blocked")
	}
	c.t = c.t.Add(time.Minute)
	if ok, _ := l.Allow("1.1.1.1", "alice"); !ok {
		t.Fatal("block did not expire")
	}
}

func TestSuccessClearsPairOnly(t *testing.T) {
	l, _ := newTest(Config{IPBurst: 1000, PairFailures: 2, AccountFailures: 3, Window: time.Minute})
	l.Failure("1.1.1.1", "alice")
	l.Success("1.1.1.1", "alice")
	l.Failure("1.1.1.1", "alice")
	if ok, _ := l.Allow("1.1.1.1", "alice"); !ok {
		t.Fatal("success did not clear the pair counter")
	}
	l.Failure("1.1.1.1", "alice")
	if ok, _ := l.Allow("9.9.9.9", "alice"); ok {
		t.Fatal("success cleared the account counter")
	}
}

func TestBoundedTables(t *testing.T) {
	l, c := newTest(Config{IPBurst: 1, IPInterval: time.Second, MaxKeys: 2, Window: time.Minute})
	l.Allow("a", "x")
	l.Allow("b", "x")
	if ok, _ := l.Allow("c", "x"); ok {
		t.Fatal("full table accepted a new key")
	}
	c.t = c.t.Add(2 * time.Second)
	if ok, _ := l.Allow("c", "x"); !ok {
		t.Fatal("stale entries not pruned")
	}
	if len(l.ips) > 2 {
		t.Fatalf("%d keys", len(l.ips))
	}
}
