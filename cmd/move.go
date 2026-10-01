package cmd

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yhu/fs-hash-util/internal/db"
	"github.com/yhu/fs-hash-util/internal/hasher"
)

var moveCmd = &cobra.Command{
	Use:   "move <src> <dst>",
	Short: "Move file(s)/folder(s) and update the database (no hash recalculation)",
	Long: `Move <src> to <dst> and patch all matching database records.
<src> may be a glob pattern (e.g. 'photos/*.jpg', 'dir/file*').
Hashes are preserved — no files are re-read unless a conflict must be resolved.

If <dst> is an existing directory, each matched source is moved inside it.
When <src> matches multiple items, <dst> must be (or become) a directory.

Conflict resolution (per file whose destination path already exists):
  - same content hash   → the destination is kept and the source is deleted.
  - different hash       → the source is moved alongside the destination under
                           a content-tagged name: <stem>-<sha256>.<ext>
                           (e.g. README-6fee…e49d.md).
Hashes come from the database when available; otherwise they are computed at
runtime and written back to the database.

Certain directories are moved as an indivisible unit, never merged file-by-file:
.git, node_modules, .svn, .hg, __pycache__, .venv, target, dist, .idea, .vscode
(the directory entries of the default scan excludes). If the destination already
has one: identical content drops the source; different content moves the source
to the first free '<name>-N' (N=1,2,…) beside it.

After the move, folder hashes for every affected scan root are recomputed so the
database stays consistent with the filesystem: ancestors on both the source and
destination side are refreshed, and folders that became empty are removed.`,
	Args: cobra.ExactArgs(2),
	RunE: runMove,
}

func init() {
	rootCmd.AddCommand(moveCmd)
}

// moveResult accumulates per-run outcome counts for reporting.
type moveResult struct {
	moved   int64 // files moved to a free destination
	renamed int64 // files moved under a content-tagged name (hash conflict)
	deduped int64 // source deleted because destination had identical content
	failed  int   // items that could not be moved
}

func runMove(_ *cobra.Command, args []string) error {
	srcs, err := expandSrc(args[0])
	if err != nil {
		return err
	}

	dstBase, err := filepath.Abs(args[1])
	if err != nil {
		return fmt.Errorf("resolve dst: %w", err)
	}
	dstBase = filepath.Clean(dstBase)

	if len(srcs) > 1 {
		if mkErr := os.MkdirAll(dstBase, 0755); mkErr != nil {
			return fmt.Errorf("create dst directory: %w", mkErr)
		}
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	roots, err := database.AllScanRoots()
	if err != nil {
		return fmt.Errorf("query scan roots: %w", err)
	}

	var res moveResult
	// Scan roots whose folder-hash records must be recomputed after the move:
	// the source side (folders lost files) and the destination side (folders gained files).
	affected := map[string]struct{}{}

	for _, srcAbs := range srcs {
		// mv semantics: if dst is an existing directory, move src inside it.
		dstAbs := dstBase
		if info, statErr := os.Lstat(dstAbs); statErr == nil && info.IsDir() {
			dstAbs = filepath.Join(dstAbs, filepath.Base(srcAbs))
		}

		if r, _ := matchScanRoot(roots, srcAbs); r != "" {
			affected[r] = struct{}{}
		}
		if r, _ := matchScanRoot(roots, dstAbs); r != "" {
			affected[r] = struct{}{}
		}

		if moveErr := moveSingleItem(srcAbs, dstAbs, database, roots, &res); moveErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", moveErr)
		}
	}

	// Recompute folder hashes so the DB matches the filesystem after the merge.
	// Folders that lost all files are deleted; ancestors on both sides are refreshed.
	for root := range affected {
		if err := recomputeFolderHashes(database, root); err != nil {
			fmt.Fprintf(os.Stderr, "warning: folder-hash recompute failed for %s: %v\n", root, err)
		}
	}

	total := res.moved + res.renamed + res.deduped
	if total == 0 && res.failed == 0 {
		fmt.Fprintln(os.Stderr, "warning: nothing to move")
	} else {
		fmt.Printf("done: %d moved, %d renamed (hash conflict), %d deduped (source deleted)\n",
			res.moved, res.renamed, res.deduped)
	}
	if res.failed > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d item(s) could not be moved\n", res.failed)
	}
	return nil
}

