package blobstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/capturetime"
	"github.com/aaaicu/withalbum/internal/storage"
)

// Playback copies of videos (M50): a smaller MP4 (H.264 video, AAC audio,
// at most 1920×1080 in the video's own orientation, moov before mdat so
// that a player can start before the whole file is there) made by an
// external tool after the commit, by the playback worker
// (internal/httpapi playback.go). The original is never changed or moved;
// the copy is derived data like a preview, stored by the original's hash
// in .system/playback, and made again when it is missing.
//
// The tools are run like the preview converters (convert.go): directly
// with a fixed argument list (no shell), a minimal environment, no stdin,
// bounded captured output, in their own process group (a timeout or a
// shutdown kills all of it), at a lower CPU priority, reading the original
// from the pool by its path and writing only into a fresh scratch
// directory in .system/work. The output is accepted only if it is a sane
// MP4 of the expected kind; it is fsynced and renamed into place.
//
//   - macOS: /usr/bin/avconvert (AVFoundation, the hardware encoder) with
//     Preset1920x1080: H.264 + AAC, fast start, the rotation applied, HDR
//     converted to SDR (BT.709), location and other private metadata
//     removed. Its bit rate is chosen by the preset (about 9-11 Mbit/s
//     for 1080p); it cannot be set.
//   - otherwise ffmpeg: h264_videotoolbox (macOS) or libx264, about
//     4.5 Mbit/s, AAC 128 kbit/s, at most 60 frames per second,
//     -movflags +faststart, metadata removed, rotation applied
//     (autorotate). HDR is tone mapped to BT.709 only when the build has
//     the zscale filter; otherwise an HDR video is converted with its
//     colours as they are (they look flat).

// Transcoder makes the playback copy of the video at src into out (which
// does not exist yet), using dir as its scratch directory. ctx carries the
// time limit; the tool must stop when it ends.
type Transcoder interface {
	Name() string
	Transcode(ctx context.Context, src, out, dir string) error
}

// Playback errors (MakePlayback).
var (
	// ErrNoTranscoder: this server has no playback tool.
	ErrNoTranscoder = errors.New("no playback transcoder")
	// ErrPlaybackNotSmaller: the copy would not be meaningfully smaller
	// than the original (at most PlaybackMaxFraction of it); nothing is
	// stored and clients stream the original.
	ErrPlaybackNotSmaller = errors.New("playback copy would not be smaller than the original")
	// ErrTranscode: the tool failed or its output was refused. It may be
	// tried again later.
	ErrTranscode = errors.New("playback transcode failed")
	// ErrTranscodeTimeout (an ErrTranscode): the tool ran out of time.
	ErrTranscodeTimeout = fmt.Errorf("%w: timed out", ErrTranscode)
	// ErrPlaybackOutput (an ErrTranscode): the tool wrote something that is
	// not an acceptable copy.
	ErrPlaybackOutput = fmt.Errorf("%w: output refused", ErrTranscode)
	// ErrOriginalChanged: the original's size, mode or modification time
	// differed after the tool ran. Nothing is stored; this must be looked
	// into.
	ErrOriginalChanged = errors.New("INTEGRITY: original changed while the playback tool ran")
)

const (
	// PlaybackRelDir holds the copies: <sha[:2]>/<sha>.mp4.
	PlaybackRelDir = ".system/playback"
	// PlaybackMaxSide bounds each side of a copy.
	PlaybackMaxSide = 1920
	// playbackWorkPrefix names the scratch directories in .system/work.
	playbackWorkPrefix = "playback-"
	// maxPlaybackMoov bounds the movie header of a copy that is read to
	// check it.
	maxPlaybackMoov = 64 << 20
	// playbackOutputSlack: the tool's output may exceed the original by
	// this much while it runs (headers, fast start rewrite) before it is
	// stopped; such a copy is refused anyway.
	playbackOutputSlack = 16 << 20
	// playbackLogLimit bounds the tool output kept (an ffmpeg probe lists
	// the streams after the metadata, so more than toolLogLimit).
	playbackLogLimit = 64 << 10
)

// PlaybackMaxFraction: a copy larger than this share of the original is
// not stored (ErrPlaybackNotSmaller).
const PlaybackMaxFraction = 0.8

// playbackWatchInterval is how often the output size is checked while a
// tool runs (a variable for tests).
var playbackWatchInterval = time.Second

