package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
)

// Setup mode (M24): the browser setup screen that registers the storage root
// and creates the first administrator, on the machine itself. Threat model:
// SECURITY.md "Setup screen".
//
// Setup mode is a separate handler with its own route table
// (setupRoutePolicies). It is served only by the plain loopback listener of
// a server that has no registered storage or no administrator yet
// (server/setup.go). The normal Handler never registers a setup route, so
// after the switch to normal mode, and on every TLS or LAN listener, /setup
// is 404. Nothing but the page is answered before the one-time setup code
// printed on the console has been entered.

//go:embed web/setup.html
var setupHTML []byte

// setupCSP: like dashboardCSP, the page's one inline script and style by hash.
var setupCSP = "default-src 'none'; script-src " + inlineHash(setupHTML, "script") +
	"; style-src " + inlineHash(setupHTML, "style") +
	"; connect-src 'self'; img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'" +
	"; require-trusted-types-for 'script'; trusted-types 'none'"

const (
	// SetupCodeTTL is how long the setup code printed at startup is valid.
	SetupCodeTTL = 30 * time.Minute
	// SetupMaxFailures wrong entries disable the setup code until restart.
	SetupMaxFailures = 5
	// setupSessionTTL bounds the setup session opened by the code.
	setupSessionTTL = time.Hour
	// setupAlphabet has 32 letters (5 bits each) without 0/O and 1/I.
	setupAlphabet   = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	setupCodeLen    = 12 // 60 bits
	setupCookie     = "withalbum_setup"
	setupCSRFHeader = "X-Setup-CSRF"
	// SetupFolderName is the one folder the setup screen may create on a
	// disk the user selected.
	SetupFolderName = "WithAlbum"
)

// Access levels of setup routes. They never appear in routePolicies.
const (
	// AccessSetupPublic: the setup page, the redirect to it, unknown paths
	// (404) and entering the setup code.
	AccessSetupPublic Access = "setup-public"
	// AccessSetup: the setup session opened by the code, plus its CSRF token
	// for POST.
	AccessSetup Access = "setup"
)

// setupRoutePolicies is the whole route table of setup mode.
var setupRoutePolicies = []RoutePolicy{
	{"GET /{$}", AccessSetupPublic, ""},
	{"GET /setup", AccessSetupPublic, ""},
	{"/", AccessSetupPublic, ""},
	{"POST /setup/api/unlock", AccessSetupPublic, ""},
	{"GET /setup/api/state", AccessSetup, ""},
	{"GET /setup/api/volumes", AccessSetup, ""},
	{"POST /setup/api/storage/check", AccessSetup, ""},
	{"POST /setup/api/storage", AccessSetup, ""},
	{"POST /setup/api/admin", AccessSetup, ""},
}

// SetupRoutes returns the route table of setup mode (tests, documentation).
func SetupRoutes() []RoutePolicy { return slices.Clone(setupRoutePolicies) }

// SetupCode is the one-time code printed on the console. Only its SHA-256 is
// kept, in memory.
type SetupCode struct {
	sum      [32]byte
	expires  time.Time
	failures int
	used     bool
}

// NewSetupCode returns a fresh code valid for SetupCodeTTL and its display
// form (XXXX-XXXX-XXXX).
func NewSetupCode(now time.Time) (*SetupCode, string, error) {
	b := make([]byte, setupCodeLen)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	code := make([]byte, setupCodeLen)
	for i, x := range b {
		code[i] = setupAlphabet[x&31] // 256 is a multiple of 32: uniform
	}
	plain := string(code)
	return &SetupCode{sum: sha256.Sum256([]byte(plain)), expires: now.Add(SetupCodeTTL)},
		plain[0:4] + "-" + plain[4:8] + "-" + plain[8:12], nil
}

