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
)

var moveDbOnly bool

var moveCmd = &cobra.Command{
	Use:   "move <src> <dst>",
	Short: "Move file(s)/folder(s) and update the database (no hash recalculation)",
	Long: `Move <src> to <dst> and patch all matching database records.
<src> may be a glob pattern (e.g. 'photos/*.jpg', 'dir/file*').
Hashes are preserved — no files are re-read or re-hashed.

If <dst> is an existing directory, each matched source is moved inside it.
When <src> matches multiple items, <dst> must be (or become) a directory.

If an atomic move fails (e.g. cross-device), a file-by-file fallback is used.
Only files that were actually moved on the filesystem are updated in the DB.

Use --db-only to skip the filesystem move and only update the DB records
(useful when files have already been moved manually).`,
	Args: cobra.ExactArgs(2),
	RunE: runMove,
}

func init() {
	moveCmd.Flags().BoolVar(&moveDbOnly, "db-only", false, "skip filesystem move; only patch database records")
	rootCmd.AddCommand(moveCmd)
}

// movedFile records a single successfully-moved regular file.
type movedFile struct {
	srcAbs string
	dstAbs string
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

	var totalFiles, totalFolders int64
	var totalFail int

	for _, srcAbs := range srcs {
		// mv semantics: if dst is an existing directory, move src inside it
		dstAbs := dstBase
		if info, statErr := os.Lstat(dstAbs); statErr == nil && info.IsDir() {
			dstAbs = filepath.Join(dstAbs, filepath.Base(srcAbs))
		}

		f, fol, fail, moveErr := moveSingleItem(srcAbs, dstAbs, database, roots)
		totalFiles += f
		totalFolders += fol
		totalFail += fail
		if moveErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", moveErr)
		}
	}

	if totalFolders > 0 || len(srcs) > 1 {
		fmt.Printf("DB updated: %d file record(s), %d folder record(s)\n", totalFiles, totalFolders)
	} else if totalFiles == 0 && totalFail == 0 {
		fmt.Fprintln(os.Stderr, "warning: no DB record found for source path; DB not updated")
	} else {
		fmt.Printf("DB updated: %d file record(s)\n", totalFiles)
	}
	if totalFail > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d item(s) could not be moved\n", totalFail)
	}
	return nil
}

