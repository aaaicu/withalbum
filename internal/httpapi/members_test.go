package httpapi_test

import (
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/users"
)

var jsonHdr = map[string]string{"Content-Type": "application/json"}

func (e *aclEnv) asJSON(t *testing.T, login, method, path, body string) reply {
	t.Helper()
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	return e.as(t, login, method, path, b, jsonHdr)
}

// invite creates an invitation from the owner of family and returns its id.
func (e *aclEnv) invite(t *testing.T, login, role string) string {
	t.Helper()
	r := e.asJSON(t, "owner", "POST", "/api/v1/folders/"+e.family+"/invitations", `{"invited_user_id":"`+e.ids[login]+`","role":"`+role+`"}`)
	return wantJSON(t, "invite "+login, r, 201)["id"].(string)
}

func (e *aclEnv) audits(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := e.Meta.DB().QueryRow("SELECT count(*) FROM audit_log WHERE action = ?", action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *aclEnv) ownerOf(t *testing.T, folder string) string {
	t.Helper()
	var owner string
	if err := e.Meta.DB().QueryRow("SELECT owner_user_id FROM folders WHERE folder_id = ?", folder).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	for login, id := range e.ids {
		if id == owner {
			return login
		}
	}
	return owner
}

// TestMembershipRouteMatrix: only the owner reaches the management routes;
// members get 403, everyone else (the server administrator included) 404.
// The owner's requests are chosen so that they pass authorization and stop
// at a business rule, which proves the decision without changing the
// folder; the archive request, run last, really archives it.
func TestMembershipRouteMatrix(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	members := map[string]int{"contributor": 403, "viewer": 403, "uploader": 403, "outsider": 404, "root": 404}
	ops := []struct {
		name, method, path, body string
		owner                    int
		ownerCode                string
	}{
		{"list invitations", "GET", f + "/invitations", "", 200, ""},
		{"invite", "POST", f + "/invitations", `{"invited_user_id":"` + e.ids["viewer"] + `","role":"VIEWER"}`, 409, "already_member"},
		{"revoke invitation", "DELETE", f + "/invitations/0123456789abcdef0123456789abcdef", "", 404, "invitation_not_found"},
		{"change role", "PATCH", f + "/members/" + e.ids["owner"], `{"role":"VIEWER"}`, 409, "target_is_owner"},
		{"remove member", "DELETE", f + "/members/" + e.ids["owner"], "", 409, "target_is_owner"},
		{"transfer", "POST", f + "/owner", `{"new_owner_user_id":"` + e.ids["outsider"] + `"}`, 404, "member_not_found"},
		{"archive", "POST", f + "/archive", "", 204, ""},
	}
	// M51: every reader lists the members; an UPLOADER does not.
	for login, want := range map[string]int{"owner": 200, "contributor": 200, "viewer": 200, "uploader": 403, "outsider": 404, "root": 404} {
		if r := e.as(t, login, "GET", f+"/members", nil, nil); r.Status != want {
			t.Fatalf("list members as %s: %d %s, want %d", login, r.Status, r.Body, want)
		}
	}
	for _, o := range ops {
		for _, login := range []string{"contributor", "viewer", "uploader", "outsider", "root", "owner"} {
			r := e.asJSON(t, login, o.method, o.path, o.body)
			want, code := members[login], map[int]string{403: "forbidden", 404: "folder_not_found"}[members[login]]
			if login == "owner" {
				want, code = o.owner, o.ownerCode
			}
			if r.Status != want {
				t.Fatalf("%s as %s: %d %s, want %d", o.name, login, r.Status, r.Body, want)
			}
			if code != "" {
				wantError(t, o.name+" as "+login, r, want, code)
			}
		}
	}
	// Nothing but the archive changed the folder.
	for _, a := range []string{"invitation.create", "invitation.revoke", "member.role_change", "member.remove", "folder.transfer_owner"} {
		if n := e.audits(t, a); n != 0 {
			t.Fatalf("%d %s rows", n, a)
		}
	}
	if e.audits(t, "folder.archive") != 1 {
		t.Fatal("archive not audited")
	}
	for _, login := range aclRoles {
		wantError(t, "archived as "+login, e.as(t, login, "GET", f, nil, nil), 404, "folder_not_found")
	}
}

// TestInvitationFlow: ACCESS_CONTROL §7.6 — the owner invites a registered
// account, nothing is accessible before acceptance, and only the named
// account can accept, once.
func TestInvitationFlow(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	orig := f + "/assets/" + e.photo + "/original"
	id := e.invite(t, "outsider", "VIEWER")
	if len(id) != 32 {
		t.Fatalf("invitation id %q", id)
	}
	wantError(t, "invite twice", e.asJSON(t, "owner", "POST", f+"/invitations", `{"invited_user_id":"`+e.ids["outsider"]+`","role":"CONTRIBUTOR"}`), 409, "invitation_pending")
	// Before acceptance: no access at all.
	wantError(t, "before accept", e.as(t, "outsider", "GET", f, nil, nil), 404, "folder_not_found")
	wantError(t, "before accept", e.as(t, "outsider", "GET", orig, nil, nil), 404, "folder_not_found")
	// The dashboard says invitees see nothing but the album name (and who
	// invited them) before accepting: no members, photos or counts.
	for _, p := range []string{f + "/members", f + "/assets", f + "/sync", f + "/invitations", f + "/assets/" + e.photo + "/preview"} {
		wantError(t, "before accept "+p, e.as(t, "outsider", "GET", p, nil, nil), 404, "folder_not_found")
	}
	mine := wantJSON(t, "my invitations", e.as(t, "outsider", "GET", "/api/v1/invitations", nil, nil), 200)["invitations"].([]any)
	if len(mine) != 1 {
		t.Fatalf("invitations %v", mine)
	}
	inv := mine[0].(map[string]any)
	wantKeys(t, "invitation", inv, "id", "folder_id", "folder_name", "invitee_user_id", "invitee_display_name", "inviter_user_id",
		"inviter_display_name", "role", "status", "created_at", "expires_at")
	if inv["id"] != id || inv["folder_name"] != "family" || inv["role"] != "VIEWER" || inv["status"] != "pending" {
		t.Fatalf("invitation %v", inv)
	}
	for _, login := range []string{"owner", "contributor", "root"} {
		if l := wantJSON(t, "others' invitations", e.as(t, login, "GET", "/api/v1/invitations", nil, nil), 200)["invitations"].([]any); len(l) != 0 {
			t.Fatalf("%s sees %v", login, l)
		}
	}
	if l := wantJSON(t, "folder invitations", e.as(t, "owner", "GET", f+"/invitations", nil, nil), 200)["invitations"].([]any); len(l) != 1 {
		t.Fatalf("owner sees %v", l)
	}
	// Someone else's invitation: 403, even for the administrator.
	for _, login := range []string{"contributor", "root", "owner"} {
		wantError(t, "accept as "+login, e.as(t, login, "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), 403, "forbidden")
	}
	wantError(t, "unknown invitation", e.as(t, "outsider", "POST", "/api/v1/invitations/0123456789abcdef0123456789abcdef/accept", nil, nil), 404, "invitation_not_found")
	m := wantJSON(t, "accept", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), 200)
	if m["id"] != e.family || m["role"] != "VIEWER" || m["can_read"] != true || m["can_upload"] != false {
		t.Fatalf("accepted folder %v", m)
	}
	if r := e.as(t, "outsider", "GET", orig, nil, nil); r.Status != 200 {
		t.Fatalf("member download %d", r.Status)
	}
	wantError(t, "accept again", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), 410, "invitation_gone")
	wantError(t, "invite a member", e.asJSON(t, "owner", "POST", f+"/invitations", `{"invited_user_id":"`+e.ids["outsider"]+`","role":"VIEWER"}`), 409, "already_member")
	if l := wantJSON(t, "after accept", e.as(t, "outsider", "GET", "/api/v1/invitations", nil, nil), 200)["invitations"].([]any); len(l) != 0 {
		t.Fatalf("accepted invitation still listed %v", l)
	}
	if e.audits(t, "invitation.create") != 1 || e.audits(t, "invitation.accept") != 1 {
		t.Fatal("invitation not audited")
	}

	// Invalid invitees all look alike: unknown, disabled, locked.
	us := users.New(e.Meta.DB())
	dis, err := us.Create(t.Context(), users.NewUser{LoginName: "gone", DisplayName: "gone", PasswordHash: mustHash(t), Role: users.RoleUser}, users.Console)
	if err != nil {
		t.Fatal(err)
	}
	if err := us.SetStatus(t.Context(), dis.ID, users.StatusDisabled, users.Console); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, target := range []string{"0123456789abcdef0123456789abcdef", dis.ID} {
		r := e.asJSON(t, "owner", "POST", f+"/invitations", `{"invited_user_id":"`+target+`","role":"VIEWER"}`)
		wantError(t, "ineligible invitee", r, 404, "invitee_not_found")
		bodies = append(bodies, string(r.Body))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("unknown and disabled invitees differ: %q %q", bodies[0], bodies[1])
	}
	wantError(t, "bad role", e.asJSON(t, "owner", "POST", f+"/invitations", `{"invited_user_id":"`+e.ids["root"]+`","role":"OWNER"}`), 400, "invalid_role")
	wantError(t, "no json", e.as(t, "owner", "POST", f+"/invitations", []byte(`{}`), nil), 415, "json_required")
	wantError(t, "no invitee", e.asJSON(t, "owner", "POST", f+"/invitations", `{"role":"VIEWER"}`), 400, "invalid_request")
}

func mustHash(t *testing.T) string {
	t.Helper()
	h, err := password.Hash(userPassword, fastParams)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestInvitationRevokedOrExpired: a revoked or expired invitation is 410
// and creates no membership; an expired one can be replaced.
func TestInvitationRevokedOrExpired(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	id := e.invite(t, "outsider", "CONTRIBUTOR")
	if r := e.as(t, "owner", "DELETE", f+"/invitations/"+id, nil, nil); r.Status != 204 {
		t.Fatalf("revoke %d %s", r.Status, r.Body)
	}
	wantError(t, "revoke twice", e.as(t, "owner", "DELETE", f+"/invitations/"+id, nil, nil), 410, "invitation_gone")
	wantError(t, "accept revoked", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), 410, "invitation_gone")
	wantError(t, "no access", e.as(t, "outsider", "GET", f, nil, nil), 404, "folder_not_found")
	// Revoking through another folder does not find it.
	id = e.invite(t, "outsider", "CONTRIBUTOR")
	wantError(t, "other folder", e.as(t, "root", "DELETE", "/api/v1/folders/"+e.adminF+"/invitations/"+id, nil, nil), 404, "invitation_not_found")
	if _, err := e.Meta.DB().Exec("UPDATE folder_invitations SET expires_at = '2020-01-01T00:00:00.000000000Z' WHERE invitation_id = ?", id); err != nil {
		t.Fatal(err)
	}
	if l := wantJSON(t, "expired list", e.as(t, "outsider", "GET", "/api/v1/invitations", nil, nil), 200)["invitations"].([]any); len(l) != 0 {
		t.Fatalf("expired invitation listed %v", l)
	}
	wantError(t, "accept expired", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), 410, "invitation_gone")
	wantError(t, "revoke expired", e.as(t, "owner", "DELETE", f+"/invitations/"+id, nil, nil), 410, "invitation_gone")
	wantError(t, "no access", e.as(t, "outsider", "GET", f, nil, nil), 404, "folder_not_found")
	again := e.invite(t, "outsider", "VIEWER")
	wantJSON(t, "accept new", e.as(t, "outsider", "POST", "/api/v1/invitations/"+again+"/accept", nil, nil), 200)
	if e.audits(t, "invitation.revoke") != 1 {
		t.Fatal("revocation not audited once")
	}
}

