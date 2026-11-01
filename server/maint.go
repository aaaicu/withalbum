package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aaaicu/withalbum/internal/backup"
	"github.com/aaaicu/withalbum/internal/integrity"
	"github.com/aaaicu/withalbum/internal/storage"
)

// Maintenance commands: verify, backup and restore. See docs/BACKUP_RESTORE.md.
//
// Lock policy:
//
//   - verify and backup only read the storage, so they run while the server
//     is running and do not take the instance lock (backup takes the lock of
//     its backup target instead).
//   - restore replaces .system and takes the instance lock: stop the server
//     first. It refuses to run otherwise.

// Exit codes of verify.
const (
	exitOK       = 0
	exitProblems = 1 // verify: problems found; others: failed
	exitUsage    = 2
	exitNoCheck  = 3 // verify: the check itself could not run
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// openSource opens WITHALBUM_ROOT for reading by verify and backup.
func openSource(cmd string, stderr io.Writer) (*storage.Volume, bool) {
	root, err := storageRoot()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return nil, false
	}
	vol, err := storage.Open(root, storage.OpenOptions{ExpectedVolumeID: expectedVolumeID(), UnreadableDiskOK: containerSetting(os.Getenv)})
	if err != nil {
		fmt.Fprintf(stderr, "%s: STORAGE: %s (%v)\n", cmd, openStorageHint(root, err), err)
		return nil, false
	}
	return vol, true
}

// runVerify implements "withalbum-server verify [-hash] [-json]".
func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hash := fs.Bool("hash", false, "Re-read every original and compare its SHA-256 (reads the whole library)")
	asJSON := fs.Bool("json", false, "Print the report as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: withalbum-server verify [-hash] [-json]")
		fmt.Fprintln(stderr, "Checks the database (integrity_check, foreign keys) and that every original in WITHALBUM_ROOT is present with the right size,")
		fmt.Fprintln(stderr, "and with -hash the right content. Changes nothing; safe while the server runs.")
		fmt.Fprintln(stderr, "Exit status: 0 healthy, 1 problems found, 3 the check could not run.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return exitUsage
	}
	vol, ok := openSource("verify", stderr)
	if !ok {
		return exitNoCheck
	}
	sqlDB, err := integrity.OpenDB(vol)
	if err != nil {
		fmt.Fprintf(stderr, "verify: %v\n", err)
		return exitNoCheck
	}
	defer sqlDB.Close()
	var progress io.Writer
	if !*asJSON {
		progress = stderr
	}
	report, err := integrity.Run(context.Background(), vol, sqlDB, integrity.Options{Hash: *hash, Progress: progress})
	if err != nil {
		fmt.Fprintf(stderr, "verify: could not complete the check: %v\n", err)
		return exitNoCheck
	}
	if *asJSON {
		if err := printJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "verify: %v\n", err)
			return exitNoCheck
		}
	} else {
		report.WriteSummary(stdout)
	}
	if !report.Healthy() {
		return exitProblems
	}
	return exitOK
}

