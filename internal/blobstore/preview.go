package blobstore

import (
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register GIF for image.Decode
	"image/jpeg"
	_ "image/png" // register PNG for image.Decode
	"os"
	"path/filepath"

	"github.com/aaaicu/withalbum/internal/storage"
)

// previewSlots bounds concurrent preview decodes. A decoded image of the
// largest accepted size (40 megapixels) can take a few hundred MiB (16-bit
// PNG), so parallel uploads must not each decode one at the same time.
var previewSlots = make(chan struct{}, 1)

// MakePreview writes the JPEG preview for the pooled original with the given
// hash. It returns an error for files that are not decodable images. A
// panic in an image decoder is returned as an error too: a preview is
// derived data and must never take the upload of the original down with it.
// Previews are made one at a time (previewSlots).
func (s *Store) MakePreview(sha string) (err error) {
	previewSlots <- struct{}{}
	defer func() { <-previewSlots }()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("preview: decoder panic: %v", r)
		}
	}()
	return MakeJPEGPreview(s.PoolPath(sha), s.PreviewPath(sha))
}

// previewTempPrefix is the name prefix of preview temp files.
const previewTempPrefix = "preview-"

// MakeJPEGPreview decodes a JPEG/PNG/GIF image at src and writes a JPEG whose
// longer side is at most 1600px to dst (temp file, fsync, rename, directory
// fsync).
func MakeJPEGPreview(src, dst string) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()
	cfg, _, err := image.DecodeConfig(file)
	if err != nil {
		return err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return errors.New("unsupported dimensions")
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	original, _, err := image.Decode(file)
	if err != nil {
		return err
	}
	return writeJPEGPreview(original, dst)
}

// writeJPEGPreview scales img so that its longer side is at most 1600px
// and writes it as a JPEG to dst (temp file, fsync, rename, directory
// fsync).
func writeJPEGPreview(original image.Image, dst string) error {
	bounds := original.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return errors.New("empty image")
	}
	const maxSide = 1600
	newW, newH := w, h
	if w >= h && w > maxSide {
		newW = maxSide
		newH = max(1, h*maxSide/w)
	}
	if h > w && h > maxSide {
		newH = maxSide
		newW = max(1, w*maxSide/h)
	}
	small := image.NewRGBA(image.Rect(0, 0, newW, newH))
	// Simple nearest-neighbor scaler. Replace with a proper media pipeline in production.
	for y := 0; y < newH; y++ {
		for x := 0; x < newW; x++ {
			small.Set(x, y, original.At(bounds.Min.X+x*w/newW, bounds.Min.Y+y*h/newH))
		}
	}
	if err := storage.MkdirIn(filepath.Dir(dst)); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), previewTempPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err := jpeg.Encode(out, small, &jpeg.Options{Quality: 78}); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), dst); err != nil {
		return err
	}
	// preview_ready is committed after this; make the file's entry durable first.
	return storage.SyncDir(filepath.Dir(dst))
}
