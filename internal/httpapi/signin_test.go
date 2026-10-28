package httpapi_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/auth/oidc"
	"github.com/aaaicu/withalbum/internal/auth/oidc/oidctest"
	"github.com/aaaicu/withalbum/internal/auth/ratelimit"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/users"
)

const (
	testGoogleAud = "test-ios.apps.googleusercontent.com"
	testAppleAud  = "aaaicu.withalbum.app"
)

func newSignInEnv(t *testing.T, google bool, rl ratelimit.Config) (*authEnv, *oidctest.TestIssuer) {
	t.Helper()
	ti := oidctest.NewTestIssuer()
	t.Cleanup(ti.Close)
	cfg := oidc.Config{AppleAudiences: []string{testAppleAud},
		KeysURL: map[oidc.Provider]string{oidc.Google: ti.Server.URL, oidc.Apple: ti.Server.URL}}
	if google {
		cfg.GoogleAudiences = []string{testGoogleAud}
	}
	return newAuthEnvOpts(t, httpapi.AuthOptions{RateLimit: rl, SignIn: cfg}), ti
}

func (e *authEnv) nonce(t *testing.T) string {
	t.Helper()
	r := e.call(t, "POST", "/api/v1/auth/sign-in/nonce", map[string]string{}, nil)
	wantStatus(t, "nonce", r, 200, "")
	return r.body["nonce"].(string)
}

func googleToken(ti *oidctest.TestIssuer, sub, nonce string) string {
	return ti.Sign(oidc.Google, oidctest.TokenFields{Sub: sub, Aud: testGoogleAud, Nonce: nonce, Email: sub + "@gmail.example"})
}

func (e *authEnv) signIn(t *testing.T, provider, token, nonce string) resp {
	t.Helper()
	return e.call(t, "POST", "/api/v1/auth/sign-in", map[string]string{"provider": provider, "id_token": token, "nonce": nonce,
		"device_name": "Phone", "platform": "android"}, nil)
}

func (e *authEnv) link(t *testing.T, access, provider, token, nonce, pw string) resp {
	t.Helper()
	return e.call(t, "POST", "/api/v1/me/sign-ins", map[string]string{"provider": provider, "id_token": token, "nonce": nonce,
		"current_password": pw}, bearer(access))
}

