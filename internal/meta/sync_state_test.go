package meta_test

import (
	"testing"

	"github.com/aaaicu/withalbum/internal/meta"
)

// The app (lib/domain/sync/account_sync.dart indexDigest) pins the same
// vectors: change both together.
func TestDigestBuilderVectors(t *testing.T) {
	if got := meta.NewDigestBuilder().Sum(); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty %s", got)
	}
	b := meta.NewDigestBuilder()
	b.Add("0a", "s0", 0, false, 0)
	b.Add("1b", "s1", 2, true, 3)
	if got := b.Sum(); got != "76153b7b6d21cdebfe3a0d9ecf53c83c3e5194f8eeb53d817c566f37b5b8c0c4" || b.Count() != 2 {
		t.Fatalf("two %s %d", got, b.Count())
	}
}
