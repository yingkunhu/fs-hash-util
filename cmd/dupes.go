package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yhu/fs-hash-util/internal/db"
)

var (
	dupesLevel   string
	dupesMinSize string
	dupesRoot    string
	dupesOutput  string
)

func init() {
	dupesCmd.Flags().StringVar(&dupesLevel, "level", "files", "report level: files or folders")
	dupesCmd.Flags().StringVar(&dupesMinSize, "min-size", "0", "only include duplicates with size >= value (e.g. 0, 1024, 1K, 10M, 2G)")
	dupesCmd.Flags().StringVar(&dupesRoot, "root", "", "filter by scan root directory")
	dupesCmd.Flags().StringVar(&dupesOutput, "output", "", "write report to this file (stdout if omitted)")
	rootCmd.AddCommand(dupesCmd)
}

var dupesCmd = &cobra.Command{
	Use:   "dupes",
	Short: "Report duplicate files or folders",
	RunE:  runDupes,
}

type dupeGroup struct {
	Hash  string   `json:"hash"`
	Size  int64    `json:"size"`
	Count int      `json:"count"`
	Paths []string `json:"paths"`
}

type dupeSummary struct {
	Groups      int   `json:"groups"`
	TotalItems  int   `json:"total_items"`
	WastedBytes int64 `json:"wasted_bytes"`
}

type dupeReport struct {
	Level   string      `json:"level"`
	MinSize int64       `json:"min_size"`
	Groups  []dupeGroup `json:"groups"`
	Summary dupeSummary `json:"summary"`
}

func runDupes(_ *cobra.Command, _ []string) error {
	switch dupesLevel {
	case "files", "folders":
	default:
		return fmt.Errorf("--level must be 'files' or 'folders', got %q", dupesLevel)
	}

	minSize, err := parseSize(dupesMinSize)
	if err != nil {
		return fmt.Errorf("--min-size: %w", err)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	var report dupeReport
	if dupesLevel == "files" {
		report, err = buildFileDupes(database, dupesRoot, minSize)
	} else {
		report, err = buildFolderDupes(database, dupesRoot, minSize)
	}
	if err != nil {
		return err
	}
	report.Level = dupesLevel
	report.MinSize = minSize

	var w io.Writer = os.Stdout
	if dupesOutput != "" {
		f, fErr := os.Create(dupesOutput)
		if fErr != nil {
			return fmt.Errorf("create output file: %w", fErr)
		}
		defer f.Close()
		w = f
	}

	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	return writePlainDupes(w, report)
}

func buildFileDupes(database *db.DB, root string, minSize int64) (dupeReport, error) {
	records, err := database.DuplicateFiles(root, minSize)
	if err != nil {
		return dupeReport{}, err
	}

	type group struct {
		size  int64
		paths []string
	}
	var order []string
	groups := map[string]*group{}
	for _, r := range records {
		if _, ok := groups[r.Hash]; !ok {
			groups[r.Hash] = &group{size: r.Size}
			order = append(order, r.Hash)
		}
		groups[r.Hash].paths = append(groups[r.Hash].paths, filepath.Join(r.ScanRoot, r.RelPath))
	}

	var report dupeReport
	report.Groups = []dupeGroup{}
	for _, hash := range order {
		g := groups[hash]
		paths := dedupSamePaths(g.paths)
		if len(paths) < 2 {
			continue
		}
		report.Groups = append(report.Groups, dupeGroup{
			Hash:  hash,
			Size:  g.size,
			Count: len(paths),
			Paths: paths,
		})
		report.Summary.TotalItems += len(paths)
		report.Summary.WastedBytes += int64(len(paths)-1) * g.size
	}
	report.Summary.Groups = len(report.Groups)
	return report, nil
}

type sizedGroup struct {
	hash  string
	size  int64
	paths []string
}

func buildFolderDupes(database *db.DB, root string, minSize int64) (dupeReport, error) {
	records, err := database.DuplicateFolders(root)
	if err != nil {
		return dupeReport{}, err
	}

	type folderKey struct{ scanRoot, relPath string }
	type group struct {
		folders []folderKey
	}
	var order []string
	groups := map[string]*group{}
	for _, r := range records {
		if _, ok := groups[r.Hash]; !ok {
			groups[r.Hash] = &group{}
			order = append(order, r.Hash)
		}
		groups[r.Hash].folders = append(groups[r.Hash].folders, folderKey{r.ScanRoot, r.RelPath})
	}

	var valid []sizedGroup
	for _, hash := range order {
		g := groups[hash]
		// All folders in the group have identical content, so compute size from the first entry.
		size, sErr := database.FolderSize(g.folders[0].scanRoot, g.folders[0].relPath)
		if sErr != nil {
			return dupeReport{}, fmt.Errorf("folder size %q: %w", g.folders[0].relPath, sErr)
		}
		if size < minSize {
			continue
		}
		var paths []string
		for _, fk := range g.folders {
			paths = append(paths, filepath.Join(fk.scanRoot, fk.relPath))
		}
		paths = dedupSamePaths(paths)
		if len(paths) < 2 {
			continue
		}
		valid = append(valid, sizedGroup{hash: hash, size: size, paths: paths})
	}

	sort.Slice(valid, func(i, j int) bool { return valid[i].size > valid[j].size })

	valid = suppressNestedGroups(valid)

	var report dupeReport
	report.Groups = []dupeGroup{}
	for _, sg := range valid {
		report.Groups = append(report.Groups, dupeGroup{
			Hash:  sg.hash,
			Size:  sg.size,
			Count: len(sg.paths),
			Paths: sg.paths,
		})
		report.Summary.TotalItems += len(sg.paths)
		report.Summary.WastedBytes += int64(len(sg.paths)-1) * sg.size
	}
	report.Summary.Groups = len(report.Groups)
	return report, nil
}

// suppressNestedGroups drops any folder-duplicate group whose every path is a
// strict descendant of the corresponding path in a single larger group. When two
// folders are reported as duplicates as a whole, their internal sub-folders are
// necessarily duplicated too; those nested groups are redundant noise, so only the
// top-most (ancestor) groups are kept.
//
// A group G is suppressed iff there exists a kept group A such that every path in
// G is a strict descendant of some path in A. groups must be sorted largest-first
// so ancestors (larger size) are considered before their descendants.
func suppressNestedGroups(groups []sizedGroup) []sizedGroup {
	kept := make([]sizedGroup, 0, len(groups))
	for _, g := range groups {
		covered := false
		for _, a := range kept {
			if groupCovers(a.paths, g.paths) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, g)
		}
	}
	return kept
}