func TestSignInLinkAndUse(t *testing.T) {
	e, ti := newSignInEnv(t, true, ratelimit.Config{IPBurst: 1000, PairFailures: 100, AccountFailures: 100})
	r := e.call(t, "POST", "/api/v1/auth/sign-in/nonce", map[string]string{}, nil)
	wantStatus(t, "nonce", r, 200, "")
	if fmt.Sprint(r.body["providers"]) != "[google apple]" || r.body["expires_at"] == nil {
		t.Fatalf("nonce reply %s", r.raw)
	}

	// Not linked yet: the token is fine, but no account has it.
	n := e.nonce(t)
	wantStatus(t, "not linked", e.signIn(t, "google", googleToken(ti, "g-member", n), n), 401, "identity_not_linked")

	access, _ := e.mustLogin(t, "member", userPassword)
	n = e.nonce(t)
	wantStatus(t, "no password", e.link(t, access, "google", googleToken(ti, "g-member", n), n, ""), 400, "invalid_request")
	n = e.nonce(t)
	wantStatus(t, "wrong password", e.link(t, access, "google", googleToken(ti, "g-member", n), n, "not the password"), 403, "invalid_credentials")
	n = e.nonce(t)
	wantStatus(t, "link with a bad token", e.link(t, access, "google", "bad.token.here", n, userPassword), 400, "invalid_identity_token")
	wantStatus(t, "link with a used nonce", e.link(t, access, "google", googleToken(ti, "g-member", n), n, userPassword), 400, "invalid_identity_token")
	n = e.nonce(t)
	wantStatus(t, "unknown provider", e.link(t, access, "facebook", "x.y.z", n, userPassword), 400, "invalid_request")
	n = e.nonce(t)
	l := e.link(t, access, "google", googleToken(ti, "g-member", n), n, userPassword)
	wantStatus(t, "link", l, 201, "")
	if l.body["provider"] != "google" || l.body["email"] != "g-member@gmail.example" || l.body["last_used_at"] != nil || strings.Contains(l.raw, "g-member\"") {
		t.Fatalf("link reply %s", l.raw)
	}
	// The same identity again: the existing link.
	n = e.nonce(t)
	wantStatus(t, "link again", e.link(t, access, "google", googleToken(ti, "g-member", n), n, userPassword), 201, "")
	// Another Google identity for the same account: unlink first.
	n = e.nonce(t)
	wantStatus(t, "second google", e.link(t, access, "google", googleToken(ti, "g-other", n), n, userPassword), 409, "provider_linked")
	// The member's identity for the administrator: in use.
	adminAccess, _ := e.mustLogin(t, "root", adminPassword)
	n = e.nonce(t)
	wantStatus(t, "in use", e.link(t, adminAccess, "google", googleToken(ti, "g-member", n), n, adminPassword), 409, "identity_in_use")

	// Sign in.
	n = e.nonce(t)
	tok := googleToken(ti, "g-member", n)
	s := e.signIn(t, "google", tok, n)
	wantStatus(t, "sign in", s, 200, "")
	if s.body["token_type"] != "Bearer" || s.body["access_token"] == nil || s.body["user"].(map[string]any)["login_name"] != "member" {
		t.Fatalf("sign-in reply %s", s.raw)
	}
	me := e.call(t, "GET", "/api/v1/me", nil, bearer(s.body["access_token"].(string)))
	wantStatus(t, "me with the new session", me, 200, "")
	if !strings.Contains(fmt.Sprint(me.body["server_features"]), "sign_in") {
		t.Fatalf("features %v", me.body["server_features"])
	}
	// The nonce is used up, also with the same token.
	wantStatus(t, "replay", e.signIn(t, "google", tok, n), 401, "invalid_identity_token")
	// A nonce the server never issued, and a token naming another nonce.
	wantStatus(t, "unissued nonce", e.signIn(t, "google", googleToken(ti, "g-member", "made-up"), "made-up"), 401, "invalid_identity_token")
	n = e.nonce(t)
	n2 := e.nonce(t)
	wantStatus(t, "nonce mismatch", e.signIn(t, "google", googleToken(ti, "g-member", n2), n), 401, "invalid_identity_token")
	// A token for another app.
	n = e.nonce(t)
	wantStatus(t, "other app", e.signIn(t, "google", ti.Sign(oidc.Google, oidctest.TokenFields{Sub: "g-member", Aud: "other-app", Nonce: n}), n), 401, "invalid_identity_token")
	// A Google token presented as Apple.
	n = e.nonce(t)
	wantStatus(t, "provider mix", e.signIn(t, "apple", googleToken(ti, "g-member", n), n), 401, "invalid_identity_token")
	wantStatus(t, "bad device", e.call(t, "POST", "/api/v1/auth/sign-in", map[string]string{"provider": "google", "id_token": tok, "nonce": n,
		"device_name": "", "platform": "android"}, nil), 400, "invalid_device")

	list := e.call(t, "GET", "/api/v1/me/sign-ins", nil, bearer(access))
	wantStatus(t, "list", list, 200, "")
	items := list.body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["last_used_at"] == nil || fmt.Sprint(list.body["providers"]) != "[google apple]" {
		t.Fatalf("list %s", list.raw)
	}

	// Apple: the app hands Apple the SHA-256 of the nonce.
	n = e.nonce(t)
	sum := sha256.Sum256([]byte(n))
	apple := ti.Sign(oidc.Apple, oidctest.TokenFields{Sub: "001234.abcd", Aud: testAppleAud, Nonce: hex.EncodeToString(sum[:])})
	wantStatus(t, "link apple", e.link(t, access, "apple", apple, n, userPassword), 201, "")
	n = e.nonce(t)
	sum = sha256.Sum256([]byte(n))
	apple = ti.Sign(oidc.Apple, oidctest.TokenFields{Sub: "001234.abcd", Aud: testAppleAud, Nonce: hex.EncodeToString(sum[:])})
	wantStatus(t, "apple sign in", e.signIn(t, "apple", apple, n), 200, "")

	// A disabled account cannot sign in, like with its password.
	member, _ := e.users.GetByLogin(t.Context(), "member")
	if err := e.users.SetStatus(t.Context(), member.ID, users.StatusDisabled, users.Console); err != nil {
		t.Fatal(err)
	}
	n = e.nonce(t)
	wantStatus(t, "disabled", e.signIn(t, "google", googleToken(ti, "g-member", n), n), 401, "invalid_credentials")
	if err := e.users.SetStatus(t.Context(), member.ID, users.StatusActive, users.Console); err != nil {
		t.Fatal(err)
	}
	access, _ = e.mustLogin(t, "member", userPassword)

	// Unlink.
	wantStatus(t, "unlink", e.call(t, "DELETE", "/api/v1/me/sign-ins/google", nil, bearer(access)), 204, "")
	wantStatus(t, "unlink again", e.call(t, "DELETE", "/api/v1/me/sign-ins/google", nil, bearer(access)), 404, "not_found")
	wantStatus(t, "unlink unknown", e.call(t, "DELETE", "/api/v1/me/sign-ins/facebook", nil, bearer(access)), 404, "not_found")
	n = e.nonce(t)
	wantStatus(t, "after unlink", e.signIn(t, "google", googleToken(ti, "g-member", n), n), 401, "identity_not_linked")
	// Now the administrator may take that identity.
	n = e.nonce(t)
	wantStatus(t, "admin links it", e.link(t, adminAccess, "google", googleToken(ti, "g-member", n), n, adminPassword), 201, "")

	// Audit: links, unlinks and sign-ins with their method; no token or
	// subject in any detail.
	rows, err := e.db.QueryContext(t.Context(), "SELECT action, result, COALESCE(details_json, '') FROM audit_log WHERE action IN ('account.identity_link', 'account.identity_unlink', 'auth.login') ORDER BY occurred_at")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]int{}
	for rows.Next() {
		var action, result, details string
		if err := rows.Scan(&action, &result, &details); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(details, "g-member") || strings.Contains(details, "001234") || strings.Contains(details, "eyJ") {
			t.Fatalf("audit %s holds identity material: %s", action, details)
		}
		if strings.Contains(details, `"method":"google"`) || strings.Contains(details, `"method":"apple"`) {
			action += "/" + result + "/method"
		} else if strings.Contains(details, `"reason":"`) {
			action += "/" + result + "/reason"
		} else {
			action += "/" + result
		}
		seen[action]++
	}
	for _, want := range []string{"account.identity_link/allowed", "account.identity_link/denied/reason", "account.identity_unlink/allowed",
		"auth.login/allowed/method", "auth.login/denied/method"} {
		if seen[want] == 0 {
			t.Errorf("no audit row %s (have %v)", want, seen)
		}
	}
}

