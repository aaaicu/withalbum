package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	pbt "github.com/aaaicu/withalbum/internal/blobstore/playbacktest"
)

func TestCheckPlaybackMP4(t *testing.T) {
	dir := t.TempDir()
	check := func(name string, body []byte) (int, int, error) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0600); err != nil {
			t.Fatal(err)
		}
		return CheckPlaybackMP4(p)
	}
	if w, h, err := check("ok.mp4", pbt.MP4(720, 1280, 100, 1, true)); err != nil || w != 720 || h != 1280 {
		t.Fatalf("fast start: %d %d %v", w, h, err)
	}
	ftyp := pbt.Box("ftyp", []byte("isom"), make([]byte, 4))
	mdat := pbt.Box("mdat", make([]byte, 10))
	// A 64-bit mdat size is fine.
	big := append(binary.BigEndian.AppendUint32(nil, 1), "mdat"...)
	big = binary.BigEndian.AppendUint64(big, 16+10)
	big = append(big, make([]byte, 10)...)
	if _, _, err := check("large.mp4", bytes.Join([][]byte{ftyp, pbt.Moov(640, 360, "avc1"), big}, nil)); err != nil {
		t.Fatalf("64-bit box: %v", err)
	}
	for name, body := range map[string][]byte{
		"moov-last":  pbt.MP4(720, 1280, 100, 1, false),
		"no-ftyp":    bytes.Join([][]byte{pbt.Moov(640, 360, "avc1"), mdat}, nil),
		"hevc":       bytes.Join([][]byte{ftyp, pbt.Moov(640, 360, "hvc1"), mdat}, nil),
		"too-large":  bytes.Join([][]byte{ftyp, pbt.Moov(3840, 2160, "avc1"), mdat}, nil),
		"no-size":    bytes.Join([][]byte{ftyp, pbt.Moov(0, 0, "avc1"), mdat}, nil),
		"no-mdat":    bytes.Join([][]byte{ftyp, pbt.Moov(640, 360, "avc1")}, nil),
		"truncated":  pbt.MP4(640, 360, 100, 1, true)[:150],
		"text":       []byte("not a movie at all, just text"),
		"empty-tail": append(pbt.MP4(640, 360, 10, 1, true), 0, 0, 0),
	} {
		if _, _, err := check(name, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func sumFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestMakePlaybackContract: the fake tool's output is checked, stored by
// the original's hash (read-only, fsynced, renamed), and refused when it
// is not smaller, not fast start, too slow, too large or when the tool
// failed; the original never changes and no scratch directory is left.
func TestMakePlaybackContract(t *testing.T) {
	s, _ := newTestStore(t)
	orig := pbt.Movie(3840, 2160, 50_000, 7)
	sha := pooled(t, s, orig)
	ctx := t.Context()
	if _, err := s.MakePlayback(ctx, sha, time.Minute); !errors.Is(err, ErrNoTranscoder) {
		t.Fatalf("no tool: %v", err)
	}
	fake := &pbt.Fake{}
	s.SetTranscoder(fake)
	f, err := s.MakePlayback(ctx, sha, time.Minute)
	if err != nil || f.Width != 720 || f.Height != 1280 || f.Bytes != int64(len(pbt.MP4(720, 1280, 1000, 1, true))) {
		t.Fatalf("made %+v, %v", f, err)
	}
	fi, err := os.Lstat(s.PlaybackPath(sha))
	if err != nil || fi.Size() != f.Bytes {
		t.Fatalf("stored copy %v, %v", fi, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0400 {
		t.Fatalf("copy mode %v", fi.Mode())
	}
	if got := sumFile(t, s.PoolPath(sha)); got != sha {
		t.Fatal("original changed")
	}
	if ok, err := s.PlaybackExists(sha); !ok || err != nil {
		t.Fatalf("exists %v %v", ok, err)
	}

	cases := map[string]struct {
		run  func(ctx context.Context, src, out string) error
		want error
	}{
		"not smaller": {func(_ context.Context, _, out string) error {
			return os.WriteFile(out, pbt.MP4(720, 1280, 45_000, 1, true), 0600)
		}, ErrPlaybackNotSmaller},
		"moov last": {func(_ context.Context, _, out string) error {
			return os.WriteFile(out, pbt.MP4(720, 1280, 100, 1, false), 0600)
		}, ErrPlaybackOutput},
		"no file":   {func(context.Context, string, string) error { return nil }, ErrPlaybackOutput},
		"tool fail": {func(context.Context, string, string) error { return errors.New("exit status 1") }, ErrTranscode},
		"timeout": {func(ctx context.Context, _, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		}, ErrTranscodeTimeout},
		"output grows": {func(ctx context.Context, _, out string) error {
			_ = os.WriteFile(out, make([]byte, 60_000+playbackOutputSlack), 0600)
			<-ctx.Done()
			return ctx.Err()
		}, ErrPlaybackOutput},
		"tamper": {func(_ context.Context, src, out string) error {
			_ = os.Chtimes(src, time.Now(), time.Now().Add(time.Hour))
			return os.WriteFile(out, pbt.MP4(720, 1280, 100, 1, true), 0600)
		}, ErrOriginalChanged},
	}
	defer func(d time.Duration) { playbackWatchInterval = d }(playbackWatchInterval)
	playbackWatchInterval = 10 * time.Millisecond
	other := pooled(t, s, pbt.Movie(3840, 2160, 50_000, 8))
	for name, c := range cases {
		fake.Set(c.run)
		limit := time.Minute
		if name == "timeout" {
			limit = 50 * time.Millisecond
		}
		start := time.Now()
		_, err := s.MakePlayback(ctx, other, limit)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if time.Since(start) > 10*time.Second {
			t.Errorf("%s took %s", name, time.Since(start))
		}
		if ok, _ := s.PlaybackExists(other); ok {
			t.Errorf("%s: a copy was stored", name)
		}
	}
	// Shutdown: the context's own error, not a failure of the tool.
	fake.Set(func(ctx context.Context, _, _ string) error { <-ctx.Done(); return ctx.Err() })
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := s.MakePlayback(cctx, other, time.Minute); !errors.Is(err, context.Canceled) || errors.Is(err, ErrTranscode) {
		t.Fatalf("cancelled: %v", err)
	}
	if n := workEntries(t, s); n != 0 {
		t.Fatalf("%d scratch directories left", n)
	}
	if got := sumFile(t, s.PoolPath(sha)); got != sha {
		t.Fatal("original changed")
	}
}

// TestPlaybackToolIsKilled: a real child process that outlives its limit
// is killed (with its process group) and reported as a timeout.
func TestPlaybackToolIsKilled(t *testing.T) {
	s, _ := newTestStore(t)
	sha := pooled(t, s, pbt.Movie(640, 360, 5000, 3))
	s.SetTranscoder(&toolTranscoder{name: "sleeper", path: os.Args[0], args: func(_ context.Context, src, out, _ string) ([]string, error) {
		return []string{"-test.run=^TestFakeConverter$", "--", "sleep", src, out}, nil
	}})
	start := time.Now()
	if _, err := s.MakePlayback(t.Context(), sha, 300*time.Millisecond); !errors.Is(err, ErrTranscodeTimeout) {
		t.Fatalf("sleeper: %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("killed after %s", d)
	}
}

func TestSweepAndRemovePlayback(t *testing.T) {
	s, _ := newTestStore(t)
	s.SetTranscoder(&pbt.Fake{})
	keep := pooled(t, s, pbt.Movie(640, 360, 5000, 1))
	drop := pooled(t, s, pbt.Movie(640, 360, 5000, 2))
	for _, sha := range []string{keep, drop} {
		if _, err := s.MakePlayback(t.Context(), sha, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	stray := filepath.Join(filepath.Dir(s.PlaybackPath(keep)), "notes.txt")
	if err := os.WriteFile(stray, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	n, err := s.SweepPlayback(t.Context(), func(sha string) (bool, error) { return sha == keep, nil })
	if err != nil || n != 1 {
		t.Fatalf("sweep %d, %v", n, err)
	}
	if ok, _ := s.PlaybackExists(drop); ok {
		t.Fatal("unready copy kept")
	}
	if ok, _ := s.PlaybackExists(keep); !ok {
		t.Fatal("ready copy deleted")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("a file that is not a copy was deleted")
	}
	if err := s.RemovePlayback(keep); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePlayback(keep); err != nil {
		t.Fatalf("second removal: %v", err)
	}
	for _, sha := range []string{keep, drop} {
		if got := sumFile(t, s.PoolPath(sha)); got != sha {
			t.Fatal("original changed")
		}
	}
}

func TestFindTranscoder(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if tr := FindTranscoder(t.Context(), env(map[string]string{"WITHALBUM_PLAYBACK": "off"})); tr != nil {
		t.Fatalf("off: %v", tr.Name())
	}
	off := map[string]string{"WITHALBUM_AVCONVERT": "off", "WITHALBUM_FFMPEG": "off"}
	if tr := FindTranscoder(t.Context(), env(off)); tr != nil {
		t.Fatalf("both off: %v", tr.Name())
	}
	// Not an executable: refused.
	if tr := FindTranscoder(t.Context(), env(map[string]string{"WITHALBUM_AVCONVERT": "off", "WITHALBUM_FFMPEG": "relative/ffmpeg"})); tr != nil {
		t.Fatalf("relative path: %v", tr.Name())
	}
	tr := FindTranscoder(t.Context(), env(nil))
	switch {
	case runtime.GOOS == "darwin" && findTool("avconvert", "") != "":
		if tr == nil || tr.Name() != "avconvert" {
			t.Fatalf("macOS with avconvert: %v", tr)
		}
	case tr != nil:
		t.Logf("transcoder here: %s", tr.Name())
	}
}

// realSource makes a high bit rate test clip (2560x1440, shown turned by
// 90 degrees, with audio) with ffmpeg, or skips.
func realSource(t *testing.T, dir string) string {
	t.Helper()
	ffmpeg := realTool(t, "ffmpeg")
	flat := filepath.Join(dir, "flat.mov")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=2560x1440:rate=30:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "6", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", "-y", flat).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot generate a clip here: %v %s", err, out)
	}
	turned := filepath.Join(dir, "turned.mov")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-display_rotation:v:0", "90", "-i", flat, "-c", "copy", "-y", turned).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot set a rotation here: %v %s", err, out)
	}
	return turned
}

// TestRealTranscoders makes playback copies with the tools installed here
// (avconvert on macOS; ffmpeg with its H.264 encoder): H.264 + AAC, fast
// start, the portrait orientation kept within 720x1280, smaller than the
// original, the original unchanged.
func TestRealTranscoders(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	dir := t.TempDir()
	src := realSource(t, dir)
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]Transcoder{}
	if runtime.GOOS == "darwin" {
		if p := findTool("avconvert", ""); p != "" {
			tools["avconvert"] = NewAvconvert(p)
		}
	}
	if tr := FindTranscoder(t.Context(), func(k string) string {
		if k == "WITHALBUM_PLAYBACK" {
			return "ffmpeg"
		}
		return ""
	}); tr != nil {
		tools["ffmpeg"] = tr
	}
	if len(tools) == 0 {
		t.Skip("no playback tool here")
	}
	for name, tr := range tools {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestStore(t)
			s.SetTranscoder(tr)
			sha := pooled(t, s, body)
			start := time.Now()
			f, err := s.MakePlayback(t.Context(), sha, 5*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d -> %d bytes (%.0f%%) in %s, %dx%d", name, len(body), f.Bytes, 100*float64(f.Bytes)/float64(len(body)), time.Since(start).Round(time.Millisecond), f.Width, f.Height)
			if f.Width != 720 || f.Height != 1280 {
				t.Fatalf("size %dx%d, want 720x1280 (portrait kept)", f.Width, f.Height)
			}
			copyBody, err := os.ReadFile(s.PlaybackPath(sha))
			if err != nil {
				t.Fatal(err)
			}
			moov, mdat := bytes.Index(copyBody, []byte("moov")), bytes.Index(copyBody, []byte("mdat"))
			if moov < 0 || mdat < 0 || moov > mdat {
				t.Fatalf("moov at %d, mdat at %d: not fast start", moov, mdat)
			}
			if !bytes.Contains(copyBody[:mdat], []byte("mp4a")) || !bytes.Contains(copyBody[:mdat], []byte("avc1")) {
				t.Fatal("no H.264 or no AAC track")
			}
			if got := sumFile(t, s.PoolPath(sha)); got != sha {
				t.Fatal("original changed")
			}
			if n := workEntries(t, s); n != 0 {
				t.Fatalf("%d scratch directories left", n)
			}
		})
	}
}