// runBackup implements "withalbum-server backup -to DIR [-init] [-rehash] [-json]".
func runBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "Backup target directory, registered once with -init (required)")
	initTarget := fs.Bool("init", false, "Register the existing -to directory as a backup target and exit")
	rehash := fs.Bool("rehash", false, "Re-hash originals already in the target; replace damaged copies (kept in damaged/)")
	keep := fs.Int("keep", 0, "After a complete snapshot, remove the oldest snapshots so that this many remain (0: keep all). Shared originals stay")
	asJSON := fs.Bool("json", false, "Print the report as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: withalbum-server backup -to DIR [-init] [-rehash] [-keep N] [-json]")
		fmt.Fprintln(stderr, "Exit status: 0 complete, 1 failed (or complete but the library has missing/damaged originals), 2 usage.")
		fmt.Fprintln(stderr, "Writes a consistent snapshot of WITHALBUM_ROOT (database + originals + previews, with a SHA-256 manifest)")
		fmt.Fprintln(stderr, "to DIR/snapshots/<id>. Incremental: originals already in DIR are not copied again. Safe while the server runs.")
		fmt.Fprintln(stderr, "The backup holds password and session hashes and every photo: keep DIR private (it is created 0700/0600).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 || *to == "" || *keep < 0 {
		fs.Usage()
		return exitUsage
	}
	if *initTarget {
		m, err := storage.InitBackupTarget(*to)
		if err != nil {
			if errors.Is(err, storage.ErrRootMissing) {
				fmt.Fprintf(stderr, "backup: %s does not exist. Create it on the backup disk first; it is never created automatically.\n", *to)
				return exitProblems
			}
			fmt.Fprintf(stderr, "backup: cannot register %s: %v\n", *to, err)
			return exitProblems
		}
		// It holds password hashes and every photo: owner only.
		if err := os.Chmod(*to, 0700); err != nil { // #nosec G302 -- a directory: owner rwx only
			fmt.Fprintf(stderr, "backup: WARNING: could not restrict %s to its owner (chmod 0700): %v\n", *to, err)
		}
		fmt.Fprintf(stdout, "Registered backup target %s (target_id=%s, mode 0700)\n", *to, m.VolumeID)
		return exitOK
	}
	vol, ok := openSource("backup", stderr)
	if !ok {
		return exitProblems
	}
	sqlDB, err := integrity.OpenDB(vol)
	if err != nil {
		fmt.Fprintf(stderr, "backup: %v\n", err)
		return exitProblems
	}
	defer sqlDB.Close()
	var progress io.Writer
	if !*asJSON {
		progress = stderr
	}
	report, err := backup.Backup(context.Background(), vol, sqlDB, *to, backup.Options{Rehash: *rehash, ServerVersion: version, Progress: progress, Keep: *keep})
	if report != nil && report.SameDevice {
		fmt.Fprintln(stderr, "backup: WARNING: the backup target is on the same disk as the storage root. It does not protect against a failed or lost disk; use a separate disk.")
	}
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrNotInitialized):
			fmt.Fprintf(stderr, "backup: %s is not a registered backup target. If it is the right disk, run: withalbum-server backup -init -to %s\n", *to, *to)
		case errors.Is(err, storage.ErrRootMissing):
			fmt.Fprintf(stderr, "backup: %s does not exist. Is the backup disk mounted?\n", *to)
		case errors.Is(err, storage.ErrLocked):
			fmt.Fprintf(stderr, "backup: another backup is writing to %s.\n", *to)
		}
		fmt.Fprintf(stderr, "backup: FAILED: %v\n", err)
		fmt.Fprintln(stderr, "backup: no snapshot was finalized; earlier snapshots in the target are unchanged.")
		return exitProblems
	}
	if report.PruneError != "" {
		fmt.Fprintf(stderr, "backup: WARNING: the new snapshot is complete, but removing old snapshots (-keep %d) failed: %s. The remaining snapshots are intact; the next backup tries again.\n", *keep, report.PruneError)
	}
	if *asJSON {
		if err := printJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "backup: %v\n", err)
			return exitProblems
		}
		if len(report.SourceWarnings) > 0 || report.PruneError != "" {
			return exitProblems
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "backup: OK snapshot %s\n  originals: %d (%d bytes), copied %d (%d bytes), already in target %d, damaged copies replaced %d\n  previews: %d\n",
		report.Path, report.Blobs, report.BlobBytes, report.Copied, report.CopiedBytes, report.Reused, report.Replaced, report.Previews)
	if len(report.PreviewsLost) > 0 {
		fmt.Fprintf(stdout, "  warning: %d preview(s) marked ready were missing in the source and are not in the backup\n", len(report.PreviewsLost))
	}
	if len(report.Pruned) > 0 {
		fmt.Fprintf(stdout, "  removed %d old snapshot(s) to keep %d: %s\n", len(report.Pruned), *keep, strings.Join(report.Pruned, ", "))
	}
	fmt.Fprintf(stdout, "  restore with: withalbum-server restore -from %s\n", report.Path)
	if len(report.SourceWarnings) > 0 {
		fmt.Fprintf(stderr, "backup: WARNING: the snapshot is complete (the target already held good copies), but %d original(s) are missing or damaged in the library itself, e.g. %s. Run withalbum-server verify and restore them (docs/BACKUP_RESTORE.md).\n",
			len(report.SourceWarnings), report.SourceWarnings[0])
		return exitProblems
	}
	if report.PruneError != "" {
		return exitProblems
	}
	return exitOK
}