// An administrator sees an account's links and can remove one (a lost
// Google account), but never add one.
func TestSignInAdmin(t *testing.T) {
	e, ti := newSignInEnv(t, true, ratelimit.Config{IPBurst: 1000, PairFailures: 100, AccountFailures: 100})
	member, _ := e.users.GetByLogin(t.Context(), "member")
	access, _ := e.mustLogin(t, "member", userPassword)
	n := e.nonce(t)
	wantStatus(t, "link", e.link(t, access, "google", googleToken(ti, "g-member", n), n, userPassword), 201, "")
	adminAccess, _ := e.mustLogin(t, "root", adminPassword)
	path := "/api/v1/admin/users/" + member.ID + "/sign-ins"

	wantStatus(t, "member cannot", e.call(t, "GET", path, nil, bearer(access)), 403, "forbidden")
	list := e.call(t, "GET", path, nil, bearer(adminAccess))
	wantStatus(t, "admin list", list, 200, "")
	items := list.body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["email"] != "g-member@gmail.example" || strings.Contains(list.raw, "g-member\"") {
		t.Fatalf("admin list %s", list.raw)
	}
	wantStatus(t, "unknown user", e.call(t, "GET", "/api/v1/admin/users/nobody/sign-ins", nil, bearer(adminAccess)), 404, "user_not_found")
	wantStatus(t, "no admin link route", e.call(t, "POST", path, map[string]string{}, bearer(adminAccess)), 404, "not_found")

	wantStatus(t, "member cannot unlink", e.call(t, "DELETE", path+"/google", nil, bearer(access)), 403, "forbidden")
	wantStatus(t, "admin unlink", e.call(t, "DELETE", path+"/google", nil, bearer(adminAccess)), 204, "")
	wantStatus(t, "admin unlink again", e.call(t, "DELETE", path+"/google", nil, bearer(adminAccess)), 404, "not_found")
	wantStatus(t, "admin unlink unknown provider", e.call(t, "DELETE", path+"/facebook", nil, bearer(adminAccess)), 404, "not_found")
	n = e.nonce(t)
	wantStatus(t, "after admin unlink", e.signIn(t, "google", googleToken(ti, "g-member", n), n), 401, "identity_not_linked")

	// The audit names the administrator as actor and the member as target.
	root, _ := e.users.GetByLogin(t.Context(), "root")
	var actor, target string
	if err := e.db.QueryRowContext(t.Context(), "SELECT actor_user_id, target_id FROM audit_log WHERE action = 'account.identity_unlink'").Scan(&actor, &target); err != nil {
		t.Fatal(err)
	}
	if actor != root.ID || target != member.ID {
		t.Fatalf("audit actor %s target %s", actor, target)
	}
}