func normalizeSetupCode(s string) string {
	s = strings.ToUpper(s)
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

// SetupStorage is the registered storage as the setup screen shows it.
type SetupStorage struct {
	Root     string
	Name     string
	Kind     string // fixed, system, disk, custom
	FS       storage.FSInfo
	Contents storage.Contents
}

// DescribeStorage describes a registered storage root for the setup screen
// (used when the server starts with a storage but no administrator).
func DescribeStorage(root string) *SetupStorage {
	st := &SetupStorage{Root: root, Name: filepath.Base(root), Kind: "fixed", Contents: storage.ContentsRegistered}
	st.FS, _ = storage.StatFS(root)
	return st
}

// SetupRefusal is an error of Register that the setup screen explains with
// Code (for example "legacy_metadata" or "locked").
type SetupRefusal struct {
	Code string
	Err  error
}

func (e *SetupRefusal) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *SetupRefusal) Unwrap() error { return e.Err }

// SetupConfig wires setup mode to the server.
type SetupConfig struct {
	Code *SetupCode
	// FixedRoot: WITHALBUM_ROOT was given; it is the only directory offered
	// and nothing is created.
	FixedRoot string
	// Home is the base of the system-disk choice (a WithAlbum folder in the
	// home folder); "" offers none.
	Home string
	// VolumesDir lists the other disks ("/Volumes" on macOS; "" none).
	VolumesDir string
	// IsMountPoint checks a disk right before a folder is created on it
	// (nil: storage.IsMountPoint).
	IsMountPoint func(string) (bool, error)
	// Storage is set when the server started with a registered storage
	// whose administrator was not set up: setup starts at the admin step.
	Storage *SetupStorage
	// Register registers root (storage.Init unless already registered) and
	// opens it with the normal startup sequence, keeping it open. adminSet
	// reports an administrator already exists (setup then ends).
	Register func(ctx context.Context, root string) (adminSet bool, err error)
	// Claim takes over the locked bootstrap administrator (ClaimBootstrap).
	Claim func(ctx context.Context, c users.Claim) error
	// Activate switches the server to normal mode. Called once, after the
	// administrator exists.
	Activate       func()
	PasswordParams password.Params // zero: password.DefaultParams
	Now            func() time.Time
}

type setupSession struct {
	sum     [32]byte
	csrf    string
	expires time.Time
}

// Setup serves setup mode.
type Setup struct {
	cfg     SetupConfig
	mu      sync.Mutex // serializes every request that reads or changes the state
	step    string     // storage, admin, done
	session *setupSession
	storage *SetupStorage
}

// NewSetup returns the setup handler for cfg.
func NewSetup(cfg SetupConfig) *Setup {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IsMountPoint == nil {
		cfg.IsMountPoint = storage.IsMountPoint
	}
	if cfg.PasswordParams == (password.Params{}) {
		cfg.PasswordParams = password.DefaultParams
	}
	s := &Setup{cfg: cfg, step: "storage"}
	if cfg.Storage != nil {
		s.storage, s.step = cfg.Storage, "admin"
	}
	return s
}

// Handler returns the routing table of setup mode.
func (s *Setup) Handler() http.Handler {
	if s.cfg.Code == nil || s.cfg.Register == nil || s.cfg.Claim == nil || s.cfg.Activate == nil {
		panic("httpapi: incomplete SetupConfig")
	}
	mux := http.NewServeMux()
	declared := map[string]Access{}
	for _, p := range setupRoutePolicies {
		declared[p.Pattern] = p.Access
	}
	done := map[string]bool{}
	reg := func(pattern string, h http.HandlerFunc) {
		access, ok := declared[pattern]
		if !ok || done[pattern] {
			panic("httpapi: setup route not declared or registered twice: " + pattern)
		}
		done[pattern] = true
		switch access {
		case AccessSetupPublic:
			mux.Handle(pattern, h)
		case AccessSetup:
			mux.Handle(pattern, s.requireSession(h))
		default:
			panic("httpapi: setup route " + pattern + " has access " + string(access))
		}
	}
	reg("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/setup", http.StatusSeeOther) })
	reg("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", setupCSP)
		_, _ = w.Write(setupHTML)
	})
	reg("/", func(w http.ResponseWriter, r *http.Request) { errorReply(w, 404, "not_found") })
	reg("POST /setup/api/unlock", s.unlock)
	reg("GET /setup/api/state", s.state)
	reg("GET /setup/api/volumes", s.volumes)
	reg("POST /setup/api/storage/check", s.checkStorage)
	reg("POST /setup/api/storage", s.useStorage)
	reg("POST /setup/api/admin", s.createAdmin)
	for p := range declared {
		if !done[p] {
			panic("httpapi: declared setup route has no handler: " + p)
		}
	}
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		mux.ServeHTTP(w, r)
	}))
}