// PlaybackFile describes a stored copy.
type PlaybackFile struct {
	Bytes         int64
	Width, Height int
}

// PlaybackRelPath is PlaybackPath relative to the storage root.
func PlaybackRelPath(sha string) string {
	return PlaybackRelDir + "/" + sha[:2] + "/" + sha + ".mp4"
}

// PlaybackPath is where the playback copy of the original sha is stored.
func (s *Store) PlaybackPath(sha string) string {
	return filepath.Join(s.root, filepath.FromSlash(PlaybackRelPath(sha)))
}

// SetTranscoder sets the playback tool (nil: none). It must be called
// before the store is used concurrently.
func (s *Store) SetTranscoder(t Transcoder) { s.transcoder = t }

// Transcoder returns the playback tool, nil when there is none.
func (s *Store) Transcoder() Transcoder { return s.transcoder }

// MakePlayback makes and stores the playback copy of the pooled original
// sha. The tool gets at most limit. Errors: ErrNoTranscoder,
// ErrPlaybackNotSmaller, ErrTranscode (ErrTranscodeTimeout,
// ErrPlaybackOutput), ErrOriginalChanged, ctx.Err() when ctx ends, and
// storage errors (the volume, the disk) that mean "try again later".
func (s *Store) MakePlayback(ctx context.Context, sha string, limit time.Duration) (PlaybackFile, error) {
	t := s.transcoder
	if t == nil {
		return PlaybackFile{}, ErrNoTranscoder
	}
	if err := s.vol.Check(); err != nil {
		return PlaybackFile{}, err
	}
	src := s.PoolPath(sha)
	before, err := os.Lstat(src)
	if err != nil {
		return PlaybackFile{}, err
	}
	if !before.Mode().IsRegular() {
		return PlaybackFile{}, fmt.Errorf("%w: original is not a regular file", ErrPlaybackOutput)
	}
	work, err := os.MkdirTemp(filepath.Join(s.root, filepath.FromSlash(WorkRelDir)), playbackWorkPrefix+"*")
	if err != nil {
		return PlaybackFile{}, err
	}
	defer os.RemoveAll(work)
	out := filepath.Join(work, "out.mp4")

	tctx, cancel := context.WithTimeoutCause(ctx, limit, ErrTranscodeTimeout)
	defer cancel()
	wctx, stop := context.WithCancelCause(tctx)
	defer stop(nil)
	// Stop a tool whose output grows beyond the original: it would not be
	// stored anyway, and it must not fill the disk.
	maxOut := before.Size() + playbackOutputSlack
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		tick := time.NewTicker(playbackWatchInterval)
		defer tick.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-tick.C:
				if fi, err := os.Lstat(out); err == nil && fi.Size() > maxOut {
					stop(fmt.Errorf("%w: output passed %d bytes", ErrPlaybackOutput, maxOut))
					return
				}
			}
		}
	}()
	runErr := t.Transcode(wctx, src, out, work)
	stop(nil)
	<-watchDone
	if err := ctx.Err(); err != nil {
		return PlaybackFile{}, err // shutdown: not the tool's fault
	}
	// Whatever happened, the original must be exactly as it was.
	after, err := os.Lstat(src)
	if err != nil || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		return PlaybackFile{}, fmt.Errorf("%w: %s (%s, %v)", ErrOriginalChanged, sha, t.Name(), err)
	}
	if cause := context.Cause(wctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return PlaybackFile{}, cause // the time limit or the output bound
	}
	if runErr != nil {
		return PlaybackFile{}, fmt.Errorf("%w: %s: %w", ErrTranscode, t.Name(), runErr)
	}
	fi, err := os.Lstat(out)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return PlaybackFile{}, fmt.Errorf("%w: %s wrote no file", ErrPlaybackOutput, t.Name())
	}
	w, h, err := CheckPlaybackMP4(out)
	if err != nil {
		return PlaybackFile{}, fmt.Errorf("%w: %s: %w", ErrPlaybackOutput, t.Name(), err)
	}
	if float64(fi.Size()) > float64(before.Size())*PlaybackMaxFraction {
		return PlaybackFile{}, ErrPlaybackNotSmaller
	}
	if err := syncFile(out); err != nil {
		return PlaybackFile{}, err
	}
	_ = os.Chmod(out, 0400)
	if err := s.vol.Check(); err != nil {
		return PlaybackFile{}, err
	}
	dst := s.PlaybackPath(sha)
	dir := filepath.Dir(dst)
	_, statErr := os.Lstat(dir)
	if err := storage.MkdirIn(dir); err != nil {
		return PlaybackFile{}, err
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := storage.SyncDir(filepath.Dir(dir)); err != nil {
			return PlaybackFile{}, err
		}
	}
	if err := os.Rename(out, dst); err != nil {
		return PlaybackFile{}, err
	}
	// The ready state is committed after this; make the entry durable first.
	if err := storage.SyncDir(dir); err != nil {
		return PlaybackFile{}, err
	}
	return PlaybackFile{Bytes: fi.Size(), Width: w, Height: h}, nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// PlaybackExists reports whether the copy of sha is a regular file.
