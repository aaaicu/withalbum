package blobstore

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestFakeConverter is not a test: run as a child process with "--" and a
// mode, it plays a preview tool (see fakeConverter).
func TestFakeConverter(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || len(os.Args) < i+4 {
		return
	}
	mode, src, out := os.Args[i+1], os.Args[i+2], os.Args[i+3]
	switch mode {
	case "jpeg":
		img := image.NewRGBA(image.Rect(0, 0, 2400, 1200))
		for x := 0; x < 2400; x++ {
			img.Set(x, x/2, color.RGBA{255, 0, 0, 255})
		}
		f, _ := os.Create(out)
		_ = jpeg.Encode(f, img, nil)
		f.Close()
	case "garbage":
		_ = os.WriteFile(out, []byte("this is not a jpeg"), 0600)
	case "nothing":
	case "fail":
		os.Stderr.WriteString("cannot decode input")
		os.Exit(3)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "tamper":
		_ = os.Chmod(src, 0600)
		f, err := os.OpenFile(src, os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			f.WriteString("x")
			f.Close()
		}
		_ = os.WriteFile(out, []byte("x"), 0600)
	case "env":
		// Writes its environment, to check it is minimal.
		_ = os.WriteFile(out, []byte(strings.Join(os.Environ(), "\n")), 0600)
		os.Exit(4)
	}
	os.Exit(0)
}

// fakeConverter runs this test binary as a converter in the given mode.
func fakeConverter(mode string) Converter {
	return Converter{Name: "fake-" + mode, Path: os.Args[0], Types: []string{"image/heic", "video/"},
		attempts: func(_ context.Context, src, out, _ string) ([][]string, error) {
			return [][]string{{"-test.run=^TestFakeConverter$", "--", mode, src, out}}, nil
		}}
}

// pooled stages and promotes body and returns its hash.
func pooled(t *testing.T, s *Store, body []byte) string {
	t.Helper()
	f, err := s.Stage(bytes.NewReader(body), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(f, f.SHA256); err != nil {
		t.Fatal(err)
	}
	return f.SHA256
}

func workEntries(t *testing.T, s *Store) int {
	t.Helper()
	e, err := os.ReadDir(filepath.Join(s.root, filepath.FromSlash(WorkRelDir)))
	if err != nil {
		t.Fatal(err)
	}
	return len(e)
}

func TestConverterContract(t *testing.T) {
	s, _ := newTestStore(t)
	heicLike := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c', 0, 0, 0, 0, 'm', 'i', 'f', '1', 'h', 'e', 'i', 'c'}, make([]byte, 100)...)
	if got := SniffMIME(heicLike); got != "image/heic" {
		t.Fatalf("sniff %q", got)
	}
	if s.PreviewMode("image/heic") != PreviewUnsupported || s.PreviewMode("image/jpeg") != PreviewBuiltin {
		t.Fatal("preview modes without converters")
	}
	if err := s.MakeConvertedPreview(t.Context(), hexSHA(heicLike), "image/heic"); !errors.Is(err, ErrNoConverter) {
		t.Fatalf("no converter: %v", err)
	}
	old := convertTimeout
	convertTimeout = 2 * time.Second
	t.Cleanup(func() { convertTimeout = old })

	for _, tc := range []struct {
		mode    string
		wantErr error // nil: a preview is written
		detail  string
	}{
		{"jpeg", nil, ""},
		{"garbage", ErrConvert, "output"},
		{"nothing", ErrConvert, "no output"},
		{"fail", ErrConvert, "cannot decode input"},
		{"sleep", ErrConvert, "timed out"},
		{"tamper", ErrConvert, "INTEGRITY"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			body := append(append([]byte{}, heicLike...), tc.mode...)
			sha := pooled(t, s, body)
			s.SetConverters([]Converter{fakeConverter(tc.mode)})
			if s.PreviewMode("image/heic") != PreviewExternal || s.PreviewMode("video/quicktime") != PreviewExternal {
				t.Fatal("preview mode with a converter")
			}
			start := time.Now()
			err := s.MakeConvertedPreview(t.Context(), sha, "image/heic")
			if tc.mode == "sleep" && time.Since(start) > 15*time.Second {
				t.Fatalf("timeout took %s", time.Since(start))
			}
			if workEntries(t, s) != 0 {
				t.Fatal("scratch directory left behind")
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
				f, err := os.Open(s.PreviewPath(sha))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				cfg, err := jpeg.DecodeConfig(f)
				if err != nil || cfg.Width != 1600 || cfg.Height != 800 {
					t.Fatalf("preview %+v %v", cfg, err)
				}
			} else {
				if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.detail) {
					t.Fatalf("err = %v, want %v with %q", err, tc.wantErr, tc.detail)
				}
				if _, err := os.Stat(s.PreviewPath(sha)); !os.IsNotExist(err) {
					t.Fatalf("preview written after a failure: %v", err)
				}
			}
			if tc.mode != "tamper" {
				if ok, err := fileMatches(s.PoolPath(sha), sha, int64(len(body))); !ok || err != nil {
					t.Fatalf("original changed: %v %v", ok, err)
				}
			}
		})
	}
	t.Run("environment", func(t *testing.T) {
		sha := pooled(t, s, append(append([]byte{}, heicLike...), "env"...))
		s.SetConverters([]Converter{fakeConverter("env")})
		if err := s.MakeConvertedPreview(t.Context(), sha, "image/heic"); !errors.Is(err, ErrConvert) {
			t.Fatalf("env mode: %v", err)
		}
	})
}