// sameOrigin refuses requests a browser marks as coming from another site,
// and POST bodies that are not JSON (a cross-site form or a "simple" fetch
// cannot send application/json without a CORS preflight, which is never
// answered).
func sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		errorReply(w, 403, "cross_origin")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		errorReply(w, 403, "cross_origin")
		return false
	}
	if r.Method == http.MethodPost {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			errorReply(w, 415, "unsupported_media_type")
			return false
		}
	}
	return true
}

func (s *Setup) requireSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(w, r) {
			return
		}
		c, err := r.Cookie(setupCookie)
		s.mu.Lock()
		sess := s.session
		s.mu.Unlock()
		if err != nil || sess == nil || s.cfg.Now().After(sess.expires) {
			errorReply(w, 401, "setup_locked")
			return
		}
		sum := sha256.Sum256([]byte(c.Value))
		if subtle.ConstantTimeCompare(sum[:], sess.sum[:]) != 1 {
			errorReply(w, 401, "setup_locked")
			return
		}
		if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get(setupCSRFHeader)), []byte(sess.csrf)) != 1 {
			errorReply(w, 403, "csrf_failed")
			return
		}
		next(w, r)
	})
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// unlock: POST /setup/api/unlock {"code"}. The first correct code opens the
// one setup session and is consumed.
func (s *Setup) unlock(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(w, r) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, now := s.cfg.Code, s.cfg.Now()
	switch {
	case c.used:
		errorReply(w, 409, "setup_code_used")
		return
	case c.failures >= SetupMaxFailures:
		errorReply(w, 423, "setup_code_disabled")
		return
	case now.After(c.expires):
		errorReply(w, 410, "setup_code_expired")
		return
	}
	sum := sha256.Sum256([]byte(normalizeSetupCode(req.Code)))
	if subtle.ConstantTimeCompare(sum[:], c.sum[:]) != 1 {
		c.failures++
		left := SetupMaxFailures - c.failures
		log.Printf("setup: a wrong setup code was entered (%d of %d attempts used)", c.failures, SetupMaxFailures)
		if left == 0 {
			log.Printf("setup: the setup code is disabled after %d wrong attempts; restart the server to get a new one", SetupMaxFailures)
			errorReply(w, 423, "setup_code_disabled")
			return
		}
		jsonReply(w, 401, map[string]any{"error": "setup_code_invalid", "attempts_left": left})
		return
	}
	token, err := randomHex(32)
	if err != nil {
		internalError(w, "setup session", err)
		return
	}
	csrf, err := randomHex(32)
	if err != nil {
		internalError(w, "setup session", err)
		return
	}
	c.used = true
	s.session = &setupSession{sum: sha256.Sum256([]byte(token)), csrf: csrf, expires: now.Add(setupSessionTTL)}
	http.SetCookie(w, &http.Cookie{Name: setupCookie, Value: token, Path: "/setup", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil, MaxAge: int(setupSessionTTL / time.Second)})
	log.Printf("setup: the setup code was accepted; continue in the browser")
	body := s.stateLocked()
	body["csrf_token"] = csrf
	jsonReply(w, 200, body)
}

func (s *Setup) state(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body := s.stateLocked()
	if s.session != nil {
		// Readable only by a same-origin page that already holds the
		// session cookie, so a reload of the page can continue.
		body["csrf_token"] = s.session.csrf
	}
	jsonReply(w, 200, body)
}

func optBytes(known bool, n uint64) *uint64 {
	if !known {
		return nil
	}
	return &n
}

