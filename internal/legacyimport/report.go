package legacyimport

import (
	"fmt"
	"io"
	"time"
)

// Report describes one Run. It is printed to the operator and, on apply,
// stored in legacy_imports.report_json.
type Report struct {
	Mode   string `json:"mode"`   // "dry-run" or "apply"
	Result string `json:"result"` // "ok" or "failed"
	// Resumed is true when apply found a database that already records this
	// state.json and only finished the last step.
	Resumed bool `json:"resumed,omitempty"`

	SourceSHA256 string `json:"source_sha256,omitempty"`
	SourceBytes  int64  `json:"source_bytes,omitempty"`

	Folders       int     `json:"folders"`
	Assets        int     `json:"assets"`
	Links         int     `json:"links"`
	Events        int     `json:"events"`
	OriginalBytes int64   `json:"original_bytes"`
	PreviewsReady int     `json:"previews_ready"`
	HashSeconds   float64 `json:"hash_seconds"`

	FolderSummaries []FolderSummary `json:"folder_summaries"`

	// Paths relative to the storage root.
	BackupPath   string `json:"backup_path,omitempty"`
	DatabasePath string `json:"database_path,omitempty"`
	MigratedPath string `json:"migrated_path,omitempty"`

	Warnings []string `json:"warnings"`
	Problems []string `json:"problems"`
	Error    string   `json:"error,omitempty"`

	parsed bool // state.json was parsed, so the counts above are meaningful
}

// FolderSummary is one folder as found in state.json.
type FolderSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Assets  int    `json:"assets"`
	Events  int    `json:"events"`
	LastSeq int64  `json:"last_seq"`
}

// WriteSummary prints a human-readable summary of r.
func (r *Report) WriteSummary(w io.Writer) {
	fmt.Fprintf(w, "WithAlbum legacy import (%s): %s\n", r.Mode, r.Result)
	if r.SourceSHA256 != "" {
		fmt.Fprintf(w, "  source:   .system/state.json (%d bytes, sha256 %s)\n", r.SourceBytes, r.SourceSHA256)
	}
	if r.parsed {
		fmt.Fprintf(w, "  folders %d, assets %d, folder links %d, events %d, originals %s, previews %d\n",
			r.Folders, r.Assets, r.Links, r.Events, humanBytes(r.OriginalBytes), r.PreviewsReady)
		for _, f := range r.FolderSummaries {
			fmt.Fprintf(w, "    folder %s %q: %d assets, %d events, last seq %d\n", f.ID, f.Name, f.Assets, f.Events, f.LastSeq)
		}
	}
	if r.parsed && r.Assets > 0 {
		d := time.Duration(r.HashSeconds * float64(time.Second)).Round(time.Millisecond)
		fmt.Fprintf(w, "  re-hashing originals took %s", d)
		if r.Mode == "dry-run" {
			fmt.Fprint(w, " (apply re-hashes them again, so expect at least that long)")
		}
		fmt.Fprintln(w)
	}
	if r.Resumed {
		fmt.Fprintln(w, "  resumed: the database already recorded this import; finished the last step")
	}
	if r.BackupPath != "" {
		fmt.Fprintf(w, "  backup:   %s\n", r.BackupPath)
	}
	if r.DatabasePath != "" {
		fmt.Fprintf(w, "  database: %s\n", r.DatabasePath)
	}
	if r.MigratedPath != "" {
		fmt.Fprintf(w, "  state.json renamed to %s (kept, not deleted)\n", r.MigratedPath)
	}
	const maxPrinted = 50
	printList := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(w, "  %s (%d):\n", title, len(items))
		for i, s := range items {
			if i == maxPrinted {
				fmt.Fprintf(w, "    ... and %d more (see -json)\n", len(items)-maxPrinted)
				break
			}
			fmt.Fprintf(w, "    - %s\n", s)
		}
	}
	printList("warnings", r.Warnings)
	printList("problems", r.Problems)
	if r.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", r.Error)
	}
	if r.Mode == "dry-run" && r.Result == "ok" {
		fmt.Fprintln(w, "  nothing was written. Run again with -apply to import.")
	}
}
