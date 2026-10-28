//go:build darwin

// Command chaos is the macOS chaos test of WithAlbum Server (run it with
// scripts/chaos-macos.sh, which builds the binaries and cleans up).
//
// It stores the library on an hdiutil sparse disk image that stands in for
// an external disk, runs the real server binary (built with -tags chaos so
// that it can stop at upload fault points) and, round after round, injects
// one failure while uploads are in flight:
//
//   - SIGKILL in the middle of a request body, or at a fault point
//     (temp file written, original in the pool, inside the commit
//     transaction, committed but not answered),
//   - a power cut approximation: SIGKILL followed at once by a forced
//     detach of the disk (hdiutil detach -force, no sync),
//   - the disk detached during an upload, then reattached with the same or
//     with a different device number (st_dev),
//   - a full disk,
//   - a graceful stop (SIGTERM) during an upload.
//
// After every round it restarts the server and checks:
//
//   - "withalbum-server verify -hash" is healthy (database integrity, every
//     original present with its SHA-256),
//   - every upload the server acknowledged (2xx) is in the folder with the
//     right bytes (downloaded and hashed), nothing else is, no asset twice,
//   - an upload answered with an error status was not stored,
//   - while the disk was missing, the server refused writes (fail closed)
//     and wrote nothing to a directory recreated at the mount path,
//   - retrying every unacknowledged upload with its Idempotency-Key stores
//     it exactly once.
//
// It never touches real disks: images live in -work and are attached with
// -nobrowse at mount points inside -work.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/aaaicu/withalbum/tools/internal/harness"
)

const (
	adminLogin = "chaos"
	// A throwaway password for a throwaway library on a disk image.
	adminPassword = "chaos-test-password-not-secret"
	hdiutil       = "/usr/bin/hdiutil"
)

var (
	serverBin = flag.String("server", "", "withalbum-server binary built with -tags chaos (required)")
	workDir   = flag.String("work", "", "scratch directory for disk images and mount points (required, must exist)")
	rounds    = flag.Int("rounds", 100, "number of failure rounds")
	seed      = flag.Uint64("seed", 0, "random seed (0: from the clock)")
	imageMB   = flag.Int("image-mb", 384, "size of the sparse disk image in MiB")
	report    = flag.String("report", "", "write the Markdown results table to this file")
	verbose   = flag.Bool("v", false, "print the server output")
)

// upload is one original the test has sent at least once. Its bytes are
// derived from its index, so they can be sent again without keeping them.
type upload struct {
	idx    int
	key    string
	size   int
	sha    string
	acked  bool // a 2xx answer was received for it
	refuse bool // an error status was received and no 2xx since
}