func (s *Store) PlaybackExists(sha string) (bool, error) {
	n, err := s.PlaybackSize(sha)
	return n > 0, err
}

// PlaybackSize is the size of the copy of sha, 0 when there is none (or
// it is not a regular file).
func (s *Store) PlaybackSize(sha string) (int64, error) {
	fi, err := os.Lstat(s.PlaybackPath(sha))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, nil
	}
	return fi.Size(), nil
}

// RemovePlayback deletes the copy of sha (none is fine). Only the copy:
// never anything in the pool.
func (s *Store) RemovePlayback(sha string) error {
	if err := s.vol.Check(); err != nil {
		return err
	}
	path := s.PlaybackPath(sha)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return storage.SyncDir(filepath.Dir(path))
}

// SweepPlayback deletes the copies whose original keep does not report
// as ready (left by a crash between the rename and the database, or by a
// release whose deletion failed). Entries that are not copies are left
// alone. It returns how many files it deleted.
func (s *Store) SweepPlayback(ctx context.Context, keep func(sha string) (bool, error)) (int, error) {
	root := filepath.Join(s.root, filepath.FromSlash(PlaybackRelDir))
	prefixes, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, p := range prefixes {
		if !p.IsDir() || len(p.Name()) != 2 || !isHex(p.Name()) {
			continue
		}
		dir := filepath.Join(root, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return removed, err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			sha, ok := strings.CutSuffix(e.Name(), ".mp4")
			if !ok || len(sha) != 64 || !isHex(sha) || sha[:2] != p.Name() || !e.Type().IsRegular() {
				continue
			}
			ok, err := keep(sha)
			if err != nil {
				return removed, err
			}
			if ok {
				continue
			}
			if err := s.vol.Check(); err != nil {
				return removed, err
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// CheckPlaybackMP4 accepts a playback copy only if it is an ISO base media
// file that starts with ftyp, has its moov before its mdat (fast start),
// an H.264 ("avc1") sample entry, and a displayed size within
// PlaybackMaxSide. It returns that size. It only reads the top-level box
// headers and the movie header.
func CheckPlaybackMP4(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := fi.Size()
	var moovAt, moovEnd, mdatAt int64 = -1, -1, -1
	hdr := make([]byte, 16)
	for i, off := 0, int64(0); off < size; i++ {
		if i >= 64 {
			return 0, 0, errors.New("too many top-level boxes")
		}
		if off+8 > size {
			return 0, 0, errors.New("truncated box header")
		}
		if _, err := f.ReadAt(hdr[:8], off); err != nil {
			return 0, 0, err
		}
		n, typ := int64(binary.BigEndian.Uint32(hdr[:4])), string(hdr[4:8])
		switch n {
		case 0:
			n = size - off
		case 1:
			if off+16 > size {
				return 0, 0, errors.New("truncated box header")
			}
			if _, err := f.ReadAt(hdr[8:16], off+8); err != nil {
				return 0, 0, err
			}
			u := binary.BigEndian.Uint64(hdr[8:16])
			if u > uint64(size) {
				return 0, 0, errors.New("box past the end of the file")
			}
			n = int64(u)
		}
		if n < 8 || off+n > size {
			return 0, 0, errors.New("box past the end of the file")
		}
		if i == 0 && typ != "ftyp" {
			return 0, 0, errors.New("does not start with ftyp")
		}
		switch typ {
		case "moov":
			if moovAt < 0 {
				moovAt, moovEnd = off, off+n
			}
		case "mdat":
			if mdatAt < 0 {
				mdatAt = off
			}
		}
		off += n
	}
	switch {
	case moovAt < 0 || mdatAt < 0:
		return 0, 0, errors.New("no moov or no mdat")
	case moovAt > mdatAt:
		return 0, 0, errors.New("moov after mdat (not fast start)")
	case moovEnd-moovAt > maxPlaybackMoov:
		return 0, 0, errors.New("movie header too large")
	}
	moov := make([]byte, moovEnd-moovAt)
	if _, err := f.ReadAt(moov, moovAt); err != nil && !errors.Is(err, io.EOF) {
		return 0, 0, err
	}
	if !bytes.Contains(moov, []byte("avc1")) {
		return 0, 0, errors.New("no H.264 video track")
	}
	m, err := capturetime.ExtractMediaInfo(f, size)
	if err != nil {
		return 0, 0, err
	}
	if m.Width <= 0 || m.Height <= 0 || m.Width > PlaybackMaxSide || m.Height > PlaybackMaxSide {
		return 0, 0, fmt.Errorf("unexpected size %dx%d", m.Width, m.Height)
	}
	return m.Width, m.Height, nil
}

// toolTranscoder runs an external tool (avconvert, ffmpeg).
type toolTranscoder struct {
	name, path string
	args       func(ctx context.Context, src, out, dir string) ([]string, error)
}

func (t *toolTranscoder) Name() string { return t.name }

func (t *toolTranscoder) Transcode(ctx context.Context, src, out, dir string) error {
	args, err := t.args(ctx, src, out, dir)
	if err != nil {
		return err
	}
	_, err = runPlaybackTool(ctx, t.path, args, dir, toolLogLimit)
	return err
}

// AvconvertPreset is the avconvert preset of playback copies.
const AvconvertPreset = "Preset1920x1080"

// NewAvconvert returns the avconvert transcoder (macOS) for the executable
// at path.
func NewAvconvert(path string) Transcoder {
	return &toolTranscoder{name: "avconvert", path: path, args: func(_ context.Context, src, out, dir string) ([]string, error) {
		// AVFoundation tells the container by the name's extension and
		// pool files have none: it reads the original through a symbolic
		// link in the scratch directory (QuickTime reads MP4 too). Paths
		// are absolute (never taken for an option); the metadata filter
		// stays on (location and other private metadata dropped) and fast
		// start is the default.
		link := filepath.Join(dir, "source.mov")
		if err := os.Symlink(src, link); err != nil {
			return nil, err
		}
		return []string{"--source", link, "--preset", AvconvertPreset, "--output", out}, nil
	}}
}

// FFmpeg encoders for playback copies.
const (
	EncoderVideoToolbox = "h264_videotoolbox"
	EncoderX264         = "libx264"
)

// ffmpegScale fits the frame (after autorotate) into 1920×1080 in its own
// orientation, never enlarging it, with even sides.
const ffmpegScale = "scale=w='min(iw,if(gte(iw,ih),1920,1080))':h='min(ih,if(gte(iw,ih),1080,1920))':force_original_aspect_ratio=decrease:force_divisible_by=2"

// ffmpegToneMap converts HDR (PQ, HLG) to SDR BT.709 (needs zscale).
const ffmpegToneMap = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"

// NewFFmpegPlayback returns the ffmpeg transcoder for the executable at
// path with encoder (EncoderVideoToolbox or EncoderX264); toneMap says the
// build has the zscale filter.
func NewFFmpegPlayback(path, encoder string, toneMap bool) Transcoder {
	return &toolTranscoder{name: "ffmpeg", path: path, args: func(ctx context.Context, src, out, dir string) ([]string, error) {
		input := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "2", "-max_alloc", "536870912"}
		if encoder == EncoderVideoToolbox {
			input = append(input, "-hwaccel", "videotoolbox")
		}
		input = append(input, "-protocol_whitelist", "file", "-format_whitelist", ffmpegFormats, "-i", src)
		hdr := false
		if toneMap {
			// The stream list of "ffmpeg -i" (it exits with an error: no
			// output was named) tells the transfer function.
			info, _ := runPlaybackTool(ctx, path, []string{"-nostdin", "-hide_banner", "-protocol_whitelist", "file",
				"-format_whitelist", ffmpegFormats, "-i", src}, dir, playbackLogLimit)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			hdr = isHDRStreamInfo(info)
		}
		vf := ffmpegScale
		if hdr {
			vf += "," + ffmpegToneMap
		}
		vf += ",format=yuv420p"
		args := append(input, "-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
			"-filter_threads", "2", "-vf", vf, "-fpsmax", "60", "-threads", "2")
		switch encoder {
		case EncoderVideoToolbox:
			args = append(args, "-c:v", EncoderVideoToolbox, "-b:v", "4500k", "-maxrate", "6000k", "-bufsize", "9000k", "-profile:v", "high")
		default:
			args = append(args, "-c:v", EncoderX264, "-preset", "veryfast", "-crf", "23", "-maxrate", "4500k", "-bufsize", "9000k", "-profile:v", "high")
		}
		if hdr {
			args = append(args, "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
		}
		return append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-movflags", "+faststart", "-f", "mp4", "-y", out), nil
	}}
}

// isHDRStreamInfo reports whether the video stream lines of "ffmpeg -i"
// name an HDR transfer function (PQ or HLG).
func isHDRStreamInfo(info string) bool {
	for _, line := range strings.Split(info, "\n") {
		if strings.Contains(line, "Video:") && (strings.Contains(line, "smpte2084") || strings.Contains(line, "arib-std-b67")) {
			return true
		}
	}
	return false
}

// FindTranscoder picks the playback tool. getenv("WITHALBUM_PLAYBACK"):
// "off" disables playback copies, "avconvert" or "ffmpeg" forces that
// tool, "" (auto) prefers avconvert on macOS and ffmpeg otherwise.
// WITHALBUM_AVCONVERT and WITHALBUM_FFMPEG name the executables like for
// previews. ffmpeg is used only with a usable H.264 encoder
// (h264_videotoolbox on macOS, else libx264), which is asked of it once
// here. It returns nil when there is none.
func FindTranscoder(ctx context.Context, getenv func(string) string) Transcoder {
	mode := getenv("WITHALBUM_PLAYBACK")
	if mode == "off" {
		return nil
	}
	if mode != "ffmpeg" && runtime.GOOS == "darwin" {
		if p := findTool("avconvert", getenv("WITHALBUM_AVCONVERT")); p != "" {
			return NewAvconvert(p)
		}
	}
	if mode == "avconvert" {
		return nil
	}
	p := findTool("ffmpeg", getenv("WITHALBUM_FFMPEG"))
	if p == "" {
		return nil
	}
	encoder, toneMap := probeFFmpeg(ctx, p)
	if encoder == "" {
		return nil
	}
	return NewFFmpegPlayback(p, encoder, toneMap)
}

// probeFFmpeg asks ffmpeg for its encoders and filters.
func probeFFmpeg(ctx context.Context, path string) (encoder string, toneMap bool) {
	dir, err := os.MkdirTemp("", "withalbum-ffmpeg-probe-")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	encoders, err := runPlaybackTool(ctx, path, []string{"-hide_banner", "-nostdin", "-encoders"}, dir, playbackLogLimit)
	if err != nil {
		return "", false
	}
	has := func(list, name string) bool {
		for _, line := range strings.Split(list, "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[1] == name {
				return true
			}
		}
		return false
	}
	switch {
	case runtime.GOOS == "darwin" && has(encoders, EncoderVideoToolbox):
		encoder = EncoderVideoToolbox
	case has(encoders, EncoderX264):
		encoder = EncoderX264
	default:
		return "", false
	}
	if !has(encoders, "aac") {
		return "", false
	}
	filters, err := runPlaybackTool(ctx, path, []string{"-hide_banner", "-nostdin", "-filters"}, dir, playbackLogLimit)
	return encoder, err == nil && has(filters, "zscale") && has(filters, "tonemap")
}

// runPlaybackTool runs path with args in dir, isolated like runTool, at a
// lower CPU priority, until it exits or ctx ends, and returns the start of
// its output (up to limit bytes).
func runPlaybackTool(ctx context.Context, path string, args []string, dir string, limit int) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- absolute tool path from FindTranscoder, argument array, no shell
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "LC_ALL=C"}
	cmd.Stdin = nil
	output := &boundedBuffer{limit: limit}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = 5 * time.Second
	isolateProcess(cmd)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	lowerPriority(cmd.Process.Pid)
	err := cmd.Wait()
	text := output.buf.String()
	if ctx.Err() != nil {
		return text, context.Cause(ctx)
	}
	if err != nil {
		return text, fmt.Errorf("%w: %s", err, strings.TrimSpace(lastLines(text, 4)))
	}
	return text, nil
}

// lastLines returns the last n lines of s (the error a tool printed last).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