// runRestore implements "withalbum-server restore -from SNAPSHOT [-check] [-replace] [-json]".
func runRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "Snapshot directory: <backup target>/snapshots/<id> (required)")
	check := fs.Bool("check", false, "Only verify the snapshot completely (every original hashed); change nothing")
	replace := fs.Bool("replace", false, "Restore over an existing library; it is kept as .system.before-restore-<UTC>, never deleted")
	asJSON := fs.Bool("json", false, "Print the report as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: withalbum-server restore -from SNAPSHOT [-check] [-replace] [-json]")
		fmt.Fprintln(stderr, "Verifies the snapshot and restores it into WITHALBUM_ROOT (an initialized volume, empty unless -replace).")
		fmt.Fprintln(stderr, "Stop the server first. Every device must sign in again afterwards. An interrupted restore is finished by running it again.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 || *from == "" {
		fs.Usage()
		return exitUsage
	}
	root := ""
	if !*check {
		var err error
		if root, err = storageRoot(); err != nil {
			fmt.Fprintf(stderr, "restore: %v\n", err)
			return exitProblems
		}
	}
	var progress io.Writer
	if !*asJSON {
		progress = stderr
	}
	report, err := backup.Restore(context.Background(), *from, root, backup.RestoreOptions{CheckOnly: *check, Replace: *replace,
		ExpectedVolumeID: expectedVolumeID(), Progress: progress})
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrLocked):
			fmt.Fprintf(stderr, "restore: %s\n", lockHint(root, err))
		case errors.Is(err, storage.ErrNotInitialized), errors.Is(err, storage.ErrRootMissing), errors.Is(err, storage.ErrVolumeMismatch):
			if root != "" {
				fmt.Fprintf(stderr, "restore: STORAGE: %s\n", openStorageHint(root, err))
			}
		case errors.Is(err, backup.ErrTargetNotEmpty):
			fmt.Fprintln(stderr, "restore: nothing was changed.")
		case errors.Is(err, backup.ErrBackupDamaged):
			fmt.Fprintln(stderr, "restore: the snapshot failed verification; nothing was activated. Try an older snapshot.")
		}
		fmt.Fprintf(stderr, "restore: FAILED: %v\n", err)
		return exitProblems
	}
	if *asJSON {
		if err := printJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "restore: %v\n", err)
			return exitProblems
		}
		return exitOK
	}
	switch {
	case report.CheckOnly:
		fmt.Fprintf(stdout, "restore -check: OK snapshot %s (%s): %d originals (%d bytes) and %d previews verified\n",
			report.BackupID, report.CreatedAt, report.Blobs, report.BlobBytes, report.Previews)
		if len(report.PreviewsSkipped) > 0 {
			fmt.Fprintf(stdout, "  warning: %d preview(s) are damaged and would not be restored\n", len(report.PreviewsSkipped))
		}
	case report.Resumed:
		fmt.Fprintf(stdout, "restore: finished the interrupted restore of snapshot %s. Run restore again only if you want another snapshot.\n", report.BackupID)
	default:
		fmt.Fprintf(stdout, "restore: OK snapshot %s (%s) restored into %s\n  originals: %d (%d bytes), previews: %d, sessions revoked: %d (every device must sign in again)\n",
			report.BackupID, report.CreatedAt, root, report.Blobs, report.BlobBytes, report.Previews, report.SessionsRevoked)
	}
	if report.PreservedPath != "" {
		fmt.Fprintf(stdout, "  the previous data was kept in %s (delete it yourself once the restored library is confirmed)\n", report.PreservedPath)
	}
	return exitOK
}
