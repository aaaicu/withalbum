//go:build darwin || linux

// Command perf measures WithAlbum Server on large synthetic libraries
// (run it with scripts/perf-bench.sh).
//
// For each size it makes a throwaway library in -work: one real upload
// through the API as a template, then N-1 more assets copied from the
// template's database rows in one transaction, each with its own tiny
// original in the pool (random bytes, never real photos). Then it runs the
// real server binary and measures, with the time of each step and the peak
// RSS of the process that did it:
//
//   - server startup (startup reconciliation of every original),
//   - folder list, the folder's asset snapshot paged to the end, the
//     event feed from the beginning, one page deep in the snapshot,
//   - single uploads into the large folder,
//   - verify (sizes) and verify -hash.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/tools/internal/harness"
)

const (
	login    = "perf"
	password = "perf-test-password-not-secret"
)

var (
	serverBin = flag.String("server", "", "withalbum-server binary (required)")
	workDir   = flag.String("work", "", "scratch directory (required, must exist; needs a few GB for 500k originals)")
	sizes     = flag.String("n", "100000,500000", "comma-separated library sizes (assets in one folder)")
	report    = flag.String("report", "", "write the Markdown results to this file")
	pageLimit = flag.Int("limit", 500, "page size for snapshot and feed paging (server maximum 500)")
	keep      = flag.Bool("keep", false, "keep the synthetic libraries")
)

type row struct {
	step    string
	seconds float64
	rss     int64 // bytes; 0 = not measured separately
	detail  string
}

func main() {
	log.SetFlags(log.Ltime)
	flag.Parse()
	if *serverBin == "" || *workDir == "" {
		fmt.Fprintln(os.Stderr, "usage: perf -server BIN -work DIR [-n 100000,500000] [-report FILE]")
		os.Exit(2)
	}
	var ns []int
	for _, f := range strings.Split(*sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 2 {
			log.Fatalf("bad size %q", f)
		}
		ns = append(ns, n)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Measured %s, %s/%s, %d CPUs, Go %s.\n\n", time.Now().UTC().Format("2006-01-02"), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	for _, n := range ns {
		rows, err := measure(n)
		if err != nil {
			log.Fatalf("n=%d: %v", n, err)
		}
		fmt.Fprintf(&out, "### %d assets in one folder\n\n| step | time | peak RSS | detail |\n|---|---:|---:|---|\n", n)
		for _, r := range rows {
			rss := "—"
			if r.rss > 0 {
				rss = fmt.Sprintf("%.0f MiB", float64(r.rss)/(1<<20))
			}
			fmt.Fprintf(&out, "| %s | %s | %s | %s |\n", r.step, fmtDur(r.seconds), rss, r.detail)
		}
		fmt.Fprintln(&out)
	}
	fmt.Print(out.String())
	if *report != "" {
		if err := os.WriteFile(*report, []byte(out.String()), 0644); err != nil {
			log.Fatal(err)
		}
	}
}

func fmtDur(s float64) string {
	switch {
	case s == 0:
		return "—"
	case s < 0.001:
		return "<1 ms"
	case s < 1:
		return fmt.Sprintf("%.0f ms", s*1000)
	case s < 120:
		return fmt.Sprintf("%.1f s", s)
	}
	return fmt.Sprintf("%.1f min", s/60)
}

func since(t time.Time) float64 { return time.Since(t).Seconds() }