func (s *Setup) stateLocked() map[string]any {
	body := map[string]any{"step": s.step, "fixed_root": s.cfg.FixedRoot, "storage": nil, "code_expires_at": ts(s.cfg.Code.expires)}
	if st := s.storage; st != nil {
		body["storage"] = map[string]any{"root": st.Root, "name": st.Name, "kind": st.Kind, "fs_type": st.FS.Type,
			"free_bytes": optBytes(st.FS.Known, st.FS.FreeBytes), "total_bytes": optBytes(st.FS.Known, st.FS.TotalBytes), "contents": st.Contents}
	}
	return body
}

// setupCandidate is one storage choice.
type setupCandidate struct {
	Kind       string  `json:"kind"` // fixed, system, disk, custom
	Name       string  `json:"name"`
	Base       string  `json:"base"` // where the WithAlbum folder may be created ("" fixed/custom)
	Path       string  `json:"path"` // the storage root this choice registers
	Mounted    bool    `json:"mounted"`
	FSType     string  `json:"fs_type"`
	TotalBytes *uint64 `json:"total_bytes"`
	FreeBytes  *uint64 `json:"free_bytes"`
	ReadOnly   bool    `json:"read_only"`
	Network    bool    `json:"network"`
	Exists     bool    `json:"exists"`   // Path exists
	Contents   string  `json:"contents"` // what Path holds, when it exists
	Problem    string  `json:"problem"`  // "" when it can be chosen
}

// describe fills in the file system, contents and problem of c.
func (s *Setup) describe(c setupCandidate) setupCandidate {
	at := c.Path
	if ok, err := storage.IsDir(c.Path); err == nil && ok {
		c.Exists = true
	} else if c.Base != "" {
		at = c.Base
	}
	if fs, err := storage.StatFS(at); err == nil {
		c.FSType, c.ReadOnly, c.Network = fs.Type, fs.ReadOnly, !fs.Local
		c.TotalBytes, c.FreeBytes = optBytes(true, fs.TotalBytes), optBytes(true, fs.FreeBytes)
	}
	if c.Exists {
		if contents, err := storage.ReadContents(c.Path); err == nil {
			c.Contents = string(contents)
		}
	}
	switch {
	case c.Kind == "disk" && !c.Mounted:
		c.Problem = "not_mounted"
	case !c.Exists && c.Base == "":
		c.Problem = "missing"
	case c.ReadOnly:
		c.Problem = "read_only"
	case c.Network:
		c.Problem = "network"
	case c.Contents == string(storage.ContentsBackupTarget):
		c.Problem = "backup_target"
	}
	return c
}

// macSystemVolumes are APFS helper volumes of macOS that may appear under
// /Volumes; they are never a place for photos.
var macSystemVolumes = map[string]bool{"Recovery": true, "Preboot": true, "VM": true, "Update": true, "xarts": true,
	"iSCPreboot": true, "Hardware": true}

// candidates lists the choices: the fixed root only, or the system disk
// (a folder in the home folder) and every entry of VolumesDir except the
// system disk itself ("/", or the volume that holds the home folder).
func (s *Setup) candidates() []setupCandidate {
	if s.cfg.FixedRoot != "" {
		return []setupCandidate{s.describe(setupCandidate{Kind: "fixed", Name: filepath.Base(s.cfg.FixedRoot), Path: s.cfg.FixedRoot})}
	}
	var out []setupCandidate
	if s.cfg.Home != "" {
		if ok, err := storage.IsDir(s.cfg.Home); err == nil && ok {
			out = append(out, s.describe(setupCandidate{Kind: "system", Name: filepath.Base(s.cfg.Home), Base: s.cfg.Home,
				Path: filepath.Join(s.cfg.Home, SetupFolderName), Mounted: true}))
		}
	}
	if s.cfg.VolumesDir != "" {
		mounts, err := storage.ListMounts(s.cfg.VolumesDir)
		if err != nil {
			log.Printf("setup: cannot list %s: %v", s.cfg.VolumesDir, err)
		}
		for _, m := range mounts {
			if m.Path == "/" || (s.cfg.Home != "" && strings.HasPrefix(s.cfg.Home+"/", m.Path+"/")) || macSystemVolumes[m.Name] {
				continue // the system disk, offered as the home folder above
			}
			mounted, err := s.cfg.IsMountPoint(m.Path)
			out = append(out, s.describe(setupCandidate{Kind: "disk", Name: m.Name, Base: m.Path,
				Path: filepath.Join(m.Path, SetupFolderName), Mounted: err == nil && mounted}))
		}
	}
	return out
}