// TestRoleChangeAndRemovalOverHTTP: ACCESS_CONTROL §7.7 — a downgrade or
// removal applies to the member's very next request, on the same session.
func TestRoleChangeAndRemovalOverHTTP(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	list := wantJSON(t, "members", e.as(t, "owner", "GET", f+"/members", nil, nil), 200)["members"].([]any)
	if len(list) != 4 || list[0].(map[string]any)["role"] != "OWNER" || list[0].(map[string]any)["user_id"] != e.ids["owner"] {
		t.Fatalf("members %v", list)
	}
	wantKeys(t, "member", list[1].(map[string]any), "user_id", "display_name", "role", "active", "joined_at", "is_me", "is_owner")
	if r := e.as(t, "viewer", "GET", f+"/sync", nil, nil); r.Status != 200 {
		t.Fatalf("viewer sync %d", r.Status)
	}
	m := wantJSON(t, "downgrade", e.asJSON(t, "owner", "PATCH", f+"/members/"+e.ids["viewer"], `{"role":"UPLOADER"}`), 200)
	if m["role"] != "UPLOADER" || m["user_id"] != e.ids["viewer"] {
		t.Fatalf("member %v", m)
	}
	wantError(t, "downgraded sync", e.as(t, "viewer", "GET", f+"/sync", nil, nil), 403, "forbidden")
	wantJSON(t, "upgrade", e.asJSON(t, "owner", "PATCH", f+"/members/"+e.ids["viewer"], `{"role":"CONTRIBUTOR"}`), 200)
	wantJSON(t, "upgraded upload", e.as(t, "viewer", "POST", f+"/assets", []byte("now a contributor"), nil), 201)
	wantError(t, "bad role", e.asJSON(t, "owner", "PATCH", f+"/members/"+e.ids["viewer"], `{"role":"OWNER"}`), 400, "invalid_role")
	wantError(t, "not a member", e.asJSON(t, "owner", "PATCH", f+"/members/"+e.ids["outsider"], `{"role":"VIEWER"}`), 404, "member_not_found")
	if r := e.as(t, "owner", "DELETE", f+"/members/"+e.ids["viewer"], nil, nil); r.Status != 204 {
		t.Fatalf("remove %d %s", r.Status, r.Body)
	}
	wantError(t, "removed", e.as(t, "viewer", "GET", f, nil, nil), 404, "folder_not_found")
	wantError(t, "removed", e.as(t, "viewer", "GET", f+"/assets/"+e.photo+"/original", nil, nil), 404, "folder_not_found")
	if l := wantJSON(t, "list", e.as(t, "viewer", "GET", "/api/v1/folders", nil, nil), 200)["folders"].([]any); len(l) != 0 {
		t.Fatalf("removed member lists %v", l)
	}
	wantError(t, "remove twice", e.as(t, "owner", "DELETE", f+"/members/"+e.ids["viewer"], nil, nil), 404, "member_not_found")
	if e.audits(t, "member.role_change") != 2 || e.audits(t, "member.remove") != 1 {
		t.Fatal("changes not audited")
	}

	// Removed through the API while an upload is in flight: the commit
	// re-check refuses it.
	var once sync.Once
	e.faults.set(httpapi.FaultAfterPreview, func() error {
		once.Do(func() {
			if r := e.as(t, "owner", "DELETE", f+"/members/"+e.ids["contributor"], nil, nil); r.Status != 204 {
				t.Errorf("remove during upload %d %s", r.Status, r.Body)
			}
		})
		return nil
	})
	body := []byte("upload of a member removed meanwhile")
	r := e.as(t, "contributor", "POST", f+"/assets", body, nil)
	e.faults.set(httpapi.FaultAfterPreview, nil)
	wantError(t, "upload after removal", r, 404, "folder_not_found")
	var n int
	if err := e.Meta.DB().QueryRow("SELECT count(*) FROM assets WHERE original_sha256 = ?", shaHex(body)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("asset rows %d %v", n, err)
	}
}

