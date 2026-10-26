package blobstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Previews of types the Go standard library cannot decode (HEIC/HEIF/AVIF
// stills, videos) are made by optional external tools: macOS sips and
// ffmpeg. They are found at startup (FindConverters); when a tool is
// missing, its types simply have no preview ("unsupported") and the
// original is stored as always. A tool:
//
//   - is run directly with an argument list (never through a shell), with
//     a minimal environment, no stdin, captured and bounded output, a
//     timeout, and in its own process group so that a timeout kills every
//     process it started;
//   - reads the original from the pool, where it is read-only (0400), and
//     the original's size and mode are checked again afterwards;
//   - writes only into a fresh scratch directory in .system/work, which is
//     removed afterwards (and at startup, after a crash);
//   - has its output accepted only if it decodes as a JPEG of sane
//     dimensions; the stored preview is re-encoded from the decoded image
//     by the same code as built-in previews.
//
// ffmpeg is limited to local files (-protocol_whitelist file) and to the
// usual container formats (-format_whitelist), so a crafted "video" cannot
// make it read other files or the network, and to one thread and 256 MiB
// per allocation. There is no portable way to cap a child's total memory
// from Go without cgo or a shell; conversions run one at a time
// (convertSlots) and still images larger than maxConvertInput are not
// converted.

// Converter runs one external preview tool.
type Converter struct {
	Name string // "sips" or "ffmpeg"
	Path string // absolute path of the executable
	// Types are the media types (SniffMIME) it converts; an entry ending in
	// "/" is a prefix.
	Types []string
	// MaxInput bounds the original's size (0: no bound).
	MaxInput int64
	// attempts returns the argument lists to try in order until one writes
	// out (a video shorter than the seek position gets a second try). It
	// may run the tool itself (in dir) to inspect src.
	attempts func(ctx context.Context, src, out, dir string) ([][]string, error)
}

// Preview modes (PreviewMode).
const (
	PreviewBuiltin     = "builtin"     // decoded in-process (JPEG, PNG, GIF)
	PreviewExternal    = "external"    // a Converter handles it
	PreviewUnsupported = "unsupported" // no preview for this type
)

// ErrNoConverter: no converter for this type on this server.
var ErrNoConverter = errors.New("no preview converter for this media type")

// ErrConvert: a converter ran but made no acceptable preview. The preview
// is "failed"; it is not retried.
var ErrConvert = errors.New("preview conversion failed")

const (
	maxConvertInput = 256 << 20 // still images
	// maxConvertPixels bounds the canvas of a still image given to sips
	// (decoded at 4 bytes per pixel: about 480 MiB). Panoramas and 48 MP
	// phone photos are well below it.
	maxConvertPixels = 120_000_000
	maxToolOutput    = 32 << 20 // bytes of the JPEG a tool writes
	toolLogLimit     = 2048     // bytes of a tool's stdout/stderr kept for the log
	workPrefix       = "preview-"
)

// convertSlots runs one external conversion at a time. It is separate from
// previewSlots so that a long video conversion never holds up the built-in
// preview an upload waits for.
var convertSlots = make(chan struct{}, 1)

// convertTimeout bounds one run of a converter (a variable for tests).
var convertTimeout = 90 * time.Second

// WorkRelDir is the scratch space of preview converters.
const WorkRelDir = ".system/work"

var builtinTypes = []string{"image/jpeg", "image/png", "image/gif"}

func typeMatches(list []string, mime string) bool {
	for _, t := range list {
		if t == mime || strings.HasSuffix(t, "/") && strings.HasPrefix(mime, t) {
			return true
		}
	}
	return false
}

// SetConverters sets the external converters, in order of preference. It
// must be called before the store is used concurrently.
func (s *Store) SetConverters(cs []Converter) { s.converters = cs }

// Converters returns the configured converters.
func (s *Store) Converters() []Converter { return s.converters }

// PreviewMode says how a preview for mime is made.
func (s *Store) PreviewMode(mime string) string {
	if typeMatches(builtinTypes, mime) {
		return PreviewBuiltin
	}
	for _, c := range s.converters {
		if typeMatches(c.Types, mime) {
			return PreviewExternal
		}
	}
	return PreviewUnsupported
}

// sipsTypes are the still image types sips (ImageIO) reads.
var sipsTypes = []string{"image/heic", "image/heif", "image/avif", "image/webp", "image/tiff", "image/bmp"}

// ffmpegFormats are the demuxers ffmpeg may use for an original.
const ffmpegFormats = "mov,mp4,m4a,3gp,3g2,mj2,matroska,webm,avi"

// NewSips returns the sips converter for the executable at path. sips -Z
// also enlarges, so the size is read first and only larger images are
// scaled down.
func NewSips(path string) Converter {
	return Converter{Name: "sips", Path: path, Types: sipsTypes, MaxInput: maxConvertInput,
		attempts: func(ctx context.Context, src, out, dir string) ([][]string, error) {
			info, err := runTool(ctx, path, []string{"-g", "pixelWidth", "-g", "pixelHeight", src}, dir)
			if err != nil {
				return nil, err
			}
			w, h := sipsDimension(info, "pixelWidth"), sipsDimension(info, "pixelHeight")
			if w <= 0 || h <= 0 {
				return nil, fmt.Errorf("cannot read the image size: %q", info)
			}
			// A small file can declare a huge canvas (a decompression
			// bomb); sips would decode all of it in memory.
			if int64(w)*int64(h) > maxConvertPixels {
				return nil, fmt.Errorf("image of %dx%d pixels is above the %d limit", w, h, maxConvertPixels)
			}
			args := []string{"-s", "format", "jpeg", "-s", "formatOptions", "85"}
			if max(w, h) > 1600 {
				args = append(args, "-Z", "1600")
			}
			return [][]string{append(args, src, "--out", out)}, nil
		}}
}

// sipsDimension reads "  key: N" from sips -g output.
func sipsDimension(info, key string) int {
	for _, line := range strings.Split(info, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && k == key {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil {
				return n
			}
		}
	}
	return 0
}

// NewFFmpeg returns the ffmpeg converter (videos) for the executable at path.
func NewFFmpeg(path string) Converter {
	return Converter{Name: "ffmpeg", Path: path, Types: []string{"video/"},
		attempts: func(_ context.Context, src, out, _ string) ([][]string, error) {
			args := func(seek []string) []string {
				a := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1", "-max_alloc", "268435456"}
				a = append(a, seek...)
				a = append(a, "-protocol_whitelist", "file", "-format_whitelist", ffmpegFormats, "-i", src,
					"-map", "0:v:0", "-frames:v", "1", "-an", "-sn", "-dn",
					"-vf", "scale=w='min(1600,iw)':h='min(1600,ih)':force_original_aspect_ratio=decrease",
					"-f", "image2", "-c:v", "mjpeg", "-q:v", "3", "-y", out)
				return a
			}
			// A frame one second in avoids the black first frame of many
			// clips; shorter clips get their first frame.
			return [][]string{args([]string{"-ss", "1"}), args(nil)}, nil
		}}
}

// toolDirs are searched after PATH: a service started by launchd has a
// minimal PATH without Homebrew.
var toolDirs = []string{"/usr/bin", "/opt/homebrew/bin", "/usr/local/bin"}

// FindConverters looks for sips and ffmpeg. getenv("WITHALBUM_PREVIEW_TOOLS")
// == "off" disables both; WITHALBUM_SIPS / WITHALBUM_FFMPEG name an
// executable explicitly ("" searches PATH and the usual directories, "off"
// disables that tool). Only absolute paths of regular executable files are
// accepted.
func FindConverters(getenv func(string) string) []Converter {
	if getenv("WITHALBUM_PREVIEW_TOOLS") == "off" {
		return nil
	}
	var out []Converter
	if p := findTool("sips", getenv("WITHALBUM_SIPS")); p != "" {
		out = append(out, NewSips(p))
	}
	if p := findTool("ffmpeg", getenv("WITHALBUM_FFMPEG")); p != "" {
		out = append(out, NewFFmpeg(p))
	}
	return out
}

func findTool(name, configured string) string {
	if configured == "off" {
		return ""
	}
	candidates := []string{configured}
	if configured == "" {
		candidates = nil
		if p, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, p)
		}
		for _, d := range toolDirs {
			candidates = append(candidates, filepath.Join(d, name))
		}
	}
	for _, p := range candidates {
		if !filepath.IsAbs(p) {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0 {
			return p
		}
	}
	return ""
}