func (s *Setup) volumes(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.candidates()
	if list == nil {
		list = []setupCandidate{}
	}
	jsonReply(w, 200, map[string]any{"candidates": list, "custom_allowed": s.cfg.FixedRoot == ""})
}

type storageChoice struct {
	Path         string `json:"path"`
	Base         string `json:"base"`
	CreateFolder bool   `json:"create_folder"`
}

// resolveChoice matches a request against the current candidates; a custom
// path (no base) must be an existing absolute directory. Paths are compared
// after resolving symlinks.
func (s *Setup) resolveChoice(req storageChoice) (setupCandidate, string) {
	if req.Base != "" {
		for _, c := range s.candidates() {
			if c.Base != "" && c.Base == req.Base && c.Path == req.Path {
				return c, ""
			}
		}
		return setupCandidate{}, "unknown_choice"
	}
	if !filepath.IsAbs(req.Path) || len(req.Path) > 4096 || strings.ContainsRune(req.Path, 0) {
		return setupCandidate{}, "invalid_path"
	}
	if s.cfg.FixedRoot != "" {
		if same, _ := samePath(req.Path, s.cfg.FixedRoot); !same {
			return setupCandidate{}, "unknown_choice"
		}
		return s.candidates()[0], ""
	}
	resolved, err := storage.ResolveDir(req.Path)
	if err != nil {
		return setupCandidate{}, "missing"
	}
	return s.describe(setupCandidate{Kind: "custom", Name: filepath.Base(resolved), Path: resolved, Mounted: true}), ""
}

func samePath(a, b string) (bool, error) {
	ra, err := storage.ResolveDir(a)
	if err != nil {
		return false, err
	}
	rb, err := storage.ResolveDir(b)
	if err != nil {
		return false, err
	}
	return ra == rb, nil
}

// checkStorage: POST /setup/api/storage/check — the choice and a write test
// (in the folder, or on the disk when the folder does not exist yet).
func (s *Setup) checkStorage(w http.ResponseWriter, r *http.Request) {
	var req storageChoice
	if !decodeJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, problem := s.resolveChoice(req)
	if problem != "" {
		errorReply(w, 400, problem)
		return
	}
	writable, writeProblem := false, ""
	if c.Problem == "" {
		at := c.Path
		if !c.Exists {
			at = c.Base
		}
		if err := storage.ProbeWritable(at); err != nil {
			writeProblem = "not_writable"
		} else {
			writable = true
		}
	}
	jsonReply(w, 200, map[string]any{"candidate": c, "writable": writable, "write_problem": writeProblem})
}