// TestOwnershipTransfer: the new owner gets every owner right at once, the
// previous owner becomes a CONTRIBUTOR (reads and uploads, manages
// nothing), and pending invitations of the previous owner are revoked.
func TestOwnershipTransfer(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	pending := e.invite(t, "outsider", "VIEWER")
	wantError(t, "to non-member", e.asJSON(t, "owner", "POST", f+"/owner", `{"new_owner_user_id":"`+e.ids["root"]+`"}`), 404, "member_not_found")
	wantError(t, "to self", e.asJSON(t, "owner", "POST", f+"/owner", `{"new_owner_user_id":"`+e.ids["owner"]+`"}`), 409, "target_is_owner")
	wantError(t, "no target", e.asJSON(t, "owner", "POST", f+"/owner", `{}`), 400, "invalid_request")
	us := users.New(e.Meta.DB())
	if err := us.SetStatus(t.Context(), e.ids["uploader"], users.StatusDisabled, users.Console); err != nil {
		t.Fatal(err)
	}
	wantError(t, "to disabled member", e.asJSON(t, "owner", "POST", f+"/owner", `{"new_owner_user_id":"`+e.ids["uploader"]+`"}`), 409, "user_inactive")

	m := wantJSON(t, "transfer", e.asJSON(t, "owner", "POST", f+"/owner", `{"new_owner_user_id":"`+e.ids["viewer"]+`"}`), 200)
	if m["role"] != "CONTRIBUTOR" || m["can_read"] != true || m["can_upload"] != true {
		t.Fatalf("previous owner's view %v", m)
	}
	if e.ownerOf(t, e.family) != "viewer" {
		t.Fatal("owner not changed")
	}
	// The previous owner: CONTRIBUTOR on the next request.
	if got := wantJSON(t, "folder", e.as(t, "owner", "GET", f, nil, nil), 200)["role"]; got != "CONTRIBUTOR" {
		t.Fatalf("previous owner role %v", got)
	}
	wantJSON(t, "previous owner uploads", e.as(t, "owner", "POST", f+"/assets", []byte("by the previous owner"), nil), 201)
	if r := e.as(t, "owner", "GET", f+"/assets/"+e.photo+"/original", nil, nil); r.Status != 200 {
		t.Fatalf("previous owner download %d", r.Status)
	}
	for name, req := range map[string][3]string{
		"invite":      {"POST", f + "/invitations", `{"invited_user_id":"` + e.ids["root"] + `","role":"VIEWER"}`},
		"invitations": {"GET", f + "/invitations", ""},
		"remove":      {"DELETE", f + "/members/" + e.ids["contributor"], ""},
		"transfer":    {"POST", f + "/owner", `{"new_owner_user_id":"` + e.ids["contributor"] + `"}`},
		"archive":     {"DELETE", f, ""},
	} {
		wantError(t, "previous owner "+name, e.asJSON(t, "owner", req[0], req[1], req[2]), 403, "forbidden")
	}
	// The pending invitation died with the transfer.
	wantError(t, "accept stale", e.as(t, "outsider", "POST", "/api/v1/invitations/"+pending+"/accept", nil, nil), 410, "invitation_gone")
	// The new owner manages the folder.
	list := wantJSON(t, "members", e.as(t, "viewer", "GET", f+"/members", nil, nil), 200)["members"].([]any)
	roles := map[string]string{}
	for _, it := range list {
		mm := it.(map[string]any)
		roles[mm["user_id"].(string)] = mm["role"].(string)
	}
	if roles[e.ids["viewer"]] != "OWNER" || roles[e.ids["owner"]] != "CONTRIBUTOR" || len(roles) != 4 {
		t.Fatalf("members after transfer %v", roles)
	}
	r := e.as(t, "viewer", "POST", f+"/invitations", []byte(`{"invited_user_id":"`+e.ids["outsider"]+`","role":"UPLOADER"}`), jsonHdr)
	wantJSON(t, "new owner invites", r, 201)
	wantJSON(t, "new owner demotes", e.asJSON(t, "viewer", "PATCH", f+"/members/"+e.ids["owner"], `{"role":"VIEWER"}`), 200)
	wantError(t, "demoted previous owner uploads", e.as(t, "owner", "POST", f+"/assets", []byte("no longer"), nil), 403, "forbidden")
	if e.audits(t, "folder.transfer_owner") != 1 {
		t.Fatal("transfer not audited")
	}
	// The administrator cannot take the folder.
	wantError(t, "admin transfer", e.asJSON(t, "root", "POST", f+"/owner", `{"new_owner_user_id":"`+e.ids["root"]+`"}`), 404, "folder_not_found")
}

