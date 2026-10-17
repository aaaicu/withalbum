package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsMountPoint(t *testing.T) {
	dir := t.TempDir()
	if ok, err := IsMountPoint(dir); err != nil || ok {
		t.Fatalf("temp dir: %v %v", ok, err)
	}
	if ok, err := IsMountPoint("/"); err != nil || !ok {
		t.Fatalf("/: %v %v", ok, err)
	}
	if _, err := IsMountPoint(filepath.Join(dir, "missing")); !errors.Is(err, ErrRootMissing) {
		t.Fatalf("missing: %v", err)
	}
}

// TestMkdirOnMountRefusesPlainDirectories: a directory that is not a mount
// point (an unplugged disk's leftover path) gets no folder.
func TestMkdirOnMountRefusesPlainDirectories(t *testing.T) {
	dir := t.TempDir()
	if _, err := MkdirOnMount(dir, "WithAlbum"); !errors.Is(err, ErrNotMounted) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "WithAlbum")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("folder created on a directory that is not mounted")
	}
	if _, err := MkdirOnMount(filepath.Join(dir, "gone"), "WithAlbum"); err == nil {
		t.Fatal("missing disk accepted")
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing disk path was created")
	}
}

func TestMkdirOne(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", ".", "..", "a/b", "../x", ".hidden"} {
		if _, err := MkdirOne(dir, bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := MkdirOne(filepath.Join(dir, "missing"), "WithAlbum"); !errors.Is(err, ErrRootMissing) {
		t.Fatalf("missing parent: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("parent created")
	}
	p, err := MkdirOne(dir, "WithAlbum")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi, err)
	}
	if again, err := MkdirOne(dir, "WithAlbum"); err != nil || again != p {
		t.Fatalf("existing: %q %v", again, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MkdirOne(dir, "file"); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("file: %v", err)
	}
}

func TestReadContents(t *testing.T) {
	check := func(name string, prep func(dir string), want Contents) {
		t.Helper()
		dir := t.TempDir()
		prep(dir)
		if got, err := ReadContents(dir); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", name, got, err, want)
		}
	}
	check("empty", func(string) {}, ContentsEmpty)
	check("finder files", func(d string) { os.WriteFile(filepath.Join(d, ".DS_Store"), nil, 0o600) }, ContentsEmpty)
	check("other", func(d string) { os.WriteFile(filepath.Join(d, "notes.txt"), nil, 0o600) }, ContentsOther)
	check("registered", func(d string) { Init(d) }, ContentsRegistered)
	check("unregistered", func(d string) { os.Mkdir(filepath.Join(d, ".system"), 0o700) }, ContentsUnregistered)
	check("backup", func(d string) { InitBackupTarget(d) }, ContentsBackupTarget)
}

func TestProbeWritableNeverCreatesDir(t *testing.T) {
	dir := t.TempDir()
	if err := ProbeWritable(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe left %v", entries)
	}
	if err := ProbeWritable(filepath.Join(dir, "missing")); !errors.Is(err, ErrRootMissing) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("probe created the directory")
	}
}

func TestStatFS(t *testing.T) {
	info, err := StatFS(t.TempDir())
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		if !errors.Is(err, ErrInspectUnsupported) {
			t.Fatalf("got %v", err)
		}
		return
	}
	if err != nil || !info.Known || info.TotalBytes == 0 || info.FreeBytes > info.TotalBytes || info.ReadOnly {
		t.Fatalf("%+v %v", info, err)
	}
	if runtime.GOOS == "darwin" && info.Type == "" {
		t.Fatalf("no file system type: %+v", info)
	}
}

func TestListMounts(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"B", "A", ".hidden", "com.apple.TimeMachine.localsnapshots"} {
		os.Mkdir(filepath.Join(dir, n), 0o700)
	}
	os.WriteFile(filepath.Join(dir, "file"), nil, 0o600)
	os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling"))
	m, err := ListMounts(dir)
	if err != nil || len(m) != 2 || m[0].Name != "A" || m[1].Name != "B" || m[0].Mounted {
		t.Fatalf("%+v %v", m, err)
	}
}
