package httpapi_test

import (
	"fmt"
	"testing"

	"github.com/aaaicu/withalbum/internal/auth/oidc"
	"github.com/aaaicu/withalbum/internal/auth/oidc/oidctest"
	"github.com/aaaicu/withalbum/internal/auth/ratelimit"
	"github.com/aaaicu/withalbum/internal/httpapi"
)

// M65: sign up with Google or Apple — through an album invite link (active
// at once, joins the album) or as a request the administrator approves.

func (e *authEnv) signUp(t *testing.T, token, nonce, invite string) resp {
	t.Helper()
	return e.call(t, "POST", "/api/v1/auth/sign-up", map[string]string{"provider": "google", "id_token": token, "nonce": nonce,
		"invite_token": invite, "device_name": "Phone", "platform": "android"}, nil)
}

func namedToken(ti *oidctest.TestIssuer, sub, email, name, nonce string) string {
	return ti.Sign(oidc.Google, oidctest.TokenFields{Sub: sub, Aud: testGoogleAud, Nonce: nonce, Email: email, Name: name})
}

// inviteLink makes an album as the administrator and a VIEWER link to it.
func (e *authEnv) inviteLink(t *testing.T) (folderID, token string) {
	t.Helper()
	admin, _ := e.mustLogin(t, "root", adminPassword)
	f := e.call(t, "POST", "/api/v1/folders", map[string]string{"name": "가족"}, bearer(admin))
	wantStatus(t, "folder", f, 201, "")
	folderID = f.body["id"].(string)
	l := e.call(t, "POST", "/api/v1/folders/"+folderID+"/invite-links", map[string]any{"role": "VIEWER"}, bearer(admin))
	wantStatus(t, "link", l, 201, "")
	return folderID, l.body["token"].(string)
}

func TestSignUpWithInviteLink(t *testing.T) {
	e, ti := newSignInEnv(t, true, ratelimit.Config{})
	folderID, link := e.inviteLink(t)

	n := e.nonce(t)
	wantStatus(t, "unknown link", e.signUp(t, namedToken(ti, "g-new", "x@gmail.example", "", n), n, "x"), 404, "invite_link_not_found")
	// The nonce is still unused: a wrong link is checked first.
	r := e.signUp(t, namedToken(ti, "g-new", "New.Person+tag@gmail.example", "새 가족", n), n, link)
	wantStatus(t, "sign up", r, 201, "")
	user := r.body["user"].(map[string]any)
	if user["display_name"] != "새 가족" || user["login_name"] != "new.persontag" {
		t.Fatalf("user %v", user)
	}
	if f, _ := r.body["folder"].(map[string]any); f == nil || f["id"] != folderID {
		t.Fatalf("folder %s", r.raw)
	}
	folders := e.call(t, "GET", "/api/v1/folders", nil, bearer(r.body["access_token"].(string)))
	if fmt.Sprint(folders.raw) == "" || !contains(folders.raw, folderID) {
		t.Fatalf("folders %s", folders.raw)
	}
	// From now on the identity signs in; signing up again just signs in.
	n = e.nonce(t)
	wantStatus(t, "sign in", e.signIn(t, "google", googleToken(ti, "g-new", n), n), 200, "")
	n = e.nonce(t)
	wantStatus(t, "sign up again", e.signUp(t, googleToken(ti, "g-new", n), n, ""), 200, "")
	// Nobody knows its password.
	wantStatus(t, "password login", e.login(t, "new.persontag", userPassword, "android"), 401, "invalid_credentials")
}

