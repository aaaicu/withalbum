package sqlitestore

import (
	"errors"
	"sync"
	"testing"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

var errNotAllowed = errors.New("not allowed")

// allow decides like the HTTP API does: authz.Allowed on what the store
// read inside the transaction.
func allow(action authz.Action) meta.Authorizer {
	return func(p authz.Principal, a authz.AlbumAccess) error {
		if authz.Allowed(p, action, &a) {
			return nil
		}
		return errNotAllowed
	}
}

func active(p authz.Principal, _ authz.AlbumAccess) error {
	if !p.Active {
		return errNotAllowed
	}
	return nil
}

type memberEnv struct {
	s      *Store
	ids    map[string]string
	folder string
}

func newMemberEnv(t *testing.T) *memberEnv {
	t.Helper()
	s := open(t, newVolume(t))
	t.Cleanup(func() { s.Close() })
	e := &memberEnv{s: s, ids: map[string]string{}}
	for _, login := range []string{"owner", "alice", "bob", "carol"} {
		e.ids[login] = addUser(t, s, login, "USER")
	}
	f, err := s.CreateFolder(t.Context(), meta.NewFolder{Name: "family", OwnerID: e.ids["owner"]})
	if err != nil {
		t.Fatal(err)
	}
	e.folder = f.ID
	return e
}

func (e *memberEnv) by(actor string, action authz.Action, target string, role authz.AlbumRole) meta.AlbumChange {
	return meta.AlbumChange{FolderID: e.folder, ActorID: e.ids[actor], TargetID: target, Role: role, Authorize: allow(action)}
}

// addMember: the original owner invites login, who accepts.
func (e *memberEnv) addMember(t *testing.T, login string, role authz.AlbumRole) {
	t.Helper()
	e.addMemberBy(t, "owner", login, role)
}

func (e *memberEnv) addMemberBy(t *testing.T, owner, login string, role authz.AlbumRole) {
	t.Helper()
	inv, err := e.s.CreateInvitation(t.Context(), e.by(owner, authz.InviteMember, e.ids[login], role))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AcceptInvitation(t.Context(), meta.AlbumChange{ActorID: e.ids[login], TargetID: inv.ID, Authorize: active}); err != nil {
		t.Fatal(err)
	}
}

// checkOwner asserts the folder's owner and that no owner is also a member.
func (e *memberEnv) checkOwner(t *testing.T, want string) {
	t.Helper()
	if n := count(t, e.s, "SELECT count(*) FROM folders WHERE folder_id = ? AND owner_user_id = ?", e.folder, e.ids[want]); n != 1 {
		t.Fatalf("owner is not %s", want)
	}
	if n := count(t, e.s, "SELECT count(*) FROM folder_members m JOIN folders f ON f.folder_id = m.folder_id WHERE m.user_id = f.owner_user_id"); n != 0 {
		t.Fatalf("%d owners are also members", n)
	}
}

func (e *memberEnv) role(t *testing.T, login string) string {
	t.Helper()
	var role string
	err := e.s.db.QueryRow("SELECT role FROM folder_members WHERE folder_id = ? AND user_id = ?", e.folder, e.ids[login]).Scan(&role)
	if err != nil {
		return ""
	}
	return role
}

func (e *memberEnv) audits(t *testing.T, action string) int {
	t.Helper()
	return count(t, e.s, "SELECT count(*) FROM audit_log WHERE action = ?", action)
}

func TestInvitationLifecycle(t *testing.T) {
	e := newMemberEnv(t)
	ctx := t.Context()
	inv, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["alice"], authz.Viewer))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.ID) != 32 || inv.Status != meta.InvitationPending || inv.FolderName != "family" || inv.InviterID != e.ids["owner"] || inv.Role != authz.Viewer {
		t.Fatalf("invitation %+v", inv)
	}
	if n := count(t, e.s, "SELECT count(*) FROM folder_invitations WHERE invitation_id = ? AND token_sha256 IS NULL", inv.ID); n != 1 {
		t.Fatal("invitation row missing or carries a token")
	}
	for name, c := range map[string]struct {
		change meta.AlbumChange
		want   error
	}{
		"duplicate pending": {e.by("owner", authz.InviteMember, e.ids["alice"], authz.Contributor), meta.ErrInvitationPending},
		"owner":             {e.by("owner", authz.InviteMember, e.ids["owner"], authz.Viewer), meta.ErrAlreadyMember},
		"unknown":           {e.by("owner", authz.InviteMember, "0123456789abcdef0123456789abcdef", authz.Viewer), meta.ErrInviteeNotFound},
		"locked bootstrap":  {e.by("owner", authz.InviteMember, e.s.BootstrapOwnerID(), authz.Viewer), meta.ErrInviteeNotFound},
		"bad role":          {e.by("owner", authz.InviteMember, e.ids["bob"], "OWNER"), meta.ErrInvalidRole},
		"not owner":         {e.by("bob", authz.InviteMember, e.ids["carol"], authz.Viewer), errNotAllowed},
	} {
		if _, err := e.s.CreateInvitation(ctx, c.change); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := e.s.db.Exec("UPDATE users SET status = 'disabled' WHERE user_id = ?", e.ids["carol"]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["carol"], authz.Viewer)); !errors.Is(err, meta.ErrInviteeNotFound) {
		t.Fatalf("disabled invitee: %v", err)
	}
	if mine, _ := e.s.ListInvitationsFor(ctx, e.ids["alice"]); len(mine) != 1 || mine[0].ID != inv.ID {
		t.Fatalf("alice's invitations %+v", mine)
	}
	if other, _ := e.s.ListInvitationsFor(ctx, e.ids["bob"]); len(other) != 0 {
		t.Fatalf("bob sees %+v", other)
	}
	// Before accepting, alice has no relation to the folder.
	if fa, _ := e.s.GetFolderAccess(ctx, e.folder, e.ids["alice"]); fa.Access.HasMembership {
		t.Fatal("membership before acceptance")
	}
	if _, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["bob"], TargetID: inv.ID, Authorize: active}); !errors.Is(err, meta.ErrNotInvitee) {
		t.Fatalf("accepted by another account: %v", err)
	}
	fa, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["alice"], TargetID: inv.ID, Authorize: active})
	if err != nil || !fa.Access.HasMembership || fa.Access.MemberRole != authz.Viewer {
		t.Fatalf("accept: %+v %v", fa, err)
	}
	if e.role(t, "alice") != "VIEWER" {
		t.Fatal("membership not stored")
	}
	if _, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["alice"], TargetID: inv.ID, Authorize: active}); !errors.Is(err, meta.ErrInvitationGone) {
		t.Fatalf("second acceptance: %v", err)
	}
	if _, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["alice"], authz.Viewer)); !errors.Is(err, meta.ErrAlreadyMember) {
		t.Fatalf("invite a member: %v", err)
	}
	// Revocation.
	inv2, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["bob"], authz.Uploader))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.RevokeInvitation(ctx, meta.AlbumChange{FolderID: "elsewhere", ActorID: e.ids["owner"], TargetID: inv2.ID, Authorize: allow(authz.InviteMember)}); !errors.Is(err, meta.ErrFolderNotFound) {
		t.Fatalf("revoke through another folder: %v", err)
	}
	if err := e.s.RevokeInvitation(ctx, e.by("owner", authz.InviteMember, inv2.ID, "")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RevokeInvitation(ctx, e.by("owner", authz.InviteMember, inv2.ID, "")); !errors.Is(err, meta.ErrInvitationGone) {
		t.Fatalf("second revocation: %v", err)
	}
	if _, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["bob"], TargetID: inv2.ID, Authorize: active}); !errors.Is(err, meta.ErrInvitationGone) {
		t.Fatalf("accept revoked: %v", err)
	}
	if _, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["bob"], TargetID: "nope", Authorize: active}); !errors.Is(err, meta.ErrInvitationNotFound) {
		t.Fatalf("accept unknown: %v", err)
	}
	for action, want := range map[string]int{"invitation.create": 2, "invitation.accept": 1, "invitation.revoke": 1} {
		if n := e.audits(t, action); n != want {
			t.Fatalf("%d %s audit rows, want %d", n, action, want)
		}
	}
	if n := count(t, e.s, "SELECT count(*) FROM audit_log WHERE action = 'invitation.accept' AND actor_user_id = ? AND target_id = ?", e.ids["alice"], inv.ID); n != 1 {
		t.Fatal("accept audit row does not name the invitee and the invitation")
	}
}

