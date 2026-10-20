package httpapi_test

import (
	"sync"
	"testing"
	"time"
)

func (e *aclEnv) createFolderKeyed(t *testing.T, login, name, key string) reply {
	t.Helper()
	return e.as(t, login, "POST", "/api/v1/folders", []byte(`{"name":`+mustJSON(t, name)+`}`), map[string]string{idemKey: key})
}

// TestFolderCreateIdempotency: an Idempotency-Key on POST /folders makes a
// retry return the folder the first request created (201,
// Idempotency-Replayed) instead of a second one; the key is scoped to the
// account and the session's device, another name is 422, the replay shows
// the folder as the caller sees it now, and pruned keys are forgotten.
func TestFolderCreateIdempotency(t *testing.T) {
	e := newACLEnv(t)
	folders := func() int { return e.count(t, "SELECT count(*) FROM folders") }
	start := folders()

	first := e.createFolderKeyed(t, "owner", "  Trip  ", "create-1")
	m := wantJSON(t, "first", first, 201)
	wantKeys(t, "folder", m, folderKeys...)
	if first.Header.Get("Idempotency-Replayed") != "" || m["name"] != "Trip" || m["role"] != "OWNER" || m["member_count"] != float64(1) {
		t.Fatalf("first %v %v", first.Header, m)
	}
	id := m["id"].(string)
	again := e.createFolderKeyed(t, "owner", "Trip", "create-1")
	r := wantJSON(t, "retry", again, 201)
	if again.Header.Get("Idempotency-Replayed") != "true" || mustJSON(t, r) != mustJSON(t, m) {
		t.Fatalf("retry %v %v, want %v", again.Header, r, m)
	}
	if folders() != start+1 {
		t.Fatalf("folders %d, want %d", folders(), start+1)
	}

	wantError(t, "other name", e.createFolderKeyed(t, "owner", "Other", "create-1"), 422, "idempotency_key_reused")
	wantError(t, "bad key", e.createFolderKeyed(t, "owner", "Trip", "no spaces allowed"), 400, "invalid_idempotency_key")
	wantError(t, "long key", e.createFolderKeyed(t, "owner", "Trip", string(make([]byte, 129))), 400, "invalid_idempotency_key")
	// The name is still checked first on a retry.
	wantError(t, "bad name", e.createFolderKeyed(t, "owner", " ", "create-1"), 400, "invalid_folder_name")

	// Another device of the same account, and another account, have their
	// own keys.
	tok := loginToken(t, e.Raw, "owner", userPassword)
	other := do(t, e.Raw, "POST", "/api/v1/folders", []byte(`{"name":"Trip"}`), map[string]string{"Authorization": "Bearer " + tok, idemKey: "create-1"})
	if o := wantJSON(t, "other device", other, 201); o["id"] == id || other.Header.Get("Idempotency-Replayed") != "" {
		t.Fatalf("other device replayed %v", o)
	}
	if o := wantJSON(t, "admin", e.createFolderKeyed(t, "root", "Trip", "create-1"), 201); o["id"] == id {
		t.Fatal("another account got the owner's folder")
	}
	// Without a key, every request creates a folder (as before).
	n := folders()
	wantJSON(t, "plain", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"Trip"}`), nil), 201)
	wantJSON(t, "plain", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"Trip"}`), nil), 201)
	if folders() != n+2 {
		t.Fatalf("plain creates: %d folders, want %d", folders(), n+2)
	}

	// A retry is authorized like a new request.
	wantError(t, "no permission", e.createFolderKeyed(t, "contributor", "Trip", "create-1"), 403, "forbidden")

	// The replay is the folder as the caller sees it now: after the owner
	// handed it to a member, the old owner is a CONTRIBUTOR of two.
	if _, err := e.Meta.DB().Exec("INSERT INTO folder_members (folder_id, user_id, role) VALUES (?, ?, 'CONTRIBUTOR')", id, e.ids["viewer"]); err != nil {
		t.Fatal(err)
	}
	wantJSON(t, "transfer", e.as(t, "owner", "POST", "/api/v1/folders/"+id+"/owner", []byte(`{"new_owner_user_id":`+mustJSON(t, e.ids["viewer"])+`}`), map[string]string{"Content-Type": "application/json"}), 200)
	again = e.createFolderKeyed(t, "owner", "Trip", "create-1")
	if r := wantJSON(t, "replay after transfer", again, 201); r["id"] != id || r["role"] != "CONTRIBUTOR" || r["member_count"] != float64(2) {
		t.Fatalf("replay after transfer %v", r)
	}
	// Archived: the folder is gone for everyone; the key still names it.
	wantStatusReply(t, "archive", e.as(t, "viewer", "DELETE", "/api/v1/folders/"+id, nil, nil), 204)
	wantError(t, "replay archived", e.createFolderKeyed(t, "owner", "Trip", "create-1"), 404, "folder_not_found")
	if folders() != n+2 {
		t.Fatal("a replay created a folder")
	}

	// Pruned receipts are forgotten: the key creates a new folder.
	if _, err := e.Meta.PruneUploadReceipts(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if e.count(t, "SELECT count(*) FROM folder_create_receipts") != 0 {
		t.Fatal("folder receipts left after pruning")
	}
	if r := wantJSON(t, "after prune", e.createFolderKeyed(t, "owner", "Trip", "create-1"), 201); r["id"] == id {
		t.Fatal("pruned key replayed")
	}
}

// TestFolderCreateIdempotencyConcurrent: simultaneous requests with one
// key create one folder; all get it, exactly one as the original.
func TestFolderCreateIdempotencyConcurrent(t *testing.T) {
	e := newACLEnv(t)
	start := e.count(t, "SELECT count(*) FROM folders")
	const n = 12
	replies := make([]reply, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			replies[i] = do(t, e.Raw, "POST", "/api/v1/folders", []byte(`{"name":"Race"}`),
				map[string]string{"Authorization": "Bearer " + e.tokens["owner"], idemKey: "race-key"})
		})
	}
	wg.Wait()
	ids := map[string]bool{}
	originals := 0
	for i, r := range replies {
		m := wantJSON(t, "concurrent create", r, 201)
		ids[m["id"].(string)] = true
		if r.Header.Get("Idempotency-Replayed") == "" {
			originals++
		}
		if m["role"] != "OWNER" || m["name"] != "Race" {
			t.Fatalf("reply %d %v", i, m)
		}
	}
	if len(ids) != 1 || originals != 1 {
		t.Fatalf("folder ids %v, %d originals; want one of each", ids, originals)
	}
	if got := e.count(t, "SELECT count(*) FROM folders"); got != start+1 {
		t.Fatalf("folders %d, want %d", got, start+1)
	}
}
