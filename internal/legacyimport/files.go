package legacyimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
)

// progressInterval is how often verifyFiles reports progress.
const progressInterval = 2 * time.Second

// fileCheck is the result of verifyFiles.
type fileCheck struct {
	problems      []string
	warnings      []string
	files         int
	bytes         int64
	previewsReady int
	elapsed       time.Duration
}

// verifyFiles re-hashes the pool file of every asset and checks that each
// asset marked preview_ready has its preview. It only reads. Assets whose key
// is not a valid SHA-256 are skipped here (validate already reports them).
func verifyFiles(ctx context.Context, root string, st *state, progress io.Writer) (fileCheck, error) {
	var fc fileCheck
	bad := func(format string, args ...any) { fc.problems = append(fc.problems, fmt.Sprintf(format, args...)) }
	var total int64
	shas := make([]string, 0, len(st.Assets))
	for _, sha := range sortedKeys(st.Assets) {
		if isSHA(sha) {
			shas = append(shas, sha)
			total += max(st.Assets[sha].Bytes, 0)
		}
	}
	start := time.Now()
	last := start
	report := func(done bool) {
		if progress == nil {
			return
		}
		if !done && time.Since(last) < progressInterval {
			return
		}
		last = time.Now()
		fmt.Fprintf(progress, "verifying originals: %d/%d files, %s/%s\n", fc.files, len(shas), humanBytes(fc.bytes), humanBytes(total))
	}
	for _, sha := range shas {
		if err := ctx.Err(); err != nil {
			return fc, err
		}
		a := st.Assets[sha]
		rel := blobstore.PoolRelPath(sha)
		size, sum, err := hashRegularFile(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			bad("asset %s: original %s is missing", sha, rel)
		case err != nil:
			bad("asset %s: original %s: %v", sha, rel, err)
		case size != a.Bytes:
			bad("asset %s: original %s has %d bytes, state.json says %d", sha, rel, size, a.Bytes)
		case sum != sha:
			bad("asset %s: original %s has SHA-256 %s (content does not match)", sha, rel, sum)
		}
		if err == nil {
			fc.bytes += size
		}
		fc.files++
		if a.PreviewReady {
			prel := blobstore.PreviewRelPath(sha)
			info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(prel)))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				bad("asset %s: preview_ready but preview %s is missing", sha, prel)
			case err != nil:
				bad("asset %s: preview %s: %v", sha, prel, err)
			case !info.Mode().IsRegular():
				bad("asset %s: preview %s is not a regular file", sha, prel)
			case info.Size() == 0:
				bad("asset %s: preview %s is empty", sha, prel)
			default:
				fc.previewsReady++
			}
		}
		report(false)
	}
	report(true)
	fc.elapsed = time.Since(start)
	orphans, err := unreferencedPoolFiles(root, st)
	if err != nil {
		return fc, err
	}
	const maxListed = 20
	for i, rel := range orphans {
		if i == maxListed {
			fc.warnings = append(fc.warnings, fmt.Sprintf("... and %d more unreferenced pool files", len(orphans)-maxListed))
			break
		}
		fc.warnings = append(fc.warnings, fmt.Sprintf("pool file %s is not referenced by state.json (left untouched)", rel))
	}
	return fc, nil
}

// hashRegularFile returns the size and SHA-256 hex of a regular file. Symlinks
// and other non-regular files are refused.
func hashRegularFile(path string) (int64, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() {
		return 0, "", errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// unreferencedPoolFiles lists files under .system/pool that no asset in
// state.json refers to (for example left behind by a PoC crash between the
// pool rename and saving state.json). They are reported, never touched.
func unreferencedPoolFiles(root string, st *state) ([]string, error) {
	pool := filepath.Join(root, ".system", "pool")
	var out []string
	err := filepath.WalkDir(pool, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == pool && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if a, ok := st.Assets[d.Name()]; !ok || !isSHA(a.ID) || blobstore.PoolRelPath(a.ID) != rel {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
