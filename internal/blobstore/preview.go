package blobstore

import (
	"errors"
	"image"
	_ "image/gif" // register GIF for image.Decode
	"image/jpeg"
	_ "image/png" // register PNG for image.Decode
	"os"
	"path/filepath"

	"github.com/aaaicu/withalbum/internal/storage"
)

// MakePreview writes the JPEG preview for the pooled original with the given
// hash. It returns an error for files that are not decodable images.
func (s *Store) MakePreview(sha string) error {
	return MakeJPEGPreview(s.PoolPath(sha), s.PreviewPath(sha))
}

// MakeJPEGPreview decodes a JPEG/PNG/GIF image at src and writes a JPEG whose
// longer side is at most 1600px to dst (temp file, fsync, rename).
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
	bounds := original.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
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
	out, err := os.CreateTemp(filepath.Dir(dst), "preview-*")
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
	return os.Rename(out.Name(), dst)
}
