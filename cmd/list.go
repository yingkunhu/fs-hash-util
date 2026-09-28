package cmd

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yhu/fs-hash-util/internal/db"
)

var (
	listMaxDepth int
	listLimit    int
)

var listCmd = &cobra.Command{
	Use:   "list [path/pattern]",
	Short: "List files and folders from the database, ls-style",
	Long: `List files and folders stored in the database.

  [path/pattern]  optional:
    - omit to list the root level of every scan root
    - absolute or relative path to list that directory or file
    - glob characters (* ? [) in the last component filter by name
      e.g. 'fshash list /photos/2024*'  or  'fshash list /docs/*.pdf'

--max-depth  0 = direct children only (default); -1 = unlimited recursion.
--limit      cap entries shown per directory listing (default 0 = unlimited).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runList,
}

func init() {
	listCmd.Flags().IntVar(&listMaxDepth, "max-depth", 0, "recursion depth (0=direct children, -1=unlimited)")
	listCmd.Flags().IntVar(&listLimit, "limit", 0, "max entries per directory (0=unlimited)")
	rootCmd.AddCommand(listCmd)
}

func runList(_ *cobra.Command, args []string) error {
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	roots, err := database.AllScanRoots()
	if err != nil {
		return fmt.Errorf("query scan roots: %w", err)
	}
	if len(roots) == 0 {
		fmt.Fprintln(os.Stderr, "no scan roots found; run 'fshash scan <dir>' first")
		return nil
	}

	if len(args) == 0 {
		// List root level of every scan root
		for _, r := range roots {
			if err := doList(database, r, "."); err != nil {
				fmt.Fprintf(os.Stderr, "error listing %s: %v\n", r, err)
			}
		}
		return nil
	}

	arg := args[0]
	hasGlob := strings.ContainsAny(filepath.Base(arg), "*?[")

	if !hasGlob {
		absArg, err := filepath.Abs(arg)
		if err != nil {
			return fmt.Errorf("resolve path: %w", err)
		}
		absArg = filepath.Clean(absArg)
		scanRoot, relPrefix := matchScanRoot(roots, absArg)
		if scanRoot == "" {
			return fmt.Errorf("%s is not under any known scan root", absArg)
		}
		return doList(database, scanRoot, relPrefix)
	}

	// Glob: pattern applies to the base name at the resolved directory
	absDir, err := filepath.Abs(filepath.Dir(arg))
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	absDir = filepath.Clean(absDir)
	pattern := filepath.ToSlash(filepath.Base(arg))

	scanRoot, relPrefix := matchScanRoot(roots, absDir)
	if scanRoot == "" {
		return fmt.Errorf("%s is not under any known scan root", absDir)
	}

	filter := func(name string) bool {
		ok, _ := filepath.Match(pattern, name)
		return ok
	}
	return doList(database, scanRoot, relPrefix, filter)
}

// doList loads all data under scanRoot/relPrefix once and renders it.
// An optional nameFilter is applied to direct children at depth 0.
func doList(database *db.DB, scanRoot, relPrefix string, filter ...func(string) bool) error {
	// Check if relPrefix is a single file record
	if relPrefix != "." {
		if rec, found, err := database.GetByRelPath(scanRoot, relPrefix); err == nil && found {
			printSingleFile(scanRoot, rec)
			return nil
		}
	}

	allFiles, err := database.FilesUnderPrefix(scanRoot, relPrefix)
	if err != nil {
		return err
	}
	allFolders, err := database.FoldersUnderPrefix(scanRoot, relPrefix)
	if err != nil {
		return err
	}

	// If folders table is empty (old DB), infer structure from file paths
	if len(allFolders) == 0 {
		allFolders = foldersFromFiles(allFiles)
	}

	sizeMap, mtimeMap := buildFolderStats(allFiles)

	var nameFilter func(string) bool
	if len(filter) > 0 {
		nameFilter = filter[0]
	}

	renderLevel(scanRoot, relPrefix, allFiles, allFolders, sizeMap, mtimeMap, 0, nameFilter)
	return nil
}

// printSingleFile shows a single file record in ls-al style.
func printSingleFile(scanRoot string, rec *db.FileRecord) {
	absDir := scanRoot
	if d := path.Dir(rec.RelPath); d != "." {
		absDir = scanRoot + "/" + d
	}
	fmt.Printf("\n%s:\n", absDir)
	date := time.Unix(0, rec.ModifiedNS).Format("2006-01-02 15:04")
	fmt.Printf("   %8s  %s  %s\n", humanSize(rec.Size), date, rec.FileName)
}

// renderLevel prints one directory section (scanRoot/relPrefix) and recurses if depth allows.
// nameFilter, when non-nil, is applied only at depth 0 to select entries by base name.
func renderLevel(
	scanRoot, relPrefix string,
	allFiles []db.FileRecord,
	allFolders []db.FolderRecord,
	sizeMap, mtimeMap map[string]int64,
	depth int,
	nameFilter func(string) bool,
) {
	type entry struct {
		isDir   bool
		name    string
		relPath string
		size    int64
		mtime   int64
	}

	var entries []entry

	for _, f := range allFiles {
		if path.Dir(f.RelPath) != relPrefix {
			continue
		}
		if depth == 0 && nameFilter != nil && !nameFilter(f.FileName) {
			continue
		}
		entries = append(entries, entry{
			isDir: false, name: f.FileName, relPath: f.RelPath,
			size: f.Size, mtime: f.ModifiedNS,
		})
	}

	for _, fold := range allFolders {
		if path.Dir(fold.RelPath) != relPrefix {
			continue
		}
		name := path.Base(fold.RelPath)
		if depth == 0 && nameFilter != nil && !nameFilter(name) {
			continue
		}
		entries = append(entries, entry{
			isDir: true, name: name, relPath: fold.RelPath,
			size: sizeMap[fold.RelPath], mtime: mtimeMap[fold.RelPath],
		})
	}

	// Alphabetical sort (case-insensitive), matching ls behaviour
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].name) < strings.ToLower(entries[j].name)
	})

	// Per-directory limit
	truncated := false
	if listLimit > 0 && len(entries) > listLimit {
		entries = entries[:listLimit]
		truncated = true
	}

	// Section header
	absPrefix := scanRoot
	if relPrefix != "." {
		absPrefix = scanRoot + "/" + relPrefix
	}
	fmt.Printf("\n%s:\n", absPrefix)

	// Total line
	var totalSize int64
	fileCount, dirCount := 0, 0
	for _, e := range entries {
		totalSize += e.size
		if e.isDir {
			dirCount++
		} else {
			fileCount++
		}
	}
	fmt.Printf("total %s  (%d files, %d dirs)\n", humanSize(totalSize), fileCount, dirCount)

	// Entries
	for _, e := range entries {
		typeChar := " "
		name := e.name
		if e.isDir {
			typeChar = "d"
			name += "/"
		}
		dateStr := "                " // 16 spaces placeholder
		if e.mtime > 0 {
			dateStr = time.Unix(0, e.mtime).Format("2006-01-02 15:04")
		}
		fmt.Printf("%s  %8s  %s  %s\n", typeChar, humanSize(e.size), dateStr, name)
	}
	if truncated {
		fmt.Printf("  ... (limit %d reached; use --limit 0 for all)\n", listLimit)
	}

	// Recurse into subdirectories
	if listMaxDepth < 0 || depth < listMaxDepth {
		for _, e := range entries {
			if e.isDir {
				renderLevel(scanRoot, e.relPath, allFiles, allFolders, sizeMap, mtimeMap, depth+1, nil)
			}
		}
	}
}

// buildFolderStats accumulates total size and latest mtime for every folder
// by walking each file's ancestor chain. O(n * avg_depth).
func buildFolderStats(files []db.FileRecord) (sizeMap, mtimeMap map[string]int64) {
	sizeMap = make(map[string]int64)
	mtimeMap = make(map[string]int64)
	for _, f := range files {
		parts := strings.Split(f.RelPath, "/")
		// Accumulate into each ancestor folder (not into the file's own entry)
		for i := 1; i < len(parts); i++ {
			ancestor := strings.Join(parts[:i], "/")
			sizeMap[ancestor] += f.Size
			if f.ModifiedNS > mtimeMap[ancestor] {
				mtimeMap[ancestor] = f.ModifiedNS
			}
		}
		// Also accumulate into the scan root "."
		sizeMap["."] += f.Size
		if f.ModifiedNS > mtimeMap["."] {
			mtimeMap["."] = f.ModifiedNS
		}
	}
	return
}

// foldersFromFiles infers folder records from file rel_paths.
// Used as a fallback when the folders table is empty (old DB without folder hashing).
func foldersFromFiles(files []db.FileRecord) []db.FolderRecord {
	seen := make(map[string]struct{})
	var out []db.FolderRecord
	for _, f := range files {
		parts := strings.Split(f.RelPath, "/")
		for i := 1; i < len(parts); i++ {
			dir := strings.Join(parts[:i], "/")
			if _, ok := seen[dir]; !ok {
				seen[dir] = struct{}{}
				out = append(out, db.FolderRecord{ScanRoot: f.ScanRoot, RelPath: dir})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out
}
