package httpapi

import (
	"testing"
	"time"
)

func TestNonceStore(t *testing.T) {
	now := time.Now()
	ns := newNonceStore()
	ns.now = func() time.Time { return now }

	a, exp, err := ns.issue("ip-a")
	if err != nil || !exp.Equal(now.Add(nonceTTL)) {
		t.Fatalf("issue: %v %v", exp, err)
	}
	if !ns.take(a) || ns.take(a) {
		t.Fatal("a nonce is good exactly once")
	}
	if ns.take("") || ns.take("never-issued") {
		t.Fatal("unknown nonce accepted")
	}

	// Expiry.
	b, _, _ := ns.issue("ip-a")
	now = now.Add(nonceTTL)
	if ns.take(b) {
		t.Fatal("expired nonce accepted")
	}

	// Per-key bound: the oldest of one address is dropped, others keep theirs.
	other, _, _ := ns.issue("ip-b")
	first, _, _ := ns.issue("ip-a")
	for range noncesPerKey {
		if _, _, err := ns.issue("ip-a"); err != nil {
			t.Fatal(err)
		}
	}
	if ns.take(first) {
		t.Fatal("oldest nonce of a full address kept")
	}
	if !ns.take(other) {
		t.Fatal("another address lost its nonce")
	}
	if n := len(ns.byKey["ip-a"]); n != noncesPerKey {
		t.Fatalf("ip-a holds %d", n)
	}

	// Global bound.
	for i := range noncesTotal + 10 {
		if _, _, err := ns.issue(string(rune('a'+i%26)) + time.Duration(i).String()); err != nil {
			t.Fatal(err)
		}
	}
	if len(ns.exp) > noncesTotal {
		t.Fatalf("%d nonces kept, bound %d", len(ns.exp), noncesTotal)
	}

	// Pruning forgets expired bookkeeping.
	now = now.Add(nonceTTL + time.Second)
	if _, _, err := ns.issue("ip-c"); err != nil {
		t.Fatal(err)
	}
	if len(ns.exp) != 1 || len(ns.order) != 1 || len(ns.byKey) != 1 {
		t.Fatalf("after prune: exp=%d order=%d keys=%d", len(ns.exp), len(ns.order), len(ns.byKey))
	}
}