// TestMembershipFaultRollsBack: ACCESS_CONTROL §7.10 — a failure inside the
// transaction leaves owner, memberships, invitations and audit log as they
// were.
func TestMembershipFaultRollsBack(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	id := e.invite(t, "outsider", "VIEWER")
	snapshot := func() string {
		var s string
		if err := e.Meta.DB().QueryRow(`SELECT (SELECT owner_user_id FROM folders WHERE folder_id = ?) || '|' ||
			(SELECT group_concat(user_id || ':' || role, ',') FROM (SELECT * FROM folder_members WHERE folder_id = ? ORDER BY user_id)) || '|' ||
			(SELECT group_concat(invitation_id || ':' || status, ',') FROM folder_invitations) || '|' ||
			(SELECT count(*) FROM audit_log)`, e.family, e.family).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	for _, c := range []struct {
		fault  error
		status int
		code   string
	}{
		{errors.New("injected"), 500, "metadata_write_failed"},
		{syscall.EIO, 503, "storage_io_error"},
	} {
		e.faults.set(httpapi.FaultMembershipBeforeCommit, func() error { return c.fault })
		for name, req := range map[string][3]string{
			"transfer": {"POST", f + "/owner", `{"new_owner_user_id":"` + e.ids["viewer"] + `"}`},
			"role":     {"PATCH", f + "/members/" + e.ids["viewer"], `{"role":"UPLOADER"}`},
			"remove":   {"DELETE", f + "/members/" + e.ids["viewer"], ""},
			"invite":   {"POST", f + "/invitations", `{"invited_user_id":"` + e.ids["root"] + `","role":"VIEWER"}`},
			"revoke":   {"DELETE", f + "/invitations/" + id, ""},
			"archive":  {"DELETE", f, ""},
		} {
			wantError(t, name, e.asJSON(t, "owner", req[0], req[1], req[2]), c.status, c.code)
		}
		wantError(t, "accept", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), c.status, c.code)
		wantError(t, "decline", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/decline", nil, nil), c.status, c.code)
		e.faults.set(httpapi.FaultMembershipBeforeCommit, nil)
		if after := snapshot(); after != before {
			t.Fatalf("state changed by failed writes:\n%s\n%s", before, after)
		}
	}
	wantJSON(t, "transfer works again", e.asJSON(t, "owner", "POST", f+"/owner", `{"new_owner_user_id":"`+e.ids["viewer"]+`"}`), 200)
}

// TestConcurrentTransferAndRemoval: the owner's transfer to a member and
// the removal of that member race; exactly one wins and the folder keeps
// exactly one owner who is not also a member.
func TestConcurrentTransferAndRemoval(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	for round := 0; round < 5; round++ {
		owner := e.ownerOf(t, e.family)
		target := "viewer"
		if owner == "viewer" {
			target = "contributor"
		}
		// Make sure target is a member again.
		var n int
		if err := e.Meta.DB().QueryRow("SELECT count(*) FROM folder_members WHERE folder_id = ? AND user_id = ?", e.family, e.ids[target]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			r := e.as(t, owner, "POST", f+"/invitations", []byte(`{"invited_user_id":"`+e.ids[target]+`","role":"VIEWER"}`), jsonHdr)
			inv := wantJSON(t, "re-invite", r, 201)["id"].(string)
			wantJSON(t, "re-accept", e.as(t, target, "POST", "/api/v1/invitations/"+inv+"/accept", nil, nil), 200)
		}
		var wg sync.WaitGroup
		var transfer, remove reply
		wg.Add(2)
		go func() {
			defer wg.Done()
			transfer = e.as(t, owner, "POST", f+"/owner", []byte(`{"new_owner_user_id":"`+e.ids[target]+`"}`), jsonHdr)
		}()
		go func() { defer wg.Done(); remove = e.as(t, owner, "DELETE", f+"/members/"+e.ids[target], nil, nil) }()
		wg.Wait()
		switch {
		case transfer.Status == 200 && (remove.Status == 403 || remove.Status == 409):
			if e.ownerOf(t, e.family) != target {
				t.Fatalf("round %d: transfer reported but owner is %s", round, e.ownerOf(t, e.family))
			}
		case remove.Status == 204 && transfer.Status == 404:
			wantError(t, "transfer after removal", transfer, 404, "member_not_found")
			if e.ownerOf(t, e.family) != owner {
				t.Fatalf("round %d: owner changed", round)
			}
		default:
			t.Fatalf("round %d: transfer %d %s, remove %d %s", round, transfer.Status, transfer.Body, remove.Status, remove.Body)
		}
		if err := e.Meta.DB().QueryRow("SELECT count(*) FROM folder_members m JOIN folders f ON f.folder_id = m.folder_id WHERE m.user_id = f.owner_user_id").Scan(&n); err != nil || n != 0 {
			t.Fatalf("round %d: owner is also a member (%d, %v)", round, n, err)
		}
	}
}

// TestAuditRowsCarryNoSecrets: membership audit rows name users, folders
// and invitations, never a token.
func TestAuditRowsCarryNoSecrets(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	id := e.invite(t, "outsider", "VIEWER")
	wantJSON(t, "accept", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), 200)
	wantJSON(t, "role", e.asJSON(t, "owner", "PATCH", f+"/members/"+e.ids["outsider"], `{"role":"CONTRIBUTOR"}`), 200)
	wantJSON(t, "transfer", e.asJSON(t, "owner", "POST", f+"/owner", `{"new_owner_user_id":"`+e.ids["outsider"]+`"}`), 200)
	rows, err := e.Meta.DB().Query(`SELECT action, COALESCE(actor_user_id, ''), target_type, COALESCE(target_id, ''), result, COALESCE(details_json, '')
		FROM audit_log WHERE action LIKE 'invitation.%' OR action LIKE 'member.%' OR action LIKE 'folder.%' ORDER BY occurred_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, actor, typ, target, result, details string
		if err := rows.Scan(&action, &actor, &typ, &target, &result, &details); err != nil {
			t.Fatal(err)
		}
		got = append(got, action)
		if actor == "" || target == "" || result != "allowed" {
			t.Fatalf("%s: actor %q target %q result %q", action, actor, target, result)
		}
		for login, tok := range e.tokens {
			if strings.Contains(details, tok) {
				t.Fatalf("%s carries the token of %s", action, login)
			}
		}
	}
	if strings.Join(got, ",") != "invitation.create,invitation.accept,member.role_change,folder.transfer_owner" {
		t.Fatalf("audit actions %v", got)
	}
}

// TestInvitationDecline: the named recipient declines with its own session,
// decided like accept — unknown 404, another account's 403 (the server
// administrator and the owner included), accepted/revoked/expired/declined
// or archived 410 — in one transaction with an audit row. A declined
// invitation leaves both lists and the owner can invite again.
func TestInvitationDecline(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	decline := func(login, id string) reply {
		return e.as(t, login, "POST", "/api/v1/invitations/"+id+"/decline", nil, nil)
	}
	id := e.invite(t, "outsider", "CONTRIBUTOR")
	for _, login := range []string{"owner", "contributor", "viewer", "uploader", "root"} {
		wantError(t, "decline as "+login, decline(login, id), 403, "forbidden")
	}
	wantError(t, "unknown", decline("outsider", "0123456789abcdef0123456789abcdef"), 404, "invitation_not_found")
	wantError(t, "no session", do(t, e.Raw, "POST", "/api/v1/invitations/"+id+"/decline", nil, nil), 401, "unauthorized")
	if e.audits(t, "invitation.decline") != 0 {
		t.Fatal("refused declines were audited")
	}
	if r := decline("outsider", id); r.Status != 204 || len(r.Body) != 0 {
		t.Fatalf("decline %d %s", r.Status, r.Body)
	}
	wantError(t, "decline again", decline("outsider", id), 410, "invitation_gone")
	wantError(t, "accept declined", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), 410, "invitation_gone")
	wantError(t, "revoke declined", e.as(t, "owner", "DELETE", f+"/invitations/"+id, nil, nil), 410, "invitation_gone")
	wantError(t, "no access", e.as(t, "outsider", "GET", f, nil, nil), 404, "folder_not_found")
	for who, path := range map[string]string{"outsider": "/api/v1/invitations", "owner": f + "/invitations"} {
		if l := wantJSON(t, "list "+who, e.as(t, who, "GET", path, nil, nil), 200)["invitations"].([]any); len(l) != 0 {
			t.Fatalf("declined invitation listed for %s: %v", who, l)
		}
	}
	if e.audits(t, "invitation.decline") != 1 {
		t.Fatal("decline not audited once")
	}
	var actor, target, details string
	if err := e.Meta.DB().QueryRow("SELECT actor_user_id, target_id, details_json FROM audit_log WHERE action = 'invitation.decline'").Scan(&actor, &target, &details); err != nil {
		t.Fatal(err)
	}
	if actor != e.ids["outsider"] || target != id || !strings.Contains(details, e.family) || !strings.Contains(details, e.ids["owner"]) {
		t.Fatalf("audit row actor %q target %q details %s", actor, target, details)
	}
	for login, tok := range e.tokens {
		if strings.Contains(details, tok) {
			t.Fatalf("decline audit carries the token of %s", login)
		}
	}

	// The owner may invite the declining account again (the pending UNIQUE
	// index only covers pending rows), and the new invitation works.
	again := e.invite(t, "outsider", "VIEWER")
	if again == id {
		t.Fatal("same invitation id")
	}
	wantJSON(t, "accept new", e.as(t, "outsider", "POST", "/api/v1/invitations/"+again+"/accept", nil, nil), 200)
	// After accepting, declining the accepted invitation is 410 and the
	// membership stays.
	wantError(t, "decline accepted", decline("outsider", again), 410, "invitation_gone")
	if r := e.as(t, "outsider", "GET", f, nil, nil); r.Status != 200 {
		t.Fatalf("member lost access: %d", r.Status)
	}

	// Revoked, expired and archived invitations are 410 too, and nothing is
	// written for them.
	revoked := e.invite(t, "root", "VIEWER")
	if r := e.as(t, "owner", "DELETE", f+"/invitations/"+revoked, nil, nil); r.Status != 204 {
		t.Fatalf("revoke %d", r.Status)
	}
	wantError(t, "decline revoked", decline("root", revoked), 410, "invitation_gone")
	expired := e.invite(t, "root", "VIEWER")
	if _, err := e.Meta.DB().Exec("UPDATE folder_invitations SET expires_at = '2020-01-01T00:00:00.000000000Z' WHERE invitation_id = ?", expired); err != nil {
		t.Fatal(err)
	}
	wantError(t, "decline expired", decline("root", expired), 410, "invitation_gone")
	var status string
	if err := e.Meta.DB().QueryRow("SELECT status FROM folder_invitations WHERE invitation_id = ?", expired).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("expired invitation rewritten to %q, %v", status, err)
	}
	archived := e.invite(t, "root", "VIEWER")
	if r := e.as(t, "owner", "DELETE", f, nil, nil); r.Status != 204 {
		t.Fatalf("archive %d", r.Status)
	}
	wantError(t, "decline archived", decline("root", archived), 410, "invitation_gone")
	if e.audits(t, "invitation.decline") != 1 {
		t.Fatal("a refused decline was audited")
	}
}