func TestRunToolMinimalEnvironment(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	t.Setenv("WITHALBUM_SECRET_FOR_TEST", "must-not-leak")
	_, err := runTool(context.Background(), os.Args[0], []string{"-test.run=^TestFakeConverter$", "--", "env", "src", out}, dir)
	if err == nil {
		t.Fatal("env mode exits 4")
	}
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	env := string(b)
	if strings.Contains(env, "must-not-leak") || !strings.Contains(env, "HOME="+dir) || !strings.Contains(env, "PATH=/usr/bin:/bin") {
		t.Fatalf("tool environment:\n%s", env)
	}
}

func TestFindConverters(t *testing.T) {
	env := map[string]string{"WITHALBUM_PREVIEW_TOOLS": "off"}
	get := func(k string) string { return env[k] }
	if cs := FindConverters(get); len(cs) != 0 {
		t.Fatalf("off: %v", cs)
	}
	env = map[string]string{"WITHALBUM_SIPS": "off", "WITHALBUM_FFMPEG": "relative/ffmpeg"}
	if cs := FindConverters(get); len(cs) != 0 {
		t.Fatalf("disabled and relative: %v", cs)
	}
	env = map[string]string{"WITHALBUM_SIPS": "off", "WITHALBUM_FFMPEG": os.Args[0]}
	cs := FindConverters(get)
	if len(cs) != 1 || cs[0].Name != "ffmpeg" || cs[0].Path != os.Args[0] {
		t.Fatalf("explicit ffmpeg: %+v", cs)
	}
}

// realTool returns the tool's path or skips the test.
func realTool(t *testing.T, name string) string {
	t.Helper()
	p := findTool(name, "")
	if p == "" {
		t.Skipf("%s not installed", name)
	}
	return p
}

// TestRealConverters makes previews of a HEIC (made with sips from a
// generated JPEG) and a QuickTime movie (generated by ffmpeg), when the
// tools are installed. The originals stay byte for byte as uploaded.
func TestRealConverters(t *testing.T) {
	work := t.TempDir()
	t.Run("sips", func(t *testing.T) {
		sips := realTool(t, "sips")
		src := filepath.Join(work, "in.jpg")
		img := image.NewRGBA(image.Rect(0, 0, 640, 480))
		for x := 0; x < 640; x++ {
			img.Set(x, x*3/4, color.RGBA{0, 128, 255, 255})
		}
		f, _ := os.Create(src)
		_ = jpeg.Encode(f, img, nil)
		f.Close()
		heic := filepath.Join(work, "in.heic")
		if out, err := exec.Command(sips, "-s", "format", "heic", src, "--out", heic).CombinedOutput(); err != nil {
			t.Skipf("sips cannot write HEIC here: %v %s", err, out)
		}
		checkRealPreview(t, NewSips(sips), heic, 640, 480)
		// Larger than 1600 px: scaled down by sips.
		big := filepath.Join(work, "big.jpg")
		img = image.NewRGBA(image.Rect(0, 0, 3200, 2000))
		f, _ = os.Create(big)
		_ = jpeg.Encode(f, img, nil)
		f.Close()
		bigHEIC := filepath.Join(work, "big.heic")
		if out, err := exec.Command(sips, "-s", "format", "heic", big, "--out", bigHEIC).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		checkRealPreview(t, NewSips(sips), bigHEIC, 1600, 1000)
	})
	t.Run("ffmpeg", func(t *testing.T) {
		ffmpeg := realTool(t, "ffmpeg")
		mov := filepath.Join(work, "clip.mov")
		if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
			"-c:v", "mpeg4", "-y", mov).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot generate a clip here: %v %s", err, out)
		}
		checkRealPreview(t, NewFFmpeg(ffmpeg), mov, 320, 240)
		// A clip shorter than the seek position gets its first frame.
		short := filepath.Join(work, "short.mov")
		if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=duration=0.3:size=160x120:rate=10",
			"-c:v", "mpeg4", "-y", short).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		checkRealPreview(t, NewFFmpeg(ffmpeg), short, 160, 120)
		// A playlist dressed up as a movie is refused (no other files read).
		evil := filepath.Join(work, "evil.mov")
		_ = os.WriteFile(evil, []byte("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:10.0,\n"+mov+"\n#EXT-X-ENDLIST\n"), 0600)
		s, _ := newTestStore(t)
		s.SetConverters([]Converter{NewFFmpeg(ffmpeg)})
		b, _ := os.ReadFile(evil)
		sha := pooled(t, s, b)
		if err := s.MakeConvertedPreview(t.Context(), sha, "video/quicktime"); !errors.Is(err, ErrConvert) {
			t.Fatalf("playlist as movie: %v", err)
		}
	})
}

func checkRealPreview(t *testing.T, c Converter, path string, w, h int) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newTestStore(t)
	s.SetConverters([]Converter{c})
	sha := pooled(t, s, body)
	mime := SniffMIME(body)
	if s.PreviewMode(mime) != PreviewExternal {
		t.Fatalf("%s: %s is not converted", c.Name, mime)
	}
	if err := s.MakeConvertedPreview(t.Context(), sha, mime); err != nil {
		t.Fatalf("%s: %v", c.Name, err)
	}
	f, err := os.Open(s.PreviewPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil || cfg.Width != w || cfg.Height != h {
		t.Fatalf("%s preview %+v %v, want %dx%d", c.Name, cfg, err, w, h)
	}
	after, err := os.ReadFile(s.PoolPath(sha))
	if err != nil || !bytes.Equal(after, body) {
		t.Fatalf("%s: original changed (%v)", c.Name, err)
	}
	if fi, err := os.Stat(s.PoolPath(sha)); err != nil || fi.Mode().Perm() != 0400 {
		t.Fatalf("%s: original mode %v %v", c.Name, fi, err)
	}
}