func (u *upload) bytes() []byte {
	r := rand.New(rand.NewChaCha8(seedBytes(uint64(u.idx) ^ runSeed)))
	b := make([]byte, u.size)
	for i := 0; i+8 <= len(b); i += 8 {
		v := r.Uint64()
		for j := 0; j < 8; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	for i := len(b) &^ 7; i < len(b); i++ {
		b[i] = byte(r.Uint32())
	}
	return b
}

func seedBytes(v uint64) [32]byte {
	var s [32]byte
	for i := 0; i < 4; i++ {
		x := v*0x9E3779B97F4A7C15 + uint64(i)*0xBF58476D1CE4E5B9
		for j := 0; j < 8; j++ {
			s[i*8+j] = byte(x >> (8 * j))
		}
	}
	return s
}

var runSeed uint64

// disk is the attached sparse image.
type disk struct {
	image, mount string
}

func run(args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

func createImage(path, volname string, mb int) error {
	_, err := run(hdiutil, "create", "-quiet", "-size", fmt.Sprintf("%dm", mb), "-type", "SPARSE", "-fs", "APFS", "-volname", volname, path)
	return err
}

func (d disk) attach() error {
	_, err := run(hdiutil, "attach", "-quiet", "-nobrowse", "-noautoopen", "-owners", "on", "-mountpoint", d.mount, d.image)
	return err
}

func (d disk) detach(force bool) error {
	args := []string{hdiutil, "detach", "-quiet", d.mount}
	if force {
		args = append(args, "-force")
	}
	var err error
	for i := 0; i < 5; i++ {
		if _, err = run(args...); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return err
}

func (d disk) attached() bool {
	var st, parent syscall.Stat_t
	if syscall.Stat(d.mount, &st) != nil || syscall.Stat(filepath.Dir(d.mount), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

func (d disk) dev() int32 {
	var st syscall.Stat_t
	if syscall.Stat(d.mount, &st) != nil {
		return -1
	}
	return st.Dev
}

// results counts what happened.
type results struct {
	scenario     map[string]int
	failures     []string
	notes        map[string]int
	uploads      int
	acked        int
	retried      int
	verifies     int
	restarts     int
	devChanges   int
	detachRounds int
	start        time.Time
}

func (r *results) fail(round int, scen, format string, a ...any) {
	msg := fmt.Sprintf("round %d (%s): ", round, scen) + fmt.Sprintf(format, a...)
	log.Printf("FAILURE %s", msg)
	r.failures = append(r.failures, msg)
}

func (r *results) note(what string) { r.notes[what]++ }

// world is the state of one chaos run.
type world struct {
	srv      *harness.Server
	disk     disk
	dummy    disk
	root     string
	folder   string
	client   *harness.Client
	uploads  []*upload
	rng      *rand.Rand
	res      *results
	cfgPath  string
	volumeID string
}

func main() {
	log.SetFlags(log.Ltime)
	flag.Parse()
	if runtime.GOOS != "darwin" || *serverBin == "" || *workDir == "" || *rounds < 1 {
		fmt.Fprintln(os.Stderr, "usage: chaos -server BIN -work DIR [-rounds N] [-seed S] [-report FILE] (macOS only; see scripts/chaos-macos.sh)")
		os.Exit(2)
	}
	runSeed = *seed
	if runSeed == 0 {
		runSeed = uint64(time.Now().UnixNano())
	}
	work, err := filepath.EvalSymlinks(*workDir)
	if err != nil {
		log.Fatalf("work dir: %v", err)
	}
	w := &world{rng: rand.New(rand.NewPCG(runSeed, 7)), res: &results{scenario: map[string]int{}, notes: map[string]int{}, start: time.Now()}}
	w.disk = disk{image: filepath.Join(work, "library.sparseimage"), mount: filepath.Join(work, "mnt")}
	w.dummy = disk{image: filepath.Join(work, "dummy.sparseimage"), mount: filepath.Join(work, "mnt-dummy")}
	w.root = filepath.Join(w.disk.mount, "WithAlbum")
	w.cfgPath = filepath.Join(work, "config", "server.json")

	// Clean up on every exit path, including ^C.
	cleanup := func() {
		if w.srv != nil {
			w.srv.Kill()
		}
		for _, d := range []disk{w.disk, w.dummy, {mount: filepath.Join(work, "mnt-clone")}} {
			if d.attached() {
				_ = d.detach(true)
			}
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Print("interrupted; cleaning up")
		cleanup()
		os.Exit(130)
	}()
	code := w.main(work)
	cleanup()
	os.Exit(code)
}

func (w *world) main(work string) int {
	log.Printf("chaos: seed=%d rounds=%d image=%dMiB work=%s", runSeed, *rounds, *imageMB, work)
	if err := os.MkdirAll(filepath.Dir(w.cfgPath), 0700); err != nil {
		log.Print(err)
		return 1
	}
	for _, d := range []disk{w.disk, w.dummy} {
		if err := createImage(d.image, "WAChaos", map[bool]int{true: *imageMB, false: 32}[d == w.disk]); err != nil {
			log.Print(err)
			return 1
		}
	}
	if err := w.disk.attach(); err != nil {
		log.Print(err)
		return 1
	}
	if err := os.Mkdir(w.root, 0700); err != nil {
		log.Print(err)
		return 1
	}
	listen, err := harness.FreePort()
	if err != nil {
		log.Print(err)
		return 1
	}
	w.srv = &harness.Server{Bin: *serverBin, Root: w.root, Config: w.cfgPath, Listen: listen, Dir: work,
		Env: []string{"WITHALBUM_MIN_FREE_BYTES=0"}}
	if *verbose {
		w.srv.Log = os.Stderr
	}
	code, out, _, err := w.srv.Command("", "-init-storage")
	if err != nil || code != 0 {
		log.Printf("init-storage: %d %v %s", code, err, out)
		return 1
	}
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "volume_id="); ok {
			w.volumeID = v
		}
		if strings.HasPrefix(l, "disk_uuid") {
			log.Printf("init-storage: %s", l)
		}
	}
	if code, out, _, err := w.srv.Command(adminPassword+"\n", "admin", "init", "-login", adminLogin); err != nil || code != 0 {
		log.Printf("admin init: %d %v %s", code, err, out)
		return 1
	}
	// From now on the server finds its storage like an installed one: the
	// config file written by the setup screen (root + volume id), so a
	// missing disk fails closed instead of offering setup.
	cfg, _ := json.Marshal(map[string]any{"format": 1, "storage_root": w.root, "volume_id": w.volumeID})
	if err := os.WriteFile(w.cfgPath, cfg, 0600); err != nil {
		log.Print(err)
		return 1
	}
	w.srv.Root = ""
	if err := w.restart(nil); err != nil {
		log.Print(err)
		return 1
	}
	if w.folder, err = w.client.CreateFolder("chaos"); err != nil {
		log.Print(err)
		return 1
	}

	w.copiedMarkerCheck(work)
	w.missingDiskStartCheck()

	scenarios := []string{
		"kill_mid_body",
		"kill_at:after_stage", "kill_at:after_promote", "kill_at:before_commit", "kill_at:after_commit",
		"powercut_mid_body", "powercut_after_commit",
		"detach_same_dev", "detach_new_dev",
		"disk_full",
		"sigterm_mid_body",
	}
	order := []string{}
	for len(order) < *rounds {
		perm := w.rng.Perm(len(scenarios))
		for _, i := range perm {
			order = append(order, scenarios[i])
		}
	}
	order = order[:*rounds]
	for i, scen := range order {
		round := i + 1
		w.res.scenario[scen]++
		log.Printf("round %d/%d: %s", round, *rounds, scen)
		w.round(round, scen)
		if len(w.res.failures) > 20 {
			log.Print("too many failures; stopping early")
			break
		}
	}
	w.srv.Stop()
	if *report != "" {
		if err := os.WriteFile(*report, []byte(w.markdown()), 0644); err != nil {
			log.Print(err)
		}
	}
	fmt.Print(w.markdown())
	if len(w.res.failures) > 0 {
		return 1
	}
	return 0
}

// restart (re)starts the server, optionally at a fault point, and logs in.
func (w *world) restart(extra []string) error {
	if w.srv.Running() {
		w.srv.Stop()
	}
	if err := w.srv.StartWith(extra, 60*time.Second); err != nil {
		return err
	}
	w.res.restarts++
	c, err := w.srv.Login(adminLogin, adminPassword)
	if err != nil {
		return err
	}
	w.client = c
	return nil
}

func (w *world) newUpload(minSize, maxSize int) *upload {
	u := &upload{idx: len(w.uploads), key: fmt.Sprintf("chaos-%d-%d", runSeed%100000, len(w.uploads))}
	u.size = minSize + w.rng.IntN(maxSize-minSize+1)
	u.sha = sha256Hex(u.bytes())
	w.uploads = append(w.uploads, u)
	w.res.uploads++
	return u
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// slowReader trickles the body so that a failure lands mid-request.
type slowReader struct {
	b     []byte
	off   int
	chunk int
	pause time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.off >= len(s.b) {
		return 0, io.EOF
	}
	if s.off > 0 {
		time.Sleep(s.pause)
	}
	n := min(len(p), s.chunk, len(s.b)-s.off)
	copy(p, s.b[s.off:s.off+n])
	s.off += n
	return n, nil
}

// send uploads u once and records the answer.
func (w *world) send(u *upload, slow bool) harness.UploadResult {
	var body io.Reader = bytes.NewReader(u.bytes())
	if slow {
		body = &slowReader{b: u.bytes(), chunk: 32 << 10, pause: 15 * time.Millisecond}
	}
	r := w.client.Upload(w.folder, u.key, fmt.Sprintf("chaos-%d.bin", u.idx), body, int64(u.size))
	w.record(u, r)
	return r
}

func (w *world) record(u *upload, r harness.UploadResult) {
	switch {
	case r.Status == 0:
		w.res.note("answer: none (connection closed or reset)")
	case r.Status/100 != 2:
		w.res.note(fmt.Sprintf("answer: %d %s", r.Status, r.Code))
	}
	switch {
	case r.Status/100 == 2:
		if r.SHA256 != u.sha {
			w.res.fail(0, "upload", "upload %d acknowledged with sha %s, sent %s", u.idx, r.SHA256, u.sha)
		}
		if !u.acked {
			w.res.acked++
		}
		u.acked, u.refuse = true, false
	case r.Status != 0 && !u.acked:
		u.refuse = true
	}
}

// async starts send in the background.
func (w *world) async(u *upload, slow bool) chan harness.UploadResult {
	ch := make(chan harness.UploadResult, 1)
	go func() {
		var body io.Reader = bytes.NewReader(u.bytes())
		if slow {
			body = &slowReader{b: u.bytes(), chunk: 32 << 10, pause: 15 * time.Millisecond}
		}
		ch <- w.client.Upload(w.folder, u.key, fmt.Sprintf("chaos-%d.bin", u.idx), body, int64(u.size))
	}()
	return ch
}

func (w *world) round(round int, scen string) {
	// Some ordinary uploads first, so the library keeps growing.
	for i := 0; i < 1+w.rng.IntN(2); i++ {
		u := w.newUpload(4<<10, 256<<10)
		if r := w.send(u, false); r.Status/100 != 2 {
			w.res.fail(round, scen, "ordinary upload before the failure: %d %s %v", r.Status, r.Code, r.Err)
		}
	}
	switch {
	case scen == "kill_mid_body", scen == "powercut_mid_body":
		u := w.newUpload(1<<20, 2<<20)
		ch := w.async(u, true)
		time.Sleep(time.Duration(50+w.rng.IntN(500)) * time.Millisecond)
		w.srv.Kill()
		if scen == "powercut_mid_body" {
			w.powerCut(round, scen)
		}
		w.record(u, <-ch)
	case strings.HasPrefix(scen, "kill_at:"), scen == "powercut_after_commit":
		point := strings.TrimPrefix(scen, "kill_at:")
		if scen == "powercut_after_commit" {
			point = "after_commit"
		}
		if err := w.restart([]string{"WITHALBUM_CHAOS_FAULT=" + point}); err != nil {
			w.res.fail(round, scen, "start at fault point: %v", err)
			return
		}
		u := w.newUpload(8<<10, 512<<10)
		ch := w.async(u, false)
		if !w.srv.WaitLine("CHAOS: at "+point, 20*time.Second) {
			w.res.fail(round, scen, "the upload never reached %s: %s", point, w.srv.Tail(5))
		}
		w.srv.Kill()
		if scen == "powercut_after_commit" {
			w.powerCut(round, scen)
		}
		w.record(u, <-ch)
	case scen == "detach_same_dev", scen == "detach_new_dev":
		w.detachRound(round, scen)
	case scen == "disk_full":
		w.diskFullRound(round, scen)
	case scen == "sigterm_mid_body":
		u := w.newUpload(512<<10, 1<<20)
		ch := w.async(u, true)
		time.Sleep(time.Duration(50+w.rng.IntN(300)) * time.Millisecond)
		w.srv.Stop()
		w.record(u, <-ch)
	}
	w.check(round, scen)
}

// powerCut detaches the disk without a sync right after a SIGKILL and
// attaches it again.
func (w *world) powerCut(round int, scen string) {
	if err := w.disk.detach(true); err != nil {
		w.res.fail(round, scen, "forced detach: %v", err)
	}
	if err := w.disk.attach(); err != nil {
		w.res.fail(round, scen, "reattach: %v", err)
	}
}

func (w *world) detachRound(round int, scen string) {
	w.res.detachRounds++
	devBefore := w.disk.dev()
	u := w.newUpload(1<<20, 2<<20)
	ch := w.async(u, true)
	time.Sleep(time.Duration(100+w.rng.IntN(400)) * time.Millisecond)
	if err := w.disk.detach(true); err != nil {
		w.res.fail(round, scen, "forced detach: %v", err)
		return
	}
	r := <-ch
	w.record(u, r)
	if r.Status/100 == 2 {
		w.res.note("upload in flight at detach still acknowledged (finished before the detach)")
	}
	alive := w.srv.Running()
	if !alive {
		w.res.note("server exited when the disk was detached")
	}
	// Fail closed while the disk is missing.
	if alive {
		if st, err := w.srv.Health(); err != nil || st != "unavailable" {
			w.res.fail(round, scen, "health while detached: %q %v, want unavailable", st, err)
		}
		v := w.newUpload(1<<10, 8<<10)
		if r := w.send(v, false); r.Status/100 == 2 {
			w.res.fail(round, scen, "upload accepted while the disk was missing: %d %s", r.Status, r.Code)
		} else if r.Status != 503 {
			w.res.note(fmt.Sprintf("upload while detached answered %d %s", r.Status, r.Code))
		}
		// The classic accident: the mount path exists again on the
		// internal disk. Nothing may be written there.
		if err := os.MkdirAll(w.root, 0700); err != nil {
			w.res.fail(round, scen, "shadow dir: %v", err)
		} else {
			v := w.newUpload(1<<10, 8<<10)
			if r := w.send(v, false); r.Status/100 == 2 {
				w.res.fail(round, scen, "upload accepted into a directory recreated at the mount path: %d", r.Status)
			}
			if st, _ := w.srv.Health(); st != "unavailable" {
				w.res.fail(round, scen, "health with a shadow directory: %q, want unavailable", st)
			}
			entries, _ := os.ReadDir(w.root)
			if len(entries) != 0 {
				w.res.fail(round, scen, "the server wrote %d entries into the shadow directory", len(entries))
			}
		}
		_ = os.RemoveAll(w.disk.mount)
	}
	// Reattach, with a different device number when asked: the dummy image
	// takes the next free disk number first.
	if scen == "detach_new_dev" {
		if err := w.dummy.attach(); err != nil {
			w.res.fail(round, scen, "attach dummy: %v", err)
		}
	}
	if err := w.disk.attach(); err != nil {
		w.res.fail(round, scen, "reattach: %v", err)
		return
	}
	if scen == "detach_new_dev" {
		_ = w.dummy.detach(false)
	}
	devAfter := w.disk.dev()
	if devAfter != devBefore {
		w.res.devChanges++
	}
	if scen == "detach_new_dev" && devAfter == devBefore {
		w.res.note("detach_new_dev: device number did not change")
	}
	if alive && w.srv.Running() {
		// After a reattach the running server must still refuse writes:
		// its open database and lock belong to the detached disk.
		v := w.newUpload(1<<10, 8<<10)
		r := w.send(v, false)
		if r.Status/100 == 2 {
			w.res.fail(round, scen, "running server accepted an upload after the disk was reattached (dev %d -> %d) without a restart", devBefore, devAfter)
		}
		if st, _ := w.srv.Health(); st != "unavailable" {
			w.res.fail(round, scen, "health after reattach without restart: %q, want unavailable", st)
		}
	}
}

func (w *world) diskFullRound(round int, scen string) {
	filler := filepath.Join(w.disk.mount, "filler.bin")
	f, err := os.OpenFile(filler, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		w.res.fail(round, scen, "filler: %v", err)
		return
	}
	chunk := make([]byte, 1<<20)
	for {
		if _, err := f.Write(chunk); err != nil {
			break
		}
	}
	_ = f.Sync()
	f.Close()
	// Too big for what is left, then tiny ones that may or may not fit.
	big := w.newUpload(4<<20, 6<<20)
	if r := w.send(big, false); r.Status/100 == 2 {
		w.res.note("disk_full: a 4-6 MiB upload still fit")
	} else if r.Status != 507 && r.Status != 503 {
		w.res.note(fmt.Sprintf("disk_full: big upload answered %d %s", r.Status, r.Code))
	}
	for i := 0; i < 3; i++ {
		w.send(w.newUpload(1<<10, 64<<10), false)
	}
	// Unknown length: the body is cut off by the free-space budget.
	u := w.newUpload(1<<20, 2<<20)
	r := w.client.Upload(w.folder, u.key, "chunked.bin", io.MultiReader(bytes.NewReader(u.bytes())), -1)
	w.record(u, r)
	if !w.srv.Running() {
		w.res.fail(round, scen, "server exited on a full disk: %s", w.srv.Tail(5))
	}
	_ = os.Remove(filler)
	if r := w.send(w.newUpload(4<<10, 64<<10), false); r.Status/100 != 2 && w.srv.Running() {
		w.res.fail(round, scen, "upload after freeing space: %d %s", r.Status, r.Code)
	}
}

// check restarts the server and compares what it holds with what it said.
func (w *world) check(round int, scen string) {
	if !w.disk.attached() {
		if err := w.disk.attach(); err != nil {
			w.res.fail(round, scen, "attach before check: %v", err)
			return
		}
	}
	if err := w.restart(nil); err != nil {
		w.res.fail(round, scen, "restart: %v", err)
		return
	}
	w.verify(round, scen)
	assets, _, _, err := w.client.ListAssets(w.folder, 500)
	if err != nil {
		w.res.fail(round, scen, "list: %v", err)
		return
	}
	bySHA := map[string]harness.Asset{}
	for _, a := range assets {
		if _, dup := bySHA[a.SHA256]; dup {
			w.res.fail(round, scen, "original %s is in the folder twice", a.SHA256)
		}
		bySHA[a.SHA256] = a
	}
	known := map[string]*upload{}
	for _, u := range w.uploads {
		known[u.sha] = u
		a, present := bySHA[u.sha]
		if !u.acked && !u.refuse {
			if present {
				w.res.note("unanswered upload found stored after restart (committed before the failure)")
			} else {
				w.res.note("unanswered upload not stored after restart")
			}
		}
		switch {
		case u.acked && !present:
			w.res.fail(round, scen, "upload %d was acknowledged but is missing after restart", u.idx)
		case u.refuse && present:
			w.res.fail(round, scen, "upload %d was answered with an error but is stored", u.idx)
		case present && a.Bytes != int64(u.size):
			w.res.fail(round, scen, "upload %d has %d bytes, sent %d", u.idx, a.Bytes, u.size)
		}
	}
	for sha := range bySHA {
		if known[sha] == nil {
			w.res.fail(round, scen, "unknown original %s in the folder", sha)
		}
	}
	// Download what this round touched (and a sample of the rest).
	for _, u := range w.uploads {
		a, ok := bySHA[u.sha]
		if !ok || (u.idx < len(w.uploads)-12 && w.rng.IntN(20) != 0) {
			continue
		}
		got, err := w.client.DownloadSHA256(w.folder, a.ID)
		if err != nil || got != u.sha {
			w.res.fail(round, scen, "download of upload %d: sha %s, %v", u.idx, got, err)
		}
	}
	// The client's retry: every upload without an acknowledgement is sent
	// again with its key and must be stored exactly once.
	for _, u := range w.uploads {
		if u.acked {
			continue
		}
		w.res.retried++
		if r := w.send(u, false); r.Status/100 != 2 {
			w.res.fail(round, scen, "retry of upload %d: %d %s %v", u.idx, r.Status, r.Code, r.Err)
		}
	}
	after, _, _, err := w.client.ListAssets(w.folder, 500)
	if err != nil {
		w.res.fail(round, scen, "list after retries: %v", err)
		return
	}
	if len(after) != len(w.uploads) {
		w.res.fail(round, scen, "folder holds %d assets after retries, %d distinct uploads were sent", len(after), len(w.uploads))
	}
}

func (w *world) verify(round int, scen string) {
	code, out, _, err := w.srv.Command("", "verify", "-hash", "-json")
	w.res.verifies++
	if err != nil || code != 0 {
		w.res.fail(round, scen, "verify -hash: exit %d %v: %s", code, err, truncate(out, 600))
		return
	}
	var rep struct {
		Blobs        int `json:"blobs"`
		Hashed       int `json:"hashed"`
		Unreferenced int `json:"unreferenced_pool_files"`
		Previews     int `json:"previews_missing"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		w.res.fail(round, scen, "verify -json output: %v", err)
		return
	}
	if rep.Hashed != rep.Blobs {
		w.res.fail(round, scen, "verify hashed %d of %d originals", rep.Hashed, rep.Blobs)
	}
	// After the restart's reconciliation nothing may be left over.
	if rep.Unreferenced > 0 {
		w.res.fail(round, scen, "verify after restart: %d pool files without a database row", rep.Unreferenced)
	}
	if rep.Previews > 0 {
		w.res.note("verify: previews missing (derived data)")
	}
}

// copiedMarkerCheck copies the library (marker and .system) to a second
// disk image and checks that the server refuses to start on the copy.
func (w *world) copiedMarkerCheck(work string) {
	w.res.scenario["once:copied_marker"]++
	clone := disk{image: filepath.Join(work, "clone.sparseimage"), mount: filepath.Join(work, "mnt-clone")}
	if err := createImage(clone.image, "WAClone", 64); err != nil {
		w.res.fail(0, "copied_marker", "%v", err)
		return
	}
	if err := clone.attach(); err != nil {
		w.res.fail(0, "copied_marker", "%v", err)
		return
	}
	defer clone.detach(true)
	w.srv.Stop()
	croot := filepath.Join(clone.mount, "WithAlbum")
	if _, err := run("/bin/cp", "-Rp", w.root, croot); err != nil {
		w.res.fail(0, "copied_marker", "copy: %v", err)
	}
	c := &harness.Server{Bin: w.srv.Bin, Root: croot, Config: filepath.Join(work, "config", "clone.json"), Listen: w.srv.Listen, Dir: work}
	err := c.Start(20 * time.Second)
	if err == nil {
		c.Kill()
		w.res.fail(0, "copied_marker", "the server started on a copy of the library on another disk")
	} else if !strings.Contains(err.Error(), "another disk") {
		w.res.fail(0, "copied_marker", "refused, but not by the disk check: %v", err)
	}
	if code, out, _, _ := c.Command("", "verify"); code == 0 || !strings.Contains(out, "another disk") {
		w.res.fail(0, "copied_marker", "verify on the copy: exit %d %s", code, truncate(out, 300))
	}
	if err := w.restart(nil); err != nil {
		w.res.fail(0, "copied_marker", "restart on the original: %v", err)
	}
}

// missingDiskStartCheck: with the disk detached the server refuses to
// start, also when the mount path exists as an empty directory.
func (w *world) missingDiskStartCheck() {
	w.res.scenario["once:start_without_disk"]++
	w.srv.Stop()
	if err := w.disk.detach(false); err != nil {
		w.res.fail(0, "start_without_disk", "detach: %v", err)
		return
	}
	if err := w.srv.Start(20 * time.Second); err == nil {
		w.srv.Kill()
		w.res.fail(0, "start_without_disk", "the server started without its disk")
	}
	_ = os.MkdirAll(w.root, 0700)
	if err := w.srv.Start(20 * time.Second); err == nil {
		w.srv.Kill()
		w.res.fail(0, "start_without_disk", "the server started on an empty directory at the mount path")
	}
	if entries, _ := os.ReadDir(w.root); len(entries) != 0 {
		w.res.fail(0, "start_without_disk", "refused start wrote %d entries into the empty directory", len(entries))
	}
	_ = os.RemoveAll(w.disk.mount)
	if err := w.disk.attach(); err != nil {
		w.res.fail(0, "start_without_disk", "reattach: %v", err)
		return
	}
	if err := w.restart(nil); err != nil {
		w.res.fail(0, "start_without_disk", "restart: %v", err)
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " | ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (w *world) markdown() string {
	var b strings.Builder
	r := w.res
	fmt.Fprintf(&b, "| item | value |\n|---|---|\n")
	sw, _ := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	fmt.Fprintf(&b, "| macOS | %s (%s) |\n", strings.TrimSpace(string(sw)), runtime.GOARCH)
	fmt.Fprintf(&b, "| disk | hdiutil SPARSE APFS image, %d MiB, attached -nobrowse |\n", *imageMB)
	fmt.Fprintf(&b, "| seed | %d |\n", runSeed)
	fmt.Fprintf(&b, "| rounds | %d |\n", sum(r.scenario)-r.scenario["once:copied_marker"]-r.scenario["once:start_without_disk"])
	fmt.Fprintf(&b, "| duration | %s |\n", time.Since(r.start).Round(time.Second))
	fmt.Fprintf(&b, "| distinct uploads sent | %d |\n", r.uploads)
	fmt.Fprintf(&b, "| acknowledged (2xx) | %d |\n", r.acked)
	fmt.Fprintf(&b, "| retries of unacknowledged uploads | %d |\n", r.retried)
	fmt.Fprintf(&b, "| server restarts | %d |\n", r.restarts)
	fmt.Fprintf(&b, "| verify -hash runs | %d |\n", r.verifies)
	fmt.Fprintf(&b, "| detach rounds / st_dev changed | %d / %d |\n", r.detachRounds, r.devChanges)
	fmt.Fprintf(&b, "| **failures** | **%d** |\n\n", len(r.failures))
	fmt.Fprintf(&b, "| scenario | rounds |\n|---|---|\n")
	keys := make([]string, 0, len(r.scenario))
	for k := range r.scenario {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "| `%s` | %d |\n", k, r.scenario[k])
	}
	if len(r.notes) > 0 {
		fmt.Fprintf(&b, "\n| observation (not a failure) | count |\n|---|---|\n")
		keys = keys[:0]
		for k := range r.notes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "| %s | %d |\n", k, r.notes[k])
		}
	}
	if len(r.failures) > 0 {
		fmt.Fprintf(&b, "\nFailures:\n\n")
		for _, f := range r.failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	return b.String()
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