// moveSingleItem moves srcAbs to dstAbs (on FS unless --db-only) and patches the DB.
// Returns filesUpdated, foldersUpdated, failCount, and any fatal error.
//
// For atomic renames (os.Rename succeeds): all matching DB records are updated.
// For file-by-file fallback: only records for files that were actually moved are updated,
// keeping the DB consistent with the filesystem.
func moveSingleItem(srcAbs, dstAbs string, database *db.DB, roots []string) (filesUpdated, foldersUpdated int64, failCount int, err error) {
	srcInfo, srcErr := os.Lstat(srcAbs)
	if srcErr != nil && !moveDbOnly {
		return 0, 0, 1, fmt.Errorf("stat %s: %w", srcAbs, srcErr)
	}

	var isDir bool
	if srcErr == nil {
		isDir = srcInfo.IsDir()
	} else {
		// --db-only and src already gone: infer isDir from dst
		if dstInfo, dstErr := os.Lstat(dstAbs); dstErr == nil {
			isDir = dstInfo.IsDir()
		}
	}

	srcRoot, srcRel := matchScanRoot(roots, srcAbs)

	// --- filesystem move ---
	// movedFiles is non-nil only when the file-by-file fallback was used.
	// A nil movedFiles means the entire srcAbs was atomically moved (or --db-only).
	var movedFiles []movedFile

	if !moveDbOnly {
		if renameErr := os.Rename(srcAbs, dstAbs); renameErr == nil {
			fmt.Printf("moved  %s\n    →  %s\n", srcAbs, dstAbs)
		} else if !isDir {
			return 0, 0, 1, fmt.Errorf("move %s: %w", filepath.Base(srcAbs), renameErr)
		} else {
			// Folder rename failed (e.g. cross-device, permission): fall back to file-by-file.
			// Only files that actually move will be recorded, preserving FS/DB consistency.
			fmt.Fprintf(os.Stderr, "atomic move failed (%v), falling back to file-by-file\n", renameErr)
			movedFiles, failCount, err = moveFileByFile(srcAbs, dstAbs)
			if err != nil {
				return 0, 0, failCount, err
			}
			if len(movedFiles) == 0 && failCount > 0 {
				return 0, 0, failCount, fmt.Errorf("no files could be moved from %s", srcAbs)
			}
			fmt.Printf("moved %d file(s) from %s", len(movedFiles), filepath.Base(srcAbs))
			if failCount > 0 {
				fmt.Printf(", %d skipped (see stderr for details)", failCount)
			}
			fmt.Println()
		}
	}

	// --- DB update ---
	if srcRoot == "" {
		fmt.Fprintf(os.Stderr, "warning: %s is not tracked in any scan root; DB not updated\n", srcAbs)
		return 0, 0, failCount, nil
	}

	if movedFiles != nil {
		// Partial move: update DB only for files that were actually moved on disk.
		// Folder hash records are intentionally skipped — they are now stale and
		// the user should run `scan --prune` to refresh them.
		filesUpdated, err = updateDBPartialMove(database, roots, movedFiles)
		return filesUpdated, 0, failCount, err
	}

	// Full move (atomic rename or --db-only): update all matching records.
	if isDir && srcRel == "." {
		// Moving an entire scan root: only scan_root column changes, rel_paths stay.
		filesUpdated, foldersUpdated, err = database.RenameScanRoot(srcRoot, dstAbs)
		return
	}

	dstRoot, dstRel := matchScanRoot(roots, dstAbs)
	if dstRoot == "" {
		return 0, 0, failCount, fmt.Errorf("destination %s is not under any known scan root; rescan after moving", dstAbs)
	}

	if isDir {
		filesUpdated, foldersUpdated, err = database.MoveFolder(srcRoot, srcRel, dstRoot, dstRel)
	} else {
		filesUpdated, err = database.MoveFile(srcRoot, srcRel, dstRoot, dstRel, path.Base(dstRel))
	}
	return
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

// updateDBPartialMove updates DB records only for files listed in movedFiles,
// all within a single transaction to ensure atomicity.
func updateDBPartialMove(database *db.DB, roots []string, movedFiles []movedFile) (int64, error) {
	if len(movedFiles) == 0 {
		return 0, nil
	}
	tx, err := database.Begin()
	if err != nil {
		return 0, err
	}
	var updated int64
	for _, mf := range movedFiles {
		srcRoot, srcRel := matchScanRoot(roots, mf.srcAbs)
		if srcRoot == "" {
			continue
		}
		dstRoot, dstRel := matchScanRoot(roots, mf.dstAbs)
		if dstRoot == "" {
			continue
		}
		n, txErr := database.MoveFileTx(tx, srcRoot, srcRel, dstRoot, dstRel, path.Base(dstRel))
		if txErr != nil {
			_ = tx.Rollback()
			return updated, txErr
		}
		updated += n
	}
	return updated, tx.Commit()
}

// moveFileByFile recursively moves a directory tree file-by-file.
// For each file it first tries os.Rename; on failure (e.g. cross-device) it falls back
// to copy+delete. Only successfully moved files are included in the returned slice.
func moveFileByFile(srcDir, dstDir string) (moved []movedFile, failCount int, err error) {
	if mkErr := os.MkdirAll(dstDir, 0755); mkErr != nil {
		return nil, 0, fmt.Errorf("create destination directory: %w", mkErr)
	}
	walkErr := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			failCount++
			fmt.Fprintf(os.Stderr, "  skip %s: %v\n", p, werr)
			return nil
		}
		if p == srcDir {
			return nil
		}
		rel, _ := filepath.Rel(srcDir, p)
		dst := filepath.Join(dstDir, rel)
		if d.IsDir() {
			mode := fs.FileMode(0755)
			if info, iErr := d.Info(); iErr == nil {
				mode = info.Mode().Perm()
			}
			_ = os.MkdirAll(dst, mode)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if mvErr := moveOneFile(p, dst); mvErr != nil {
			failCount++
			fmt.Fprintf(os.Stderr, "  skip %s: %v\n", p, mvErr)
			return nil
		}
		moved = append(moved, movedFile{srcAbs: p, dstAbs: dst})
		return nil
	})
	cleanupEmptyDirs(srcDir)
	return moved, failCount, walkErr
}

// moveOneFile tries os.Rename; on failure falls back to copy+delete.
func moveOneFile(src, dst string) error {
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
