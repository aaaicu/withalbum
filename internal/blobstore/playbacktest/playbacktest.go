// Package playbacktest builds small, hand-made movies for tests of the
// playback copies (blobstore.MakePlayback) and a fake Transcoder. The
// movies hold only the structures the server reads (top-level boxes, a
// track header, an H.264 sample entry); no real video is used. Import it
// only from tests.
package playbacktest

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"sync"
)

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// Box returns an ISO BMFF box of type typ with the given contents.
func Box(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	return append(append(be32(uint32(8+len(body))), typ...), body...)
}

// Moov is a movie header with one video track of w×h (identity matrix)
// whose sample entry is codec (e.g. "avc1").
func Moov(w, h uint32, codec string) []byte {
	tk := []byte{0, 0, 0, 7}
	tk = append(tk, make([]byte, 36)...)
	for _, v := range []uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000} {
		tk = append(tk, be32(v)...)
	}
	tk = append(tk, be32(w<<16)...)
	tk = append(tk, be32(h<<16)...)
	mvhd := Box("mvhd", []byte{0, 0, 0, 0}, make([]byte, 8), be32(600), be32(1800), make([]byte, 80))
	stsd := Box("stsd", []byte{0, 0, 0, 0}, be32(1), Box(codec, make([]byte, 78)))
	mdia := Box("mdia", Box("hdlr", make([]byte, 8), []byte("vide"), make([]byte, 13)), Box("minf", Box("stbl", stsd)))
	return Box("moov", mvhd, Box("trak", Box("tkhd", tk), mdia))
}

// MP4 is an MP4 of w×h H.264 video with an mdat of mdat bytes (filled
// with seed), the moov first when fastStart.
func MP4(w, h uint32, mdat int, seed byte, fastStart bool) []byte {
	ftyp := Box("ftyp", []byte("isom"), be32(0x200), []byte("isomavc1mp41"))
	data := Box("mdat", bytes.Repeat([]byte{seed}, mdat))
	if fastStart {
		return bytes.Join([][]byte{ftyp, Moov(w, h, "avc1"), data}, nil)
	}
	return bytes.Join([][]byte{ftyp, data, Moov(w, h, "avc1")}, nil)
}

// Movie is a QuickTime original (sniffed video/quicktime) of w×h with an
// mdat of mdat bytes filled with seed.
func Movie(w, h uint32, mdat int, seed byte) []byte {
	return bytes.Join([][]byte{
		Box("ftyp", []byte("qt  "), make([]byte, 4), []byte("qt  ")),
		Box("mdat", bytes.Repeat([]byte{seed}, mdat)),
		Moov(w, h, "hvc1"),
	}, nil)
}

// Fake is a Transcoder whose behavior a test sets. The zero value writes
// a fast-start 720×1280 MP4 of 1000 bytes.
type Fake struct {
	mu    sync.Mutex
	calls int
	// Run replaces what a call does (nil: the default output).
	Run func(ctx context.Context, src, out string) error
}

// Name implements blobstore.Transcoder.
func (f *Fake) Name() string { return "fake" }

// Transcode implements blobstore.Transcoder.
func (f *Fake) Transcode(ctx context.Context, src, out, dir string) error {
	f.mu.Lock()
	f.calls++
	run := f.Run
	f.mu.Unlock()
	if run == nil {
		return os.WriteFile(out, MP4(720, 1280, 1000, 1, true), 0600)
	}
	return run(ctx, src, out)
}

// Set replaces Run.
func (f *Fake) Set(run func(ctx context.Context, src, out string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Run = run
}

// Calls returns how many times Transcode ran.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