func TestInvitationExpiry(t *testing.T) {
	e := newMemberEnv(t)
	ctx := t.Context()
	inv, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["alice"], authz.Viewer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec("UPDATE folder_invitations SET expires_at = ? WHERE invitation_id = ?", "2020-01-01T00:00:00.000000000Z", inv.ID); err != nil {
		t.Fatal(err)
	}
	if mine, _ := e.s.ListInvitationsFor(ctx, e.ids["alice"]); len(mine) != 0 {
		t.Fatalf("expired invitation listed: %+v", mine)
	}
	if _, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["alice"], TargetID: inv.ID, Authorize: active}); !errors.Is(err, meta.ErrInvitationGone) {
		t.Fatalf("accept expired: %v", err)
	}
	if e.role(t, "alice") != "" {
		t.Fatal("expired invitation made a member")
	}
	// A new invitation replaces the expired one.
	inv2, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["alice"], authz.Contributor))
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, e.s, "SELECT count(*) FROM folder_invitations WHERE invitation_id = ? AND status = 'expired'", inv.ID); n != 1 {
		t.Fatal("old invitation not marked expired")
	}
	if _, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["alice"], TargetID: inv2.ID, Authorize: active}); err != nil {
		t.Fatal(err)
	}
	// Unparsable expiry fails closed.
	inv3, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["bob"], authz.Viewer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec("UPDATE folder_invitations SET expires_at = 'garbage' WHERE invitation_id = ?", inv3.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["bob"], TargetID: inv3.ID, Authorize: active}); !errors.Is(err, meta.ErrInvitationGone) {
		t.Fatalf("accept with unparsable expiry: %v", err)
	}
}