// recomputeFolderHashes rebuilds all folder-hash records for scanRoot from the current
// file records, so the folders table stays consistent with the filesystem after a move.
//
// It uses the same aggregation as `scan` (aggregateFolderHashes): a folder hash is the
// SHA-256 of its sorted direct-children content hashes. Folders that no longer contain
// any files are deleted (matching a fresh scan, which omits empty folders). File content
// hashes are unchanged by a move, so no file is re-read.
//
// scan_id handling: an existing folder keeps its own scan_id; a newly-created folder
// inherits the scan_id of an existing folder/file under the root (a move is not a scan,
// and the column is an FK into scans — we never invent an id).
func recomputeFolderHashes(database *db.DB, scanRoot string) error {
	files, err := database.FilesUnderPrefix(scanRoot, ".")
	if err != nil {
		return fmt.Errorf("list files: %w", err)
	}

	dirs, folderHashes := aggregateFolderHashes(files)

	// Current folder records, to know their scan_id and which ones vanished.
	existing, err := database.AllFolders(scanRoot)
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}
	existingScanID := make(map[string]int64, len(existing))
	for _, f := range existing {
		existingScanID[f.RelPath] = f.ScanID
	}

	// Fallback scan_id for folders with no prior record: any existing folder or file.
	var fallbackScanID int64
	hasFallback := false
	for _, f := range existing {
		fallbackScanID, hasFallback = f.ScanID, true
		break
	}
	if !hasFallback {
		for _, f := range files {
			fallbackScanID, hasFallback = f.ScanID, true
			break
		}
	}

	want := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		want[d] = struct{}{}
	}

	tx, err := database.Begin()
	if err != nil {
		return err
	}

	// Upsert every folder that currently contains files.
	for _, dir := range dirs {
		scanID, ok := existingScanID[dir]
		if !ok {
			if !hasFallback {
				continue // nothing to anchor a scan_id to; skip (should not happen when files exist)
			}
			scanID = fallbackScanID
		}
		if err := database.UpsertFolder(tx, db.FolderRecord{
			ScanRoot: scanRoot, RelPath: dir, Hash: folderHashes[dir], ScanID: scanID,
		}); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert folder %q: %w", dir, err)
		}
	}

	// Delete folder records that no longer contain any files (emptied by the move).
	for _, f := range existing {
		if _, keep := want[f.RelPath]; keep {
			continue
		}
		if err := database.DeleteFolderByRelPathTx(tx, scanRoot, f.RelPath); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete folder %q: %w", f.RelPath, err)
		}
	}

	return tx.Commit()
}

// moveSingleItem moves srcAbs to dstAbs, resolving per-file conflicts, and patches the DB.
// For a directory source it walks the tree and resolves each regular file independently,
// recreating empty subdirectories at the destination.
func moveSingleItem(srcAbs, dstAbs string, database *db.DB, roots []string, res *moveResult) error {
	srcInfo, srcErr := os.Lstat(srcAbs)
	if srcErr != nil {
		res.failed++
		return fmt.Errorf("stat %s: %w", srcAbs, srcErr)
	}

	if !srcInfo.IsDir() {
		return resolveAndMove(srcAbs, dstAbs, database, roots, res)
	}

	// An atomic directory (.git, node_modules, …) is moved as an indivisible unit
	// (see resolveAtomicDir), even when it is the source itself — never walked file-by-file.
	if atomicDirNames[filepath.Base(srcAbs)] {
		return resolveAtomicDir(srcAbs, dstAbs, res)
	}

	// Directory: merge file-by-file into dstAbs.
	if mkErr := os.MkdirAll(dstAbs, 0755); mkErr != nil {
		res.failed++
		return fmt.Errorf("create destination directory: %w", mkErr)
	}

	walkErr := filepath.WalkDir(srcAbs, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			res.failed++
			fmt.Fprintf(os.Stderr, "  skip %s: %v\n", p, werr)
			return nil
		}
		if p == srcAbs {
			return nil
		}
		rel, _ := filepath.Rel(srcAbs, p)
		dst := filepath.Join(dstAbs, rel)
		if d.IsDir() {
			// An atomic directory (.git, node_modules, …) is handled as a unit; don't descend.
			if atomicDirNames[d.Name()] {
				resolveAtomicDir(p, dst, res)
				return filepath.SkipDir
			}
			mode := fs.FileMode(0755)
			if info, iErr := d.Info(); iErr == nil {
				mode = info.Mode().Perm()
			}
			_ = os.MkdirAll(dst, mode) // recreate (possibly empty) dir at destination
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if rErr := resolveAndMove(p, dst, database, roots, res); rErr != nil {
			res.failed++
			fmt.Fprintf(os.Stderr, "  skip %s: %v\n", p, rErr)
		}
		return nil
	})

	cleanupEmptyDirs(srcAbs)
	return walkErr
}

