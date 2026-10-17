package httpapi

import (
	"testing"
	"time"
)

// TestDenialSummary: one row per account, route and status per window; the
// suppressed count travels with the next row; the table stays bounded.
func TestDenialSummary(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	d := newDenialLog(nil)
	d.now = func() time.Time { return now }
	k := denialKey{user: "u", route: "GET /x", status: 403}
	if w, n := d.take(k); !w || n != 0 {
		t.Fatalf("first: %v %d", w, n)
	}
	for range 5 {
		if w, _ := d.take(k); w {
			t.Fatal("repeat written within the window")
		}
	}
	if w, _ := d.take(denialKey{user: "u", route: "GET /x", status: 404}); !w {
		t.Fatal("other status not written")
	}
	now = now.Add(DenialWindow)
	if w, n := d.take(k); !w || n != 5 {
		t.Fatalf("after window: %v %d, want true 5", w, n)
	}
	// Full table: stale entries make room; otherwise new keys are skipped.
	d = newDenialLog(nil)
	d.now = func() time.Time { return now }
	for i := range maxDenialKeys {
		d.take(denialKey{user: "u", route: "r", status: i})
	}
	if w, _ := d.take(denialKey{user: "v", route: "r", status: 1}); w || len(d.seen) != maxDenialKeys {
		t.Fatalf("full table accepted a key: %v %d", w, len(d.seen))
	}
	now = now.Add(DenialWindow)
	if w, _ := d.take(denialKey{user: "v", route: "r", status: 1}); !w || len(d.seen) != 1 {
		t.Fatalf("stale entries not dropped: %v %d", w, len(d.seen))
	}
}
