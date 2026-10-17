//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/storage"
)

// crashChildEnv makes the test binary act as a server child process for
// TestSIGKILLDuringUpload. Its value is the fault point to stop at.
const crashChildEnv = "WITHALBUM_CRASH_CHILD"

func TestMain(m *testing.M) {
	if point := os.Getenv(crashChildEnv); point != "" {
		crashChild(point)
		return
	}
	os.Exit(m.Run())
}

// blockAt announces reaching point on stdout and then blocks forever, so the
// parent can SIGKILL the process exactly there.
type blockAt string

func (b blockAt) Fault(point string) error {
	if point == string(b) {
		fmt.Printf("AT %s\n", point)
		select {}
	}
	return nil
}

// crashChild runs the real startup sequence on WITHALBUM_ROOT and serves on
// a random loopback port until it is killed.
func crashChild(point string) {
	var faults httpapi.FaultInjector
	if point != "mid_body" {
		faults = blockAt(point)
	}
	a, err := openApp(context.Background(), os.Getenv("WITHALBUM_ROOT"), appOptions{MinFreeBytes: 0, Faults: faults})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child:", err)
		os.Exit(3)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "child:", err)
		os.Exit(3)
	}
	fmt.Printf("LISTEN %s\n", ln.Addr())
	_ = http.Serve(ln, a.api.Handler())
	os.Exit(4)
}

func newRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	// storage.Open resolves symlinks (macOS /var -> /private/var); use the
	// same spelling everywhere.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func mustOpenApp(t *testing.T, root string, faults httpapi.FaultInjector) *app {
	t.Helper()
	a, err := openApp(t.Context(), root, appOptions{Faults: faults})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// testPhoto returns a decodable JPEG unique to seed, so that the preview