func TestSignInProviderOff(t *testing.T) {
	e, ti := newSignInEnv(t, false, ratelimit.Config{})
	r := e.call(t, "POST", "/api/v1/auth/sign-in/nonce", map[string]string{}, nil)
	if fmt.Sprint(r.body["providers"]) != "[apple]" {
		t.Fatalf("providers %s", r.raw)
	}
	n := e.nonce(t)
	wantStatus(t, "google off", e.signIn(t, "google", googleToken(ti, "g", n), n), 400, "provider_disabled")
	access, _ := e.mustLogin(t, "member", userPassword)
	n = e.nonce(t)
	wantStatus(t, "link google off", e.link(t, access, "google", googleToken(ti, "g", n), n, userPassword), 400, "provider_disabled")
	list := e.call(t, "GET", "/api/v1/me/sign-ins", nil, bearer(access))
	if fmt.Sprint(list.body["items"]) != "[]" || fmt.Sprint(list.body["providers"]) != "[apple]" {
		t.Fatalf("list %s", list.raw)
	}
}

func TestSignInFailuresAreRateLimited(t *testing.T) {
	e, ti := newSignInEnv(t, true, ratelimit.Config{IPBurst: 100, PairFailures: 3, AccountFailures: 100})
	for i := range 3 {
		n := e.nonce(t)
		wantStatus(t, fmt.Sprint("bad ", i), e.signIn(t, "google", "bad.token.here", n), 401, "invalid_identity_token")
	}
	n := e.nonce(t)
	wantStatus(t, "blocked", e.signIn(t, "google", googleToken(ti, "g", n), n), 429, "rate_limited")
	// The password login of every account is unaffected.
	e.mustLogin(t, "member", userPassword)
}

func TestSignInNonceIsMissingOrUnknown(t *testing.T) {
	e, ti := newSignInEnv(t, true, ratelimit.Config{})
	wantStatus(t, "no nonce", e.signIn(t, "google", googleToken(ti, "g", "x"), ""), 400, "invalid_request")
	wantStatus(t, "no token", e.signIn(t, "google", "", e.nonce(t)), 400, "invalid_request")
	wantStatus(t, "long nonce", e.signIn(t, "google", googleToken(ti, "g", "x"), strings.Repeat("n", 200)), 401, "invalid_identity_token")
	wantStatus(t, "not json", e.call(t, "POST", "/api/v1/auth/sign-in", nil, map[string]string{"Content-Type": "text/plain"}), 415, "json_required")
}