func TestRoleChangeAndRemoval(t *testing.T) {
	e := newMemberEnv(t)
	ctx := t.Context()
	e.addMember(t, "alice", authz.Contributor)
	m, err := e.s.SetMemberRole(ctx, e.by("owner", authz.ChangeMemberRole, e.ids["alice"], authz.Uploader))
	if err != nil || m.Role != "UPLOADER" || e.role(t, "alice") != "UPLOADER" {
		t.Fatalf("role change: %+v %v", m, err)
	}
	if _, err := e.s.SetMemberRole(ctx, e.by("owner", authz.ChangeMemberRole, e.ids["alice"], authz.Uploader)); err != nil {
		t.Fatal(err)
	}
	if n := e.audits(t, "member.role_change"); n != 1 {
		t.Fatalf("%d role_change rows (unchanged role must not be recorded)", n)
	}
	for name, c := range map[string]struct {
		change meta.AlbumChange
		want   error
	}{
		"owner":      {e.by("owner", authz.ChangeMemberRole, e.ids["owner"], authz.Viewer), meta.ErrTargetIsOwner},
		"non-member": {e.by("owner", authz.ChangeMemberRole, e.ids["bob"], authz.Viewer), meta.ErrMemberNotFound},
		"bad role":   {e.by("owner", authz.ChangeMemberRole, e.ids["alice"], "ADMIN"), meta.ErrInvalidRole},
		"by member":  {e.by("alice", authz.ChangeMemberRole, e.ids["alice"], authz.Contributor), errNotAllowed},
	} {
		if _, err := e.s.SetMemberRole(ctx, c.change); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", name, err, c.want)
		}
	}
	if err := e.s.RemoveMember(ctx, e.by("owner", authz.RevokeMember, e.ids["owner"], "")); !errors.Is(err, meta.ErrTargetIsOwner) {
		t.Fatalf("remove owner: %v", err)
	}
	if err := e.s.RemoveMember(ctx, e.by("owner", authz.RevokeMember, e.ids["alice"], "")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RemoveMember(ctx, e.by("owner", authz.RevokeMember, e.ids["alice"], "")); !errors.Is(err, meta.ErrMemberNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	if fa, _ := e.s.GetFolderAccess(ctx, e.folder, e.ids["alice"]); fa.Access.HasMembership {
		t.Fatal("still a member")
	}
	if e.audits(t, "member.remove") != 1 {
		t.Fatal("removal not audited")
	}
}

func TestTransferOwnership(t *testing.T) {
	e := newMemberEnv(t)
	ctx := t.Context()
	e.addMember(t, "alice", authz.Viewer)
	e.addMember(t, "carol", authz.Uploader)
	pending, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["bob"], authz.Viewer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec("UPDATE users SET status = 'disabled' WHERE user_id = ?", e.ids["carol"]); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		change meta.AlbumChange
		want   error
	}{
		"self":            {e.by("owner", authz.TransferOwner, e.ids["owner"], ""), meta.ErrTargetIsOwner},
		"non-member":      {e.by("owner", authz.TransferOwner, e.ids["bob"], ""), meta.ErrMemberNotFound},
		"disabled member": {e.by("owner", authz.TransferOwner, e.ids["carol"], ""), meta.ErrNewOwnerInactive},
		"by member":       {e.by("alice", authz.TransferOwner, e.ids["alice"], ""), errNotAllowed},
	} {
		if err := e.s.TransferOwnership(ctx, c.change); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", name, err, c.want)
		}
		e.checkOwner(t, "owner")
	}
	if err := e.s.TransferOwnership(ctx, e.by("owner", authz.TransferOwner, e.ids["alice"], "")); err != nil {
		t.Fatal(err)
	}
	e.checkOwner(t, "alice")
	if e.role(t, "owner") != "CONTRIBUTOR" || e.role(t, "alice") != "" {
		t.Fatalf("after transfer: previous owner %q, new owner membership %q", e.role(t, "owner"), e.role(t, "alice"))
	}
	if n := count(t, e.s, "SELECT count(*) FROM folder_invitations WHERE invitation_id = ? AND status = 'revoked'", pending.ID); n != 1 {
		t.Fatal("pending invitation of the previous owner not revoked")
	}
	if n := count(t, e.s, `SELECT count(*) FROM audit_log WHERE action = 'folder.transfer_owner' AND actor_user_id = ? AND target_id = ?
		AND json_extract(details_json, '$.new_owner_id') = ? AND json_extract(details_json, '$.invitations_revoked') = 1`, e.ids["owner"], e.folder, e.ids["alice"]); n != 1 {
		t.Fatal("transfer not audited")
	}
	// The previous owner lost every owner right at once.
	if err := e.s.TransferOwnership(ctx, e.by("owner", authz.TransferOwner, e.ids["owner"], "")); !errors.Is(err, errNotAllowed) {
		t.Fatalf("previous owner transfers again: %v", err)
	}
	if _, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["bob"], authz.Viewer)); !errors.Is(err, errNotAllowed) {
		t.Fatalf("previous owner invites: %v", err)
	}
	// The new owner manages the folder, including the way back.
	if _, err := e.s.SetMemberRole(ctx, e.by("alice", authz.ChangeMemberRole, e.ids["owner"], authz.Viewer)); err != nil {
		t.Fatal(err)
	}
	if err := e.s.TransferOwnership(ctx, e.by("alice", authz.TransferOwner, e.ids["owner"], "")); err != nil {
		t.Fatal(err)
	}
	e.checkOwner(t, "owner")
	if e.role(t, "alice") != "CONTRIBUTOR" {
		t.Fatal("second transfer: previous owner not CONTRIBUTOR")
	}
}