// step really runs.
func testPhoto(t *testing.T, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for i := range img.Pix {
		img.Pix[i] = byte(i*7 + seed)
	}
	img.Set(0, 0, color.RGBA{byte(seed), 1, 2, 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func serve(h http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func filesIn(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// recovered describes the state expected after a crash at some point.
type recovered struct {
	incomingRemoved int  // unfinished temp files deleted at startup
	orphans         int  // unreferenced pool files quarantined at startup
	committed       bool // the asset and its event are visible
}

// checkRecovered restarts the server on root and verifies the invariants:
// startup reconciliation brings DB and pool in line (no missing or damaged
// original, nothing left in incoming), an uncommitted upload left no event,
// a committed one is complete, the client's retry succeeds exactly once,
// and a second restart finds nothing to repair.
func checkRecovered(t *testing.T, root, folderID string, body []byte, want recovered) {
	t.Helper()
	a := mustOpenApp(t, root, nil)
	r := a.report
	if !r.Healthy() || r.IncomingRemoved != want.incomingRemoved || r.Orphans != want.orphans {
		a.Close()
		t.Fatalf("after restart: %s, want incoming_removed=%d orphans=%d", r.Summary(), want.incomingRemoved, want.orphans)
	}
	if n := filesIn(t, filepath.Join(root, ".system", "incoming")); n != 0 {
		t.Fatalf("incoming holds %d files after restart", n)
	}
	if n := filesIn(t, filepath.Join(root, filepath.FromSlash(blobstore.OrphansRelDir))); n != want.orphans {
		t.Fatalf("orphans holds %d files, want %d", n, want.orphans)
	}
	h := a.api.Handler()
	events := func() int {
		page, err := a.meta.ListEvents(t.Context(), folderID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Events)
	}
	sha := shaHex(body)
	if want.committed {
		if n := events(); n != 1 {
			t.Fatalf("committed upload: %d events", n)
		}
		if w := serve(h, "GET", "/api/v1/assets/"+sha+"/original", nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body) {
			t.Fatalf("committed original: %d", w.Code)
		}
	} else {
		if n := events(); n != 0 {
			t.Fatalf("uncommitted upload left %d events", n)
		}
		if w := serve(h, "GET", "/api/v1/assets/"+sha+"/original", nil); w.Code != 404 {
			t.Fatalf("uncommitted original visible: %d", w.Code)
		}
	}
	// The client did not get a success, so it retries.
	wantStatus := 201
	if want.committed {
		wantStatus = 200
	}
	if w := serve(h, "POST", "/api/v1/folders/"+folderID+"/assets", body); w.Code != wantStatus {
		t.Fatalf("retry: %d %s, want %d", w.Code, w.Body, wantStatus)
	}
	if n := events(); n != 1 {
		t.Fatalf("after retry: %d events, want 1", n)
	}
	if w := serve(h, "GET", "/api/v1/assets/"+sha+"/preview", nil); w.Code != 200 {
		t.Fatalf("preview after retry: %d", w.Code)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = mustOpenApp(t, root, nil)
	defer a.Close()
	if a.report.Changed() || !a.report.Healthy() || a.report.Blobs != 1 {
		t.Fatalf("second restart: %s", a.report.Summary())
	}
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newFolder creates a folder through a short-lived app and returns its id.
func newFolder(t *testing.T, root string) string {
	t.Helper()
	a := mustOpenApp(t, root, nil)
	defer a.Close()
	f, err := a.meta.CreateFolder(t.Context(), "crash")
	if err != nil {
		t.Fatal(err)
	}
	return f.ID
}

// crashPoints lists, for each fault point, what a crash right there leaves.
var crashPoints = []struct {
	point string
	want  recovered
}{
	{httpapi.FaultAfterStage, recovered{incomingRemoved: 1}},
	{httpapi.FaultAfterPromote, recovered{orphans: 1}},
	{httpapi.FaultAfterPreview, recovered{orphans: 1}},
	{httpapi.FaultBeforeCommit, recovered{orphans: 1}},
	{httpapi.FaultAfterCommit, recovered{committed: true}},
}

// snapshotAt copies the whole storage root when the upload reaches point,
// producing the on-disk image a crash at that instant would leave (every
// earlier step has been fsynced), and then fails the upload.
type snapshotAt struct {
	t     *testing.T
	point string
	src   string
	dst   string
	taken bool
}

var errCrash = errors.New("simulated crash")

func (s *snapshotAt) Fault(point string) error {
	if point != s.point {
		return nil
	}
	s.taken = true
	if err := copyTree(s.src, s.dst); err != nil {
		s.t.Errorf("snapshot: %v", err)
	}
	return errCrash
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, b, 0600); err != nil {
			return err
		}
		return os.Chmod(target, info.Mode().Perm())
	})
}

// TestUploadFaultRecovery stops an upload after each step. The live server
// answers 5xx; the crash image taken at that step and the live root both
// recover to a consistent state on restart.
func TestUploadFaultRecovery(t *testing.T) {
	for i, cp := range crashPoints {
		t.Run(cp.point, func(t *testing.T) {
			root := newRoot(t)
			folderID := newFolder(t, root)
			snap := filepath.Join(t.TempDir(), "image")
			faults := &snapshotAt{t: t, point: cp.point, src: root, dst: snap}
			a := mustOpenApp(t, root, faults)
			body := testPhoto(t, i)
			w := serve(a.api.Handler(), "POST", "/api/v1/folders/"+folderID+"/assets", body)
			if w.Code < 500 {
				t.Fatalf("upload with a fault at %s: %d %s, want 5xx", cp.point, w.Code, w.Body)
			}
			if !faults.taken {
				t.Fatalf("fault point %s not reached", cp.point)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			t.Run("crash image", func(t *testing.T) { checkRecovered(t, snap, folderID, body, cp.want) })
			// The live process cleaned its own temp file before replying.
			live := cp.want
			live.incomingRemoved = 0
			t.Run("live root", func(t *testing.T) { checkRecovered(t, root, folderID, body, live) })
		})
	}
}

// TestPreviewFailureDoesNotFailUpload: a failed preview still commits the
// original; preview_ready is false.
func TestPreviewFailureDoesNotFailUpload(t *testing.T) {
	root := newRoot(t)
	folderID := newFolder(t, root)
	a := mustOpenApp(t, root, failAt(httpapi.FaultPreview))
	defer a.Close()
	w := serve(a.api.Handler(), "POST", "/api/v1/folders/"+folderID+"/assets", testPhoto(t, 99))
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"preview_ready":false`) {
		t.Fatalf("upload with failed preview: %d %s", w.Code, w.Body)
	}
}

type failAt string

func (f failAt) Fault(point string) error {
	if point == string(f) {
		return errCrash
	}
	return nil
}

// TestStartupRefusesSecondInstance: a second server (or migrate-legacy
// -apply) on the same volume is refused while the first one runs.
func TestStartupRefusesSecondInstance(t *testing.T) {
	root := newRoot(t)
	a := mustOpenApp(t, root, nil)
	if _, err := openApp(t.Context(), root, appOptions{}); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("second openApp = %v, want ErrLocked", err)
	}
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	var stdout, stderr bytes.Buffer
	if code := runMigrateLegacy([]string{"-apply"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "LOCKED") {
		t.Fatalf("migrate-legacy -apply while serving: exit %d %s", code, stderr.String())
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := mustOpenApp(t, root, nil)
	b.Close()
}

// TestStartupReportsMissingOriginal: a database row whose pool file is gone
// starts the server with an integrity warning in /api/v1/health.
func TestStartupReportsMissingOriginal(t *testing.T) {
	root := newRoot(t)
	folderID := newFolder(t, root)
	a := mustOpenApp(t, root, nil)
	body := testPhoto(t, 7)
	if w := serve(a.api.Handler(), "POST", "/api/v1/folders/"+folderID+"/assets", body); w.Code != 201 {
		t.Fatalf("upload %d", w.Code)
	}
	a.Close()
	if err := os.Remove(filepath.Join(root, ".system", "pool", shaHex(body)[:2], shaHex(body))); err != nil {
		t.Fatal(err)
	}
	a = mustOpenApp(t, root, nil)
	defer a.Close()
	if a.report.Missing != 1 || a.report.Healthy() {
		t.Fatalf("report %s", a.report.Summary())
	}
	if w := serve(a.api.Handler(), "GET", "/api/v1/health", nil); !strings.Contains(w.Body.String(), `"integrity":"warning"`) {
		t.Fatalf("health %s", w.Body)
	}
}

func TestMinFreeBytes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"", 1 << 30, true},
		{"0", 0, true},
		{" 2048 ", 2048, true},
		{"-1", 0, false},
		{"1GB", 0, false},
	} {
		got, err := minFreeBytes(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("minFreeBytes(%q) = %d, %v", tc.in, got, err)
		}
	}
}

// TestSIGKILLDuringUpload runs the real server as a child process, kills it
// with SIGKILL in the middle of an upload (while the body streams in, and at
// every fault point), restarts on the same root and checks consistency.
func TestSIGKILLDuringUpload(t *testing.T) {
	points := append([]struct {
		point string
		want  recovered
	}{{"mid_body", recovered{incomingRemoved: 1}}}, crashPoints...)
	for i, cp := range points {
		t.Run(cp.point, func(t *testing.T) {
			root := newRoot(t)
			folderID := newFolder(t, root)
			body := testPhoto(t, 100+i)
			if cp.point == "mid_body" {
				body = append(body, bytes.Repeat([]byte{0xAB}, 256<<10)...)
			}
			killDuringUpload(t, root, folderID, cp.point, body)
			checkRecovered(t, root, folderID, body, cp.want)
		})
	}
}

func killDuringUpload(t *testing.T, root, folderID, point string, body []byte) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), crashChildEnv+"="+point, "WITHALBUM_ROOT="+root, "WITHALBUM_VOLUME_ID=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed {
			cmd.Process.Kill()
			cmd.Wait()
		}
		if t.Failed() {
			t.Logf("child stderr:\n%s", stderr.String())
		}
	}()
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitLine := func(prefix string) string {
		t.Helper()
		timeout := time.After(60 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("child exited before %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return strings.TrimPrefix(l, prefix)
				}
			case <-timeout:
				t.Fatalf("timed out waiting for %q", prefix)
			}
		}
	}
	addr := waitLine("LISTEN ")

	pr, pw := io.Pipe()
	req, err := http.NewRequest("POST", "http://"+addr+"/api/v1/folders/"+folderID+"/assets", pr)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var status int
	var reqErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			reqErr = err
			return
		}
		status = res.StatusCode
		res.Body.Close()
	}()
	if point == "mid_body" {
		half := len(body) / 2
		if _, err := pw.Write(body[:half]); err != nil {
			t.Fatal(err)
		}
		waitForIncoming(t, root)
	} else {
		go func() {
			pw.Write(body)
			pw.Close()
		}()
		waitLine("AT " + point)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed = true
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("child did not die from SIGKILL: %v", err)
	}
	pw.CloseWithError(errors.New("server killed"))
	wg.Wait()
	if reqErr == nil {
		t.Fatalf("upload got a response (%d) although the server was killed before replying", status)
	}
}

// waitForIncoming waits until the child has written part of the body to an
// upload temp file.
func waitForIncoming(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, ".system", "incoming")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if info, err := e.Info(); err == nil && info.Size() > 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no partial upload appeared in incoming")
}