// MakeConvertedPreview writes the preview of the pooled original sha of
// type mime with the first converter that handles the type. ErrNoConverter,
// ErrConvert (wrapping the reason); other errors (storage, cancellation)
// mean the preview may be tried again later.
func (s *Store) MakeConvertedPreview(ctx context.Context, sha, mime string) (err error) {
	convertSlots <- struct{}{}
	defer func() { <-convertSlots }()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: decoder panic: %v", ErrConvert, r)
		}
	}()
	var conv *Converter
	for i := range s.converters {
		if typeMatches(s.converters[i].Types, mime) {
			conv = &s.converters[i]
			break
		}
	}
	if conv == nil {
		return ErrNoConverter
	}
	if err := s.vol.Check(); err != nil {
		return err
	}
	src := s.PoolPath(sha)
	before, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("%w: original is not a regular file", ErrConvert)
	}
	if conv.MaxInput > 0 && before.Size() > conv.MaxInput {
		return fmt.Errorf("%w: original has %d bytes, %s limit %d", ErrConvert, before.Size(), conv.Name, conv.MaxInput)
	}
	work, err := os.MkdirTemp(filepath.Join(s.root, filepath.FromSlash(WorkRelDir)), workPrefix+"*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	out := filepath.Join(work, "out.jpg")
	attempts, runErr := conv.attempts(ctx, src, out, work)
	for _, args := range attempts {
		_ = os.Remove(out)
		if _, runErr = runTool(ctx, conv.Path, args, work); runErr == nil {
			if fi, err := os.Lstat(out); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
				break
			}
			runErr = errors.New("no output")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Whatever happened, the original must be exactly as it was.
	after, err := os.Lstat(src)
	if err != nil || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		return fmt.Errorf("%w: INTEGRITY: original %s changed while %s ran (%v)", ErrConvert, sha, conv.Name, err)
	}
	if runErr != nil {
		return fmt.Errorf("%w: %s: %w", ErrConvert, conv.Name, runErr)
	}
	img, err := decodeToolJPEG(out)
	if err != nil {
		return fmt.Errorf("%w: %s output: %w", ErrConvert, conv.Name, err)
	}
	if err := s.vol.Check(); err != nil {
		return err
	}
	return writeJPEGPreview(img, s.PreviewPath(sha))
}