// useStorage: POST /setup/api/storage — create the WithAlbum folder when
// asked (only directly on a selected, mounted disk or in the home folder),
// write-test it, register and open it.
func (s *Setup) useStorage(w http.ResponseWriter, r *http.Request) {
	var req storageChoice
	if !decodeJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step != "storage" {
		errorReply(w, 409, "storage_already_set")
		return
	}
	c, problem := s.resolveChoice(req)
	if problem != "" {
		errorReply(w, 400, problem)
		return
	}
	if c.Problem != "" {
		errorReply(w, 409, c.Problem)
		return
	}
	if !c.Exists {
		if !req.CreateFolder || c.Base == "" {
			errorReply(w, 409, "folder_missing")
			return
		}
		var err error
		switch c.Kind {
		case "disk":
			// Re-check right before creating: the disk may have been unplugged
			// since it was listed, leaving its path on the internal disk.
			ok, merr := s.cfg.IsMountPoint(c.Base)
			if merr != nil || !ok {
				errorReply(w, 409, "not_mounted")
				return
			}
			_, err = storage.MkdirOne(c.Base, SetupFolderName)
		case "system":
			_, err = storage.MkdirOne(c.Base, SetupFolderName)
		default:
			errorReply(w, 409, "folder_missing")
			return
		}
		if err != nil {
			log.Printf("setup: cannot create %s: %v", c.Path, err)
			errorReply(w, 409, "not_writable")
			return
		}
		log.Printf("setup: created the folder %s", c.Path)
		c = s.describe(c)
	}
	if err := storage.ProbeWritable(c.Path); err != nil {
		errorReply(w, 409, "not_writable")
		return
	}
	adminSet, err := s.cfg.Register(r.Context(), c.Path)
	if err != nil {
		log.Printf("setup: cannot register %s: %v", c.Path, err)
		var refusal *SetupRefusal
		if errors.As(err, &refusal) {
			errorReply(w, 409, refusal.Code)
			return
		}
		errorReply(w, 500, "storage_failed")
		return
	}
	st := &SetupStorage{Root: c.Path, Name: c.Name, Kind: c.Kind, Contents: storage.Contents(c.Contents)}
	st.FS, _ = storage.StatFS(c.Path)
	s.storage = st
	s.step = "admin"
	if adminSet {
		s.finishLocked(w)
	}
	jsonReply(w, 200, s.stateLocked())
}

// finishLocked ends setup mode: the session is dropped and the server
// switches to normal mode.
func (s *Setup) finishLocked(w http.ResponseWriter) {
	s.step = "done"
	s.session = nil
	http.SetCookie(w, &http.Cookie{Name: setupCookie, Value: "", Path: "/setup", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	s.cfg.Activate()
}

// createAdmin: POST /setup/api/admin — the same takeover of the bootstrap
// administrator as "admin init", then the switch to normal mode.
func (s *Setup) createAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoginName       string `json:"login_name"`
		DisplayName     string `json:"display_name"`
		Password        string `json:"password"`
		PasswordConfirm string `json:"password_confirm"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.LoginName = strings.TrimSpace(req.LoginName)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.DisplayName == "" {
		req.DisplayName = req.LoginName
	}
	switch {
	case users.ValidateLoginName(req.LoginName) != nil:
		errorReply(w, 400, "invalid_login_name")
		return
	case users.ValidateDisplayName(req.DisplayName) != nil:
		errorReply(w, 400, "invalid_display_name")
		return
	case password.CheckPolicy(req.Password) != nil:
		errorReply(w, 400, "invalid_password")
		return
	case subtle.ConstantTimeCompare([]byte(req.Password), []byte(req.PasswordConfirm)) != 1:
		errorReply(w, 400, "password_mismatch")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.step != "admin" {
		errorReply(w, 409, "storage_required")
		return
	}
	h, err := password.Hash(req.Password, s.cfg.PasswordParams)
	if err != nil {
		internalError(w, "setup password hash", err)
		return
	}
	err = s.cfg.Claim(r.Context(), users.Claim{LoginName: req.LoginName, DisplayName: req.DisplayName, PasswordHash: h})
	switch {
	case errors.Is(err, users.ErrLoginTaken):
		errorReply(w, 409, "login_taken")
		return
	case errors.Is(err, users.ErrAlreadyInitialized):
		errorReply(w, 409, "already_initialized")
		return
	case err != nil:
		internalError(w, "setup administrator", err)
		return
	}
	log.Printf("setup: administrator %q created; switching to normal mode", req.LoginName)
	s.finishLocked(w)
	jsonReply(w, 200, map[string]any{"step": "done", "login_name": req.LoginName})
}

// Expires returns the end of the code's validity.
func (c *SetupCode) Expires() time.Time { return c.expires }

// SetupBanner is the console text that shows the setup URL and code.
func SetupBanner(url, code string, expires time.Time) string {
	return fmt.Sprintf("SETUP: open %s on this Mac and enter the setup code  %s\n"+
		"SETUP: the code works once, for %d minutes (until %s), and is disabled after %d wrong entries; restart the server for a new one.",
		url, code, int(SetupCodeTTL/time.Minute), expires.Format("15:04"), SetupMaxFailures)
}