// atomicDirNames are directory names moved as an indivisible unit rather than merged
// file-by-file. This mirrors the *directory* entries of scanner.DefaultExcludes (the
// file entries .DS_Store / Thumbs.db are not directories and stay on the per-file path).
// Keep this in sync with scanner.DefaultExcludes in internal/scanner/exclude.go.
var atomicDirNames = map[string]bool{
	".git":         true,
	"node_modules": true,
	".svn":         true,
	".hg":          true,
	"__pycache__":  true,
	".venv":        true,
	"target":       true,
	"dist":         true,
	".idea":        true,
	".vscode":      true,
}

// resolveAtomicDir moves an atomic directory (.git, node_modules, …) as a single unit,
// resolving a destination conflict by the directory's whole-content hash (never descending
// file-by-file):
//   - destination free              → move the tree across unchanged.
//   - destination exists, same hash → delete source (identical tree already there).
//   - destination exists, diff hash → move source to the first free "<name>-N" (N=1,2,…).
//
// These directories are never tracked in the database (they are default scan excludes), so
// no DB records are touched here.
func resolveAtomicDir(srcDir, dstDir string, res *moveResult) error {
	label := filepath.Base(srcDir)
	dstInfo, dstErr := os.Lstat(dstDir)
	if dstErr != nil {
		// No conflict: move the whole tree to the destination path.
		if err := moveDirTree(srcDir, dstDir); err != nil {
			res.failed++
			fmt.Fprintf(os.Stderr, "  skip %s: %v\n", srcDir, err)
			return err
		}
		res.moved++
		fmt.Printf("moved  %s\n    →  %s (%s, atomic)\n", srcDir, dstDir, label)
		return nil
	}

	if dstInfo.IsDir() {
		srcH, sErr := dirContentHash(srcDir)
		dstH, dErr := dirContentHash(dstDir)
		if sErr == nil && dErr == nil && srcH == dstH {
			// Identical tree already at destination: drop the source.
			if err := os.RemoveAll(srcDir); err != nil {
				res.failed++
				fmt.Fprintf(os.Stderr, "  skip %s: %v\n", srcDir, err)
				return err
			}
			res.deduped++
			fmt.Printf("dedupe %s (identical %s already at %s)\n", srcDir, label, dstDir)
			return nil
		}
		// On hash error, fall through to the suffixed-move path (safe default: keep both).
	}

	// Different content (or target is not a directory): move source to the first free <name>-N.
	target := nextFreeSuffixed(dstDir)
	if err := moveDirTree(srcDir, target); err != nil {
		res.failed++
		fmt.Fprintf(os.Stderr, "  skip %s: %v\n", srcDir, err)
		return err
	}
	res.renamed++
	fmt.Printf("moved  %s\n    →  %s (%s conflict)\n", srcDir, target, label)
	return nil
}

// moveDirTree moves an entire directory tree src → dst. It tries os.Rename first
// (atomic, same-device, avoids per-file permission issues); on failure (e.g. cross-device)
// it falls back to a recursive copy that preserves permissions, then removes the source.
func moveDirTree(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create destination parent: %w", err)
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Cross-device (or other rename failure): recursive copy, then delete source.
	walkErr := filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			mode := fs.FileMode(0755)
			if info, iErr := d.Info(); iErr == nil {
				mode = info.Mode().Perm()
			}
			return os.MkdirAll(target, mode)
		}
		if !d.Type().IsRegular() {
			return nil // skip non-regular entries (symlinks, etc.)
		}
		return copyFile(p, target)
	})
	if walkErr != nil {
		return fmt.Errorf("copy tree %s: %w", src, walkErr)
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("remove source tree %s: %w", src, err)
	}
	return nil
}