// groupCovers reports whether every path in child is a strict descendant of some
// path in parent.
func groupCovers(parent, child []string) bool {
	for _, c := range child {
		found := false
		for _, p := range parent {
			if isStrictDescendant(c, p) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// isStrictDescendant reports whether child is nested strictly below ancestor,
// comparing on path-separator boundaries so "/a/foo" is not treated as a
// descendant of "/a/fo".
func isStrictDescendant(child, ancestor string) bool {
	c := filepath.Clean(child)
	a := filepath.Clean(ancestor)
	if c == a {
		return false
	}
	prefix := a
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(c, prefix)
}

// dedupSamePaths removes paths that point to the same physical filesystem entry
// (identical device+inode). Paths that cannot be stat'd are kept as-is.
// This handles case-insensitive filesystems where the same directory may be
// stored under different-cased paths in the database.
func dedupSamePaths(paths []string) []string {
	var (
		seen []os.FileInfo
		out  []string
	)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			out = append(out, p)
			continue
		}
		same := false
		for _, prev := range seen {
			if os.SameFile(prev, fi) {
				same = true
				break
			}
		}
		if !same {
			seen = append(seen, fi)
			out = append(out, p)
		}
	}
	return out
}

func writePlainDupes(w io.Writer, r dupeReport) error {
	levelLabel := "Files"
	if r.Level == "folders" {
		levelLabel = "Folders"
	}
	fmt.Fprintf(w, "Duplicate %s  groups=%d  wasted=%s\n",
		levelLabel, r.Summary.Groups, formatSize(r.Summary.WastedBytes))
	if len(r.Groups) == 0 {
		return nil
	}
	fmt.Fprintln(w, strings.Repeat("─", 60))
	for i, g := range r.Groups {
		fmt.Fprintf(w, "\n #%d  hash=%.16s…  size=%s  count=%d\n",
			i+1, g.Hash, formatSize(g.Size), g.Count)
		for _, p := range g.Paths {
			fmt.Fprintf(w, "     %s\n", p)
		}
	}
	return nil
}

func formatSize(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
		tb = 1 << 40
	)
	switch {
	case n >= tb:
		return fmt.Sprintf("%.1f TB", float64(n)/float64(tb))
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// parseSize parses a size string such as "1024", "1K", "10M", "2G", "1.5TB".
// Supported units (case-insensitive): B, K, KB, M, MB, G, GB, T, TB.
// No unit means bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	upper := strings.ToUpper(s)

	units := map[string]int64{
		"B": 1,
		"K": 1 << 10, "KB": 1 << 10,
		"M": 1 << 20, "MB": 1 << 20,
		"G": 1 << 30, "GB": 1 << 30,
		"T": 1 << 40, "TB": 1 << 40,
	}

	i := 0
	for i < len(upper) && (upper[i] >= '0' && upper[i] <= '9' || upper[i] == '.') {
		i++
	}
	numStr := upper[:i]
	unit := upper[i:]

	if numStr == "" {
		return 0, fmt.Errorf("no numeric value in %q", s)
	}
	mult := int64(1)
	if unit != "" {
		m, ok := units[unit]
		if !ok {
			return 0, fmt.Errorf("unknown unit %q (valid: B K KB M MB G GB T TB)", unit)
		}
		mult = m
	}

	if n, err := strconv.ParseInt(numStr, 10, 64); err == nil {
		return n * mult, nil
	}
	if f, err := strconv.ParseFloat(numStr, 64); err == nil {
		return int64(f * float64(mult)), nil
	}
	return 0, fmt.Errorf("cannot parse size %q", s)
}