// TestTransferRollsBack: a failure after some of the steps leaves the folder
// exactly as it was, with no audit row.
func TestTransferRollsBack(t *testing.T) {
	e := newMemberEnv(t)
	ctx := t.Context()
	e.addMember(t, "alice", authz.Viewer)
	inv, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["bob"], authz.Viewer))
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string) {
		t.Helper()
		e.checkOwner(t, "owner")
		if e.role(t, "alice") != "VIEWER" || e.role(t, "owner") != "" {
			t.Fatalf("%s: memberships changed", name)
		}
		if count(t, e.s, "SELECT count(*) FROM folder_invitations WHERE invitation_id = ? AND status = 'pending'", inv.ID) != 1 {
			t.Fatalf("%s: invitation changed", name)
		}
		if e.audits(t, "folder.transfer_owner") != 0 {
			t.Fatalf("%s: audit row written", name)
		}
	}
	// Right before COMMIT, after every step.
	injected := errors.New("injected")
	c := e.by("owner", authz.TransferOwner, e.ids["alice"], "")
	c.BeforeCommit = func() error { return injected }
	if err := e.s.TransferOwnership(ctx, c); !errors.Is(err, injected) {
		t.Fatalf("before commit: %v", err)
	}
	check("before commit")
	// In the middle: membership removed and owner changed, then the
	// insert of the previous owner fails.
	if _, err := e.s.db.Exec(`CREATE TRIGGER test_fail_member BEFORE INSERT ON folder_members WHEN NEW.role = 'CONTRIBUTOR'
		BEGIN SELECT RAISE(ABORT, 'injected member insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.s.TransferOwnership(ctx, e.by("owner", authz.TransferOwner, e.ids["alice"], "")); err == nil {
		t.Fatal("transfer succeeded despite the failing insert")
	}
	check("mid-transfer")
	if _, err := e.s.db.Exec("DROP TRIGGER test_fail_member"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.TransferOwnership(ctx, e.by("owner", authz.TransferOwner, e.ids["alice"], "")); err != nil {
		t.Fatal(err)
	}
	e.checkOwner(t, "alice")
}

// TestConcurrentOwnerChanges: transfers and removals racing on the same
// folder serialize; the folder always ends with one owner who is not a
// member, and at most one transfer per round wins.
func TestConcurrentOwnerChanges(t *testing.T) {
	e := newMemberEnv(t)
	ctx := t.Context()
	for round := 0; round < 10; round++ {
		owner := "owner"
		for _, login := range []string{"owner", "alice", "bob"} {
			var n int
			if err := e.s.db.QueryRow("SELECT count(*) FROM folders WHERE folder_id = ? AND owner_user_id = ?", e.folder, e.ids[login]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 1 {
				owner = login
			}
		}
		for _, login := range []string{"owner", "alice", "bob"} {
			if login != owner && e.role(t, login) == "" {
				e.addMemberBy(t, owner, login, authz.Viewer)
			}
		}
		others := []string{}
		for _, login := range []string{"owner", "alice", "bob"} {
			if login != owner {
				others = append(others, login)
			}
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		transfers := 0
		run := func(fn func() error, isTransfer bool) {
			defer wg.Done()
			err := fn()
			if err != nil && !errors.Is(err, errNotAllowed) && !errors.Is(err, meta.ErrMemberNotFound) {
				t.Errorf("round %d: %v", round, err)
			}
			if err == nil && isTransfer {
				mu.Lock()
				transfers++
				mu.Unlock()
			}
		}
		wg.Add(3)
		go run(func() error {
			return e.s.TransferOwnership(ctx, e.by(owner, authz.TransferOwner, e.ids[others[0]], ""))
		}, true)
		go run(func() error {
			return e.s.TransferOwnership(ctx, e.by(owner, authz.TransferOwner, e.ids[others[1]], ""))
		}, true)
		go run(func() error { return e.s.RemoveMember(ctx, e.by(owner, authz.RevokeMember, e.ids[others[0]], "")) }, false)
		wg.Wait()
		if transfers > 1 {
			t.Fatalf("round %d: %d transfers succeeded", round, transfers)
		}
		if n := count(t, e.s, "SELECT count(*) FROM folder_members m JOIN folders f ON f.folder_id = m.folder_id WHERE m.user_id = f.owner_user_id"); n != 0 {
			t.Fatalf("round %d: owner is also a member", round)
		}
	}
}

func TestConcurrentAcceptOnce(t *testing.T) {
	e := newMemberEnv(t)
	inv, err := e.s.CreateInvitation(t.Context(), e.by("owner", authz.InviteMember, e.ids["alice"], authz.Viewer))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.s.AcceptInvitation(t.Context(), meta.AlbumChange{ActorID: e.ids["alice"], TargetID: inv.ID, Authorize: active})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, meta.ErrInvitationGone):
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 || e.audits(t, "invitation.accept") != 1 {
		t.Fatalf("%d acceptances succeeded", ok)
	}
}

func TestArchiveFolder(t *testing.T) {
	e := newMemberEnv(t)
	ctx := t.Context()
	e.addMember(t, "alice", authz.Contributor)
	inv, err := e.s.CreateInvitation(ctx, e.by("owner", authz.InviteMember, e.ids["bob"], authz.Viewer))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.ArchiveFolder(ctx, e.by("alice", authz.DeleteAlbum, "", "")); !errors.Is(err, errNotAllowed) {
		t.Fatalf("member archives: %v", err)
	}
	if err := e.s.ArchiveFolder(ctx, e.by("owner", authz.DeleteAlbum, "", "")); err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{"owner", "alice"} {
		if _, err := e.s.GetFolderAccess(ctx, e.folder, e.ids[login]); !errors.Is(err, meta.ErrFolderNotFound) {
			t.Fatalf("%s still sees the archived folder: %v", login, err)
		}
	}
	if _, err := e.s.AcceptInvitation(ctx, meta.AlbumChange{ActorID: e.ids["bob"], TargetID: inv.ID, Authorize: active}); !errors.Is(err, meta.ErrInvitationGone) {
		t.Fatalf("accept for an archived folder: %v", err)
	}
	if mine, _ := e.s.ListInvitationsFor(ctx, e.ids["bob"]); len(mine) != 0 {
		t.Fatal("invitation to an archived folder listed")
	}
	if err := e.s.ArchiveFolder(ctx, e.by("owner", authz.DeleteAlbum, "", "")); !errors.Is(err, meta.ErrFolderNotFound) {
		t.Fatalf("archive twice: %v", err)
	}
	if count(t, e.s, "SELECT count(*) FROM folders WHERE folder_id = ?", e.folder) != 1 || e.audits(t, "folder.archive") != 1 {
		t.Fatal("folder row removed or archive not audited")
	}
}