// decodeToolJPEG accepts a converter's output only if it is a JPEG of sane
// size and dimensions.
func decodeToolJPEG(path string) (image.Image, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxToolOutput {
		return nil, fmt.Errorf("unexpected output file (%d bytes)", fi.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 {
		return nil, fmt.Errorf("unexpected dimensions %dx%d", cfg.Width, cfg.Height)
	}
	return jpeg.Decode(bytes.NewReader(b))
}

// boundedBuffer keeps the first limit bytes written to it.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// runTool runs path with args in dir, isolated as described above, and
// returns the start of its output.
func runTool(ctx context.Context, path string, args []string, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- absolute tool path from FindConverters, argument array, no shell
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "LC_ALL=C"}
	cmd.Stdin = nil
	output := &boundedBuffer{limit: toolLogLimit}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = 5 * time.Second
	isolateProcess(cmd)
	err := cmd.Run()
	text := output.buf.String()
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("timed out after %s", convertTimeout)
	}
	if err != nil {
		return text, fmt.Errorf("%w: %s", err, strings.TrimSpace(text))
	}
	return text, nil
}

// cleanWork removes converter and playback tool scratch directories left
// by a crash.
func (s *Store) cleanWork() error {
	dir := filepath.Join(s.root, filepath.FromSlash(WorkRelDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), workPrefix) && !strings.HasPrefix(e.Name(), playbackWorkPrefix) {
			continue
		}
		if err := s.vol.Check(); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