func measure(n int) ([]row, error) {
	dir := filepath.Join(*workDir, fmt.Sprintf("perf-%d", n))
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	if !*keep {
		defer os.RemoveAll(dir)
	}
	root := filepath.Join(dir, "library")
	if err := os.Mkdir(root, 0700); err != nil {
		return nil, err
	}
	listen, err := harness.FreePort()
	if err != nil {
		return nil, err
	}
	srv := &harness.Server{Bin: *serverBin, Root: root, Config: filepath.Join(dir, "config.json"), Listen: listen, Dir: dir,
		Env: []string{"WITHALBUM_MIN_FREE_BYTES=0"}}
	if code, o, _, err := srv.Command("", "-init-storage"); err != nil || code != 0 {
		return nil, fmt.Errorf("init-storage: %v %s", err, o)
	}
	if code, o, _, err := srv.Command(password+"\n", "admin", "init", "-login", login); err != nil || code != 0 {
		return nil, fmt.Errorf("admin init: %v %s", err, o)
	}
	if err := srv.Start(60 * time.Second); err != nil {
		return nil, err
	}
	c, err := srv.Login(login, password)
	if err != nil {
		srv.Kill()
		return nil, err
	}
	folder, err := c.CreateFolder("perf")
	if err != nil {
		srv.Kill()
		return nil, err
	}
	body, _ := harness.RandomBytes(1024)
	if r := c.Upload(folder, "perf-template", "template.bin", strings.NewReader(string(body)), int64(len(body))); r.Status != 201 {
		srv.Kill()
		return nil, fmt.Errorf("template upload: %d %s %v", r.Status, r.Code, r.Err)
	}
	// Let the background workers settle the template (capture time etc.).
	time.Sleep(2 * time.Second)
	srv.Stop()

	var rows []row
	t := time.Now()
	if err := synthesize(root, folder, n-1); err != nil {
		return nil, fmt.Errorf("synthesize: %w", err)
	}
	dbSize := fileSize(filepath.Join(root, ".system", "withalbum.db"))
	rows = append(rows, row{step: "synthesize (not the server)", seconds: since(t), detail: fmt.Sprintf("%d assets, database %.0f MiB", n, float64(dbSize)/(1<<20))})
	log.Printf("n=%d: synthesized in %.1fs", n, since(t))

	t = time.Now()
	if err := srv.Start(30 * time.Minute); err != nil {
		return nil, err
	}
	startup := since(t)
	recovery := ""
	if srv.WaitLine("recovery:", time.Second) {
		recovery = "startup reconciliation of every original"
	}
	rows = append(rows, row{step: "server start until /health answers", seconds: startup, detail: recovery})
	defer srv.Kill()
	if c, err = srv.Login(login, password); err != nil {
		return nil, err
	}

	t = time.Now()
	nf, err := c.ListFolders()
	if err != nil {
		return nil, err
	}
	rows = append(rows, row{step: "GET /folders", seconds: since(t), detail: fmt.Sprintf("%d folder", nf)})

	t = time.Now()
	assets, cursor, pages, err := c.ListAssets(folder, *pageLimit)
	if err != nil {
		return nil, err
	}
	if len(assets) != n {
		return nil, fmt.Errorf("snapshot has %d assets, want %d", len(assets), n)
	}
	snap := since(t)
	rows = append(rows, row{step: "snapshot GET /folders/{id}/assets, all pages", seconds: snap,
		detail: fmt.Sprintf("%d pages of %d, %.1f ms/page", pages, *pageLimit, snap*1000/float64(pages))})

	// One page deep in the snapshot (keyset paging: should not depend on depth).
	deep := assets[len(assets)*9/10].ID
	t = time.Now()
	const reps = 20
	for i := 0; i < reps; i++ {
		if _, err := pageAfter(c, folder, deep); err != nil {
			return nil, err
		}
	}
	rows = append(rows, row{step: "one snapshot page at 90% depth", seconds: since(t) / reps, detail: fmt.Sprintf("mean of %d", reps)})

	t = time.Now()
	events, epages, err := c.ListEvents(folder, "", *pageLimit)
	if err != nil {
		return nil, err
	}
	feed := since(t)
	rows = append(rows, row{step: "event feed GET /sync from the beginning, all pages", seconds: feed,
		detail: fmt.Sprintf("%d events, %d pages, %.1f ms/page", events, epages, feed*1000/float64(epages))})

	t = time.Now()
	if _, _, err := c.ListEvents(folder, cursor, *pageLimit); err != nil {
		return nil, err
	}
	rows = append(rows, row{step: "event feed from the snapshot cursor (up to date)", seconds: since(t)})

	var upl []float64
	for i := 0; i < 5; i++ {
		b, _ := harness.RandomBytes(64 << 10)
		t = time.Now()
		if r := c.Upload(folder, fmt.Sprintf("perf-new-%d", i), "new.bin", strings.NewReader(string(b)), int64(len(b))); r.Status != 201 {
			return nil, fmt.Errorf("upload: %d %s %v", r.Status, r.Code, r.Err)
		}
		upl = append(upl, since(t))
	}
	rows = append(rows, row{step: "upload 64 KiB into the large folder", seconds: mean(upl), detail: fmt.Sprintf("mean of %d (fsync included), max %s", len(upl), fmtDur(maxOf(upl)))})

	srv.Stop()
	rows = append(rows, row{step: "server process (all steps above)", rss: srv.MaxRSS(), detail: "peak RSS of the server"})

	for _, args := range [][]string{{"verify"}, {"verify", "-hash"}} {
		t = time.Now()
		code, o, ru, err := srv.Command("", append(args, "-json")...)
		if err != nil || code != 0 {
			return nil, fmt.Errorf("%v: exit %d %v %.300s", args, code, err, o)
		}
		var rep struct {
			Blobs  int `json:"blobs"`
			Hashed int `json:"hashed"`
		}
		_ = json.Unmarshal([]byte(o), &rep)
		rows = append(rows, row{step: strings.Join(args, " "), seconds: since(t), rss: harness.RSSBytes(ru),
			detail: fmt.Sprintf("%d originals, %d hashed", rep.Blobs, rep.Hashed)})
	}
	return rows, nil
}