// dirContentHash returns a content-only hash of a directory tree, computed with the same
// aggregation as scan/move folder hashes (aggregateFolderHashes) so that two trees are
// judged equal iff their file contents and layout match. File names of the root are not
// part of the hash. An empty tree (no regular files) hashes to EmptyFileHash.
func dirContentHash(dirAbs string) (string, error) {
	var recs []db.FileRecord
	walkErr := filepath.WalkDir(dirAbs, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := hasher.HashFile(p, info.Size())
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dirAbs, p)
		if err != nil {
			return err
		}
		recs = append(recs, db.FileRecord{RelPath: filepath.ToSlash(rel), Hash: h})
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if len(recs) == 0 {
		return hasher.EmptyFileHash, nil
	}
	_, hashes := aggregateFolderHashes(recs)
	return hashes["."], nil
}

// nextFreeSuffixed returns base with the first "-N" suffix (N=1,2,3,…) that does not exist.
// Deterministic: no timestamps or randomness.
func nextFreeSuffixed(base string) string {
	for n := 1; ; n++ {
		cand := fmt.Sprintf("%s-%d", base, n)
		if _, err := os.Lstat(cand); err != nil {
			return cand
		}
	}
}

// resolveAndMove moves one regular file srcAbs to dstAbs, resolving a destination
// conflict by content hash:
//   - destination free            → move, update DB.
//   - destination exists, same hash → delete source + its DB record (dedupe).
//   - destination exists, diff hash → move source to <stem>-<hash>.<ext>, update DB.
//
// A destination that is a directory (type mismatch) is treated as "different".
func resolveAndMove(srcAbs, dstAbs string, database *db.DB, roots []string, res *moveResult) error {
	dstInfo, dstErr := os.Lstat(dstAbs)
	if dstErr != nil {
		// No conflict: destination path is free.
		if err := doMove(srcAbs, dstAbs); err != nil {
			return err
		}
		updateDBMove(database, roots, srcAbs, dstAbs)
		res.moved++
		fmt.Printf("moved  %s\n    →  %s\n", srcAbs, dstAbs)
		return nil
	}

	srcHash, err := resolveHash(database, roots, srcAbs)
	if err != nil {
		return fmt.Errorf("hash %s: %w", srcAbs, err)
	}

	sameContent := false
	if dstInfo.Mode().IsRegular() {
		dstHash, dErr := resolveHash(database, roots, dstAbs)
		if dErr != nil {
			return fmt.Errorf("hash %s: %w", dstAbs, dErr)
		}
		sameContent = srcHash == dstHash
	}
	// If dst is a directory, sameContent stays false → treated as different.

	if sameContent {
		// Identical content already at destination: drop the source.
		if err := os.Remove(srcAbs); err != nil {
			return fmt.Errorf("remove duplicate source %s: %w", srcAbs, err)
		}
		deleteDBRecord(database, roots, srcAbs)
		res.deduped++
		fmt.Printf("dedupe %s (identical content already at %s)\n", srcAbs, dstAbs)
		return nil
	}

	// Different content: move source under a content-tagged name.
	renamed := hashSuffixName(filepath.Base(dstAbs), srcHash)
	renamedDst := filepath.Join(filepath.Dir(dstAbs), renamed)

	if ri, riErr := os.Lstat(renamedDst); riErr == nil {
		// The tagged name is content-derived; an existing one means identical
		// content is already there. Idempotent: drop the source.
		if ri.Mode().IsRegular() {
			if err := os.Remove(srcAbs); err != nil {
				return fmt.Errorf("remove duplicate source %s: %w", srcAbs, err)
			}
			deleteDBRecord(database, roots, srcAbs)
			res.deduped++
			fmt.Printf("dedupe %s (identical content already at %s)\n", srcAbs, renamedDst)
			return nil
		}
		return fmt.Errorf("tagged destination %s exists and is not a regular file", renamedDst)
	}

	if err := doMove(srcAbs, renamedDst); err != nil {
		return err
	}
	updateDBMove(database, roots, srcAbs, renamedDst)
	res.renamed++
	fmt.Printf("moved  %s\n    →  %s (hash conflict)\n", srcAbs, renamedDst)
	return nil
}

