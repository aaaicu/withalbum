package sqlitestore

import (
	"testing"

	"github.com/aaaicu/withalbum/internal/authz"
)

// TestReaderCondMatchesAuthz: the SQL that decides whose likes, names and
// mentions are shown agrees with authz.ListAlbumAssets for every relation
// to a folder, including a locked and a disabled account.
func TestReaderCondMatchesAuthz(t *testing.T) {
	e := newMemberEnv(t)
	e.addMember(t, "alice", authz.Contributor)
	e.addMember(t, "bob", authz.Viewer)
	e.addMember(t, "carol", authz.Uploader)
	e.ids["dave"] = addUser(t, e.s, "dave", "USER") // no relation
	e.ids["erin"] = addUser(t, e.s, "erin", "USER")
	e.addMember(t, "erin", authz.Viewer)
	if _, err := e.s.db.Exec("UPDATE users SET status = 'disabled' WHERE user_id = ?", e.ids["erin"]); err != nil {
		t.Fatal(err)
	}
	e.ids["frank"] = addUser(t, e.s, "frank", "SERVER_ADMIN")
	e.addMember(t, "frank", authz.Viewer)
	if _, err := e.s.db.Exec("UPDATE users SET password_hash = '!locked' WHERE user_id = ?", e.ids["frank"]); err != nil {
		t.Fatal(err)
	}
	for login, id := range e.ids {
		fa, err := e.s.GetFolderAccess(t.Context(), e.folder, id)
		if err != nil {
			t.Fatal(err)
		}
		p, err := principalTx(t.Context(), e.s.db, id, "")
		if err != nil {
			t.Fatal(err)
		}
		want := authz.Allowed(p, authz.ListAlbumAssets, &fa.Access)
		var got bool
		if err := e.s.db.QueryRow("SELECT "+readerCond("?1", "?2"), e.folder, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: readerCond %v, authz %v", login, got, want)
		}
		if want != authz.Allowed(p, authz.ReactToAsset, &fa.Access) {
			t.Errorf("%s: ReactToAsset differs from ListAlbumAssets", login)
		}
	}
}