func TestSignUpRequestApproveReject(t *testing.T) {
	e, ti := newSignInEnv(t, true, ratelimit.Config{})
	n := e.call(t, "POST", "/api/v1/auth/sign-in/nonce", map[string]string{}, nil)
	if fmt.Sprint(n.body["sign_up"]) != "[invite request]" {
		t.Fatalf("nonce %s", n.raw)
	}
	nonce := n.body["nonce"].(string)
	r := e.signUp(t, namedToken(ti, "g-req", "req@gmail.example", "", nonce), nonce, "")
	wantStatus(t, "request", r, 202, "")
	if r.body["status"] != "pending" {
		t.Fatalf("request %s", r.raw)
	}
	nonce = e.nonce(t)
	wantStatus(t, "sign in while pending", e.signIn(t, "google", googleToken(ti, "g-req", nonce), nonce), 403, "sign_up_pending")
	nonce = e.nonce(t)
	wantStatus(t, "request again", e.signUp(t, googleToken(ti, "g-req", nonce), nonce, ""), 403, "sign_up_pending")
	nonce = e.nonce(t)
	wantStatus(t, "second request", e.signUp(t, googleToken(ti, "g-rej", nonce), nonce, ""), 202, "")

	admin, _ := e.mustLogin(t, "root", adminPassword)
	member, _ := e.mustLogin(t, "member", userPassword)
	wantStatus(t, "member list", e.call(t, "GET", "/api/v1/admin/sign-up-requests", nil, bearer(member)), 403, "forbidden")
	list := e.call(t, "GET", "/api/v1/admin/sign-up-requests", nil, bearer(admin))
	items, _ := list.body["items"].([]any)
	if len(items) != 2 || list.body["open"] != true {
		t.Fatalf("list %s", list.raw)
	}
	req := items[0].(map[string]any)
	if req["login_name"] != "req" || req["display_name"] != "req" || req["email"] != "req@gmail.example" || req["provider"] != "google" {
		t.Fatalf("request %v", req)
	}
	rej := items[1].(map[string]any)["user_id"].(string)

	wantStatus(t, "approve", e.call(t, "POST", "/api/v1/admin/sign-up-requests/"+req["user_id"].(string)+"/approve", nil, bearer(admin)), 204, "")
	wantStatus(t, "approve twice", e.call(t, "POST", "/api/v1/admin/sign-up-requests/"+req["user_id"].(string)+"/approve", nil, bearer(admin)), 404, "sign_up_request_not_found")
	nonce = e.nonce(t)
	wantStatus(t, "sign in approved", e.signIn(t, "google", googleToken(ti, "g-req", nonce), nonce), 200, "")

	wantStatus(t, "reject", e.call(t, "POST", "/api/v1/admin/sign-up-requests/"+rej+"/reject", nil, bearer(admin)), 204, "")
	nonce = e.nonce(t)
	wantStatus(t, "sign in rejected", e.signIn(t, "google", googleToken(ti, "g-rej", nonce), nonce), 401, "identity_not_linked")
	if l := e.call(t, "GET", "/api/v1/admin/sign-up-requests", nil, bearer(admin)); fmt.Sprint(l.body["items"]) != "[]" {
		t.Fatalf("left %s", l.raw)
	}
	// An approved-by-hand (enabled) waiting account leaves the list too.
	nonce = e.nonce(t)
	wantStatus(t, "third request", e.signUp(t, googleToken(ti, "g-3", nonce), nonce, ""), 202, "")
	l := e.call(t, "GET", "/api/v1/admin/sign-up-requests", nil, bearer(admin))
	id := l.body["items"].([]any)[0].(map[string]any)["user_id"].(string)
	wantStatus(t, "enable", e.call(t, "PATCH", "/api/v1/admin/users/"+id, map[string]string{"status": "active"}, bearer(admin)), 200, "")
	if l := e.call(t, "GET", "/api/v1/admin/sign-up-requests", nil, bearer(admin)); fmt.Sprint(l.body["items"]) != "[]" {
		t.Fatalf("after enable %s", l.raw)
	}
}

func TestSignUpRequestsOff(t *testing.T) {
	ti := oidctest.NewTestIssuer()
	t.Cleanup(ti.Close)
	e := newAuthEnvOpts(t, httpapi.AuthOptions{NoSignUpRequests: true, SignIn: oidc.Config{GoogleAudiences: []string{testGoogleAud},
		KeysURL: map[oidc.Provider]string{oidc.Google: ti.Server.URL}}})
	n := e.call(t, "POST", "/api/v1/auth/sign-in/nonce", map[string]string{}, nil)
	if fmt.Sprint(n.body["sign_up"]) != "[invite]" {
		t.Fatalf("nonce %s", n.raw)
	}
	nonce := n.body["nonce"].(string)
	wantStatus(t, "closed", e.signUp(t, googleToken(ti, "g", nonce), nonce, ""), 403, "sign_up_closed")
	_, link := e.inviteLink(t)
	wantStatus(t, "with link", e.signUp(t, googleToken(ti, "g", nonce), nonce, link), 201, "")
}

func TestSignUpQueueIsBounded(t *testing.T) {
	e, ti := newSignInEnv(t, true, ratelimit.Config{IPBurst: 100})
	for i := range 20 {
		n := e.nonce(t)
		wantStatus(t, fmt.Sprint("request ", i), e.signUp(t, googleToken(ti, fmt.Sprint("g-", i), n), n, ""), 202, "")
	}
	n := e.nonce(t)
	wantStatus(t, "full", e.signUp(t, googleToken(ti, "g-x", n), n, ""), 429, "sign_up_queue_full")
	// A link still works.
	_, link := e.inviteLink(t)
	n = e.nonce(t)
	wantStatus(t, "with link", e.signUp(t, googleToken(ti, "g-x", n), n, link), 201, "")
}

func TestSignUpProviderOff(t *testing.T) {
	e, ti := newSignInEnv(t, false, ratelimit.Config{})
	n := e.nonce(t)
	wantStatus(t, "google off", e.signUp(t, googleToken(ti, "g", n), n, ""), 400, "provider_disabled")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