// resolveHash returns the content hash for absPath: the DB hash if a record
// exists and has one, otherwise a freshly computed hash that is written back to
// the DB record when one exists.
func resolveHash(database *db.DB, roots []string, absPath string) (string, error) {
	root, rel := matchScanRoot(roots, absPath)
	var rec *db.FileRecord
	if root != "" {
		if r, found, err := database.GetByRelPath(root, rel); err == nil && found {
			rec = r
			if rec.Hash != "" {
				return rec.Hash, nil
			}
		}
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}
	h, err := hasher.HashFile(absPath, info.Size())
	if err != nil {
		return "", err
	}
	if rec != nil {
		_ = database.UpdateHash(rec.ID, h) // best-effort cache; move still proceeds
	}
	return h, nil
}

// updateDBMove patches the DB record for a file moved from srcAbs to dstAbs.
// Warns (does not fail) when either side is outside every known scan root.
func updateDBMove(database *db.DB, roots []string, srcAbs, dstAbs string) {
	srcRoot, srcRel := matchScanRoot(roots, srcAbs)
	if srcRoot == "" {
		fmt.Fprintf(os.Stderr, "warning: %s is not tracked in any scan root; DB not updated\n", srcAbs)
		return
	}
	dstRoot, dstRel := matchScanRoot(roots, dstAbs)
	if dstRoot == "" {
		fmt.Fprintf(os.Stderr, "warning: %s is not under any known scan root; DB not updated (rescan after moving)\n", dstAbs)
		return
	}
	if _, err := database.MoveFile(srcRoot, srcRel, dstRoot, dstRel, path.Base(dstRel)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: DB update failed for %s: %v\n", dstAbs, err)
	}
}

// deleteDBRecord removes the DB record for a source file that was deduped away.
func deleteDBRecord(database *db.DB, roots []string, srcAbs string) {
	srcRoot, srcRel := matchScanRoot(roots, srcAbs)
	if srcRoot == "" {
		return
	}
	if _, err := database.DeleteByRelPath(srcRoot, srcRel); err != nil {
		fmt.Fprintf(os.Stderr, "warning: DB delete failed for %s: %v\n", srcAbs, err)
	}
}

// hashSuffixName inserts "-<hash>" before the last extension of name.
// "README.md" → "README-<hash>.md"; "file4" → "file4-<hash>";
// "archive.tar.gz" → "archive.tar-<hash>.gz".
func hashSuffixName(name, hash string) string {
	ext := filepath.Ext(name) // includes leading dot, "" if none
	stem := name[:len(name)-len(ext)]
	return stem + "-" + hash + ext
}

// expandSrc resolves the source argument to a list of absolute paths.
// If the argument contains glob characters (* ? [), it is expanded via filepath.Glob.
func expandSrc(arg string) ([]string, error) {
	if !strings.ContainsAny(arg, "*?[") {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, fmt.Errorf("resolve src: %w", err)
		}
		return []string{filepath.Clean(abs)}, nil
	}
	matches, err := filepath.Glob(arg)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", arg, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no files match %q", arg)
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		abs, _ := filepath.Abs(m)
		out[i] = filepath.Clean(abs)
	}
	return out, nil
}

// doMove moves a single regular file src → dst, creating the parent directory.
// It tries os.Rename first; on failure (e.g. cross-device) it falls back to copy+delete.
func doMove(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// copyFile copies src to dst, preserving the source file's permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// cleanupEmptyDirs removes all empty subdirectories under dir, bottom-up, then dir itself.
func cleanupEmptyDirs(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // only succeeds if empty
	}
	_ = os.Remove(dir)
}

// matchScanRoot returns the deepest scan root that contains absPath,
// and the path of absPath relative to that root (forward-slash separated).
// Returns ("", "") if no scan root covers absPath.
func matchScanRoot(roots []string, absPath string) (scanRoot, relPath string) {
	best := ""
	for _, r := range roots {
		if r == absPath {
			return r, "."
		}
		if strings.HasPrefix(absPath, r+string(filepath.Separator)) && len(r) > len(best) {
			best = r
		}
	}
	if best == "" {
		return "", ""
	}
	return best, filepath.ToSlash(absPath[len(best)+1:])
}