// pageAfter fetches one snapshot page after the given asset id.
func pageAfter(c *harness.Client, folder, after string) (int, error) {
	var page struct {
		Assets []json.RawMessage `json:"assets"`
	}
	st, raw, err := c.GetJSON("/api/v1/folders/"+folder+"/assets?limit="+strconv.Itoa(*pageLimit)+"&after="+after, &page)
	if err != nil {
		return 0, err
	}
	if st != 200 || len(page.Assets) == 0 {
		return 0, fmt.Errorf("page: %d %.200s", st, raw)
	}
	return len(page.Assets), nil
}

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// synthesize copies the template asset (the only one in folder) count
// times: blobs, assets, folder links and asset_added events, plus one tiny
// pool file per blob, so that every check of the server and of verify has
// real work to do.
func synthesize(root, folder string, count int) error {
	ctx := context.Background()
	sqlDB, err := db.Open(filepath.Join(root, ".system", "withalbum.db"))
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	var tmplAsset, tmplSHA string
	if err := sqlDB.QueryRowContext(ctx, `SELECT a.asset_id, a.original_sha256 FROM folder_assets fa JOIN assets a ON a.asset_id = fa.asset_id WHERE fa.folder_id = ?`, folder).Scan(&tmplAsset, &tmplSHA); err != nil {
		return fmt.Errorf("template: %w", err)
	}
	var lastSeq int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT last_seq FROM folder_sequences WHERE folder_id = ?`, folder).Scan(&lastSeq); err != nil {
		return err
	}
	cols := func(table string) ([]string, error) {
		rows, err := sqlDB.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}
	// INSERT INTO t (cols) SELECT cols-with-overrides FROM t WHERE key = template.
	copyStmt := func(tx *sql.Tx, table, where string, override map[string]string) (*sql.Stmt, []string, error) {
		cs, err := cols(table)
		if err != nil {
			return nil, nil, err
		}
		var sel []string
		var params []string
		for _, c := range cs {
			if _, ok := override[c]; ok {
				sel = append(sel, "?")
				params = append(params, c)
			} else {
				sel = append(sel, c)
			}
		}
		q := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s", table, strings.Join(cs, ","), strings.Join(sel, ","), table, where)
		st, err := tx.PrepareContext(ctx, q)
		return st, params, err
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	blobSt, blobP, err := copyStmt(tx, "blobs", "sha256 = '"+tmplSHA+"'", map[string]string{"sha256": "", "bytes": "", "relative_path": ""})
	if err != nil {
		return err
	}
	assetSt, assetP, err := copyStmt(tx, "assets", "asset_id = '"+tmplAsset+"'", map[string]string{"asset_id": "", "original_sha256": "", "original_filename": ""})
	if err != nil {
		return err
	}
	linkSt, linkP, err := copyStmt(tx, "folder_assets", "folder_id = '"+folder+"' AND asset_id = '"+tmplAsset+"'", map[string]string{"asset_id": ""})
	if err != nil {
		return err
	}
	eventSt, eventP, err := copyStmt(tx, "folder_changes", "folder_id = '"+folder+"' AND seq = "+strconv.FormatInt(lastSeq, 10), map[string]string{"seq": "", "asset_id": ""})
	if err != nil {
		return err
	}
	var tmplRel string
	if err := tx.QueryRowContext(ctx, "SELECT relative_path FROM blobs WHERE sha256 = ?", tmplSHA).Scan(&tmplRel); err != nil {
		return err
	}
	relPrefix := strings.TrimSuffix(tmplRel, tmplSHA[:2]+"/"+tmplSHA)
	pool := filepath.Join(root, ".system", "pool")
	buf := make([]byte, 40)
	for i := 1; i <= count; i++ {
		binary.LittleEndian.PutUint64(buf, uint64(i))
		copy(buf[8:], tmplSHA[:32])
		size := 24 + i%16
		content := buf[:size]
		sum := sha256.Sum256(content)
		sha := hex.EncodeToString(sum[:])
		idSum := sha256.Sum256([]byte("asset" + strconv.Itoa(i)))
		id := hex.EncodeToString(idSum[:16])
		vals := map[string]any{"sha256": sha, "bytes": size, "relative_path": relPrefix + sha[:2] + "/" + sha,
			"asset_id": id, "original_sha256": sha, "original_filename": fmt.Sprintf("IMG_%06d.bin", i), "seq": lastSeq + int64(i)}
		args := func(names []string) []any {
			out := make([]any, len(names))
			for k, n := range names {
				out[k] = vals[n]
			}
			return out
		}
		if _, err := blobSt.ExecContext(ctx, args(blobP)...); err != nil {
			return fmt.Errorf("blob %d: %w", i, err)
		}
		if _, err := assetSt.ExecContext(ctx, args(assetP)...); err != nil {
			return fmt.Errorf("asset %d: %w", i, err)
		}
		if _, err := linkSt.ExecContext(ctx, args(linkP)...); err != nil {
			return fmt.Errorf("link %d: %w", i, err)
		}
		if _, err := eventSt.ExecContext(ctx, args(eventP)...); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		p := filepath.Join(pool, sha[:2], sha)
		if err := os.WriteFile(p, content, 0400); err != nil {
			return err
		}
		if i%100000 == 0 {
			log.Printf("  %d/%d", i, count)
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE folder_sequences SET last_seq = ? WHERE folder_id = ?", lastSeq+int64(count), folder); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = sqlDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}
