package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yhu/fs-hash-util/internal/db"
	"github.com/yhu/fs-hash-util/internal/hasher"
)

// --- helpers ---

func openMoveTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	// Satisfy the files.scan_id FK (REFERENCES scans(id)): seed records use ScanID 1.
	if _, err := database.CreateScan("test-root", 0); err != nil {
		t.Fatalf("create scan: %v", err)
	}
	return database
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func hashOf(t *testing.T, content string) string {
	t.Helper()
	// mirror hasher.HashFile over an in-memory string by writing a temp file
	f := filepath.Join(t.TempDir(), "h")
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	h, err := hasher.HashFile(f, int64(len(content)))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// seedFile inserts a file record for absPath under scanRoot with the given hash.
func seedFile(t *testing.T, database *db.DB, scanRoot, absPath, hash string, size int64) {
	t.Helper()
	rel, err := filepath.Rel(scanRoot, absPath)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	rel = filepath.ToSlash(rel)
	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := database.Upsert(tx, db.FileRecord{
		ScanRoot: scanRoot, FileName: filepath.Base(absPath), RelPath: rel,
		Size: size, Hash: hash, ScanID: 1,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// seedFolder inserts a folder-hash record under scanRoot.
func seedFolder(t *testing.T, database *db.DB, scanRoot, relPath, hash string, scanID int64) {
	t.Helper()
	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := database.UpsertFolder(tx, db.FolderRecord{
		ScanRoot: scanRoot, RelPath: relPath, Hash: hash, ScanID: scanID,
	}); err != nil {
		t.Fatalf("upsert folder: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func dbHash(t *testing.T, database *db.DB, scanRoot, absPath string) (string, bool) {
	t.Helper()
	rel, _ := filepath.Rel(scanRoot, absPath)
	rec, found, err := database.GetByRelPath(scanRoot, filepath.ToSlash(rel))
	if err != nil {
		t.Fatalf("getbyrelpath: %v", err)
	}
	if !found {
		return "", false
	}
	return rec.Hash, true
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// --- hashSuffixName unit tests ---

func TestHashSuffixName(t *testing.T) {
	const h = "6fee39eceaa5fe9dd1b64df12d8b1ddf05b913bc69affc18dd0dc5fe96e1e49d"
	cases := []struct{ name, want string }{
		{"README.md", "README-" + h + ".md"},
		{"file4", "file4-" + h},
		{"archive.tar.gz", "archive.tar-" + h + ".gz"},
		{".env", "-" + h + ".env"}, // dotfile: ext is the whole name per filepath.Ext
	}
	for _, c := range cases {
		if got := hashSuffixName(c.name, h); got != c.want {
			t.Errorf("hashSuffixName(%q)=%q want %q", c.name, got, c.want)
		}
	}
}

// --- behavior tests ---

// No conflict: file moves and DB rel_path is updated.
func TestResolveMove_NoConflict(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	src := filepath.Join(root, "a.txt")
	dst := filepath.Join(root, "sub", "a.txt")
	writeFile(t, src, "hello")
	seedFile(t, database, root, src, hashOf(t, "hello"), 5)

	var res moveResult
	if err := resolveAndMove(src, dst, database, []string{root}, &res); err != nil {
		t.Fatalf("resolveAndMove: %v", err)
	}
	if exists(src) || !exists(dst) {
		t.Fatalf("file not moved: srcExists=%v dstExists=%v", exists(src), exists(dst))
	}
	if res.moved != 1 {
		t.Fatalf("want moved=1, got %+v", res)
	}
	if _, ok := dbHash(t, database, root, src); ok {
		t.Error("source DB record should be gone")
	}
	if _, ok := dbHash(t, database, root, dst); !ok {
		t.Error("destination DB record missing")
	}
}

// Same-hash conflict: source deleted, target untouched, source DB row removed.
func TestResolveMove_SameHashDedupe(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcDir := filepath.Join(root, "s")
	dstDir := filepath.Join(root, "d")
	src := filepath.Join(srcDir, "file1")
	dst := filepath.Join(dstDir, "file1")
	writeFile(t, src, "same")
	writeFile(t, dst, "same")
	seedFile(t, database, root, src, hashOf(t, "same"), 4)
	seedFile(t, database, root, dst, hashOf(t, "same"), 4)

	var res moveResult
	if err := resolveAndMove(src, dst, database, []string{root}, &res); err != nil {
		t.Fatalf("resolveAndMove: %v", err)
	}
	if exists(src) {
		t.Error("source should be deleted on same-hash")
	}
	if !exists(dst) {
		t.Error("destination must be preserved")
	}
	if res.deduped != 1 {
		t.Fatalf("want deduped=1, got %+v", res)
	}
	if _, ok := dbHash(t, database, root, src); ok {
		t.Error("source DB record should be deleted")
	}
	if h, ok := dbHash(t, database, root, dst); !ok || h != hashOf(t, "same") {
		t.Error("destination DB record must remain unchanged")
	}
}

// Different-hash conflict: source moved to stem-<hash>.ext, both files present.
func TestResolveMove_DiffHashRename(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcDir := filepath.Join(root, "s")
	dstDir := filepath.Join(root, "d")
	src := filepath.Join(srcDir, "file4")
	dst := filepath.Join(dstDir, "file4")
	writeFile(t, src, "content-EFG")
	writeFile(t, dst, "target-XYZ")
	srcHash := hashOf(t, "content-EFG")
	seedFile(t, database, root, src, srcHash, int64(len("content-EFG")))
	seedFile(t, database, root, dst, hashOf(t, "target-XYZ"), int64(len("target-XYZ")))

	var res moveResult
	if err := resolveAndMove(src, dst, database, []string{root}, &res); err != nil {
		t.Fatalf("resolveAndMove: %v", err)
	}
	renamed := filepath.Join(dstDir, "file4-"+srcHash)
	if exists(src) {
		t.Error("source should have been moved away")
	}
	if !exists(dst) {
		t.Error("original destination must be preserved")
	}
	if !exists(renamed) {
		t.Fatalf("renamed file %s not created", renamed)
	}
	if res.renamed != 1 {
		t.Fatalf("want renamed=1, got %+v", res)
	}
	// DB: original dst row intact, source row now at renamed rel path.
	if h, ok := dbHash(t, database, root, renamed); !ok || h != srcHash {
		t.Errorf("renamed DB record wrong: hash=%q ok=%v want %q", h, ok, srcHash)
	}
	if h, ok := dbHash(t, database, root, dst); !ok || h != hashOf(t, "target-XYZ") {
		t.Error("destination DB record must be unchanged")
	}
}

// Runtime hash: DB record exists but hash empty → computed and persisted back.
func TestResolveMove_RuntimeHashPersisted(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	src := filepath.Join(root, "s", "a.txt")
	dst := filepath.Join(root, "d", "a.txt")
	writeFile(t, src, "same")
	writeFile(t, dst, "same")
	seedFile(t, database, root, src, "", 4) // empty hash on purpose
	seedFile(t, database, root, dst, "", 4)

	var res moveResult
	if err := resolveAndMove(src, dst, database, []string{root}, &res); err != nil {
		t.Fatalf("resolveAndMove: %v", err)
	}
	if res.deduped != 1 {
		t.Fatalf("want deduped=1 (identical content), got %+v", res)
	}
	// Destination hash should have been computed and written back.
	if h, ok := dbHash(t, database, root, dst); !ok || h != hashOf(t, "same") {
		t.Errorf("runtime hash not persisted to dst record: got %q", h)
	}
}

// Folder merge mirroring the user's example.
func TestMoveFolder_MergeExample(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcRoot := filepath.Join(root, "source")
	dstRoot := filepath.Join(root, "target")

	// Source tree.
	writeFile(t, filepath.Join(srcRoot, "file1"), "ABC")
	writeFile(t, filepath.Join(srcRoot, "folder_A", "file2"), "f2")
	writeFile(t, filepath.Join(srcRoot, "folder_A", "file3"), "f3")
	if err := os.MkdirAll(filepath.Join(srcRoot, "folderB"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(srcRoot, "folderC", "file4"), "EFG")

	// Target tree (pre-move).
	writeFile(t, filepath.Join(dstRoot, "file1"), "ABC")   // identical → dedupe
	writeFile(t, filepath.Join(dstRoot, "folderD", "file6"), "f6")
	writeFile(t, filepath.Join(dstRoot, "folderC", "file4"), "XYZ") // diff → rename
	writeFile(t, filepath.Join(dstRoot, "folderC", "file5"), "f5")

	var res moveResult
	if err := moveSingleItem(srcRoot, dstRoot, database, []string{root}, &res); err != nil {
		t.Fatalf("moveSingleItem: %v", err)
	}

	efg := hashOf(t, "EFG")
	wantPresent := []string{
		filepath.Join(dstRoot, "file1"),
		filepath.Join(dstRoot, "folder_A", "file2"),
		filepath.Join(dstRoot, "folder_A", "file3"),
		filepath.Join(dstRoot, "folderB"), // empty dir recreated
		filepath.Join(dstRoot, "folderD", "file6"),
		filepath.Join(dstRoot, "folderC", "file4"),       // original target kept
		filepath.Join(dstRoot, "folderC", "file4-"+efg),  // moved source, tagged
		filepath.Join(dstRoot, "folderC", "file5"),
	}
	for _, p := range wantPresent {
		if !exists(p) {
			t.Errorf("expected present after merge: %s", p)
		}
	}
	// file1 was deduped: original target kept with content ABC.
	if b, _ := os.ReadFile(filepath.Join(dstRoot, "file1")); string(b) != "ABC" {
		t.Errorf("target file1 content changed: %q", string(b))
	}
	// folderC/file4 original target must still be XYZ.
	if b, _ := os.ReadFile(filepath.Join(dstRoot, "folderC", "file4")); string(b) != "XYZ" {
		t.Errorf("target folderC/file4 overwritten: %q", string(b))
	}
	// Source tree removed.
	if exists(srcRoot) {
		t.Errorf("source tree should be cleaned up: %s still exists", srcRoot)
	}
	if res.deduped != 1 || res.renamed != 1 || res.moved != 2 {
		t.Fatalf("unexpected counts: %+v (want moved=2 renamed=1 deduped=1)", res)
	}
}

// oracleFolderHashes is the independent ground truth: what a fresh scan of scanRoot
// would store, derived straight from the current file records via the shared aggregator.
func oracleFolderHashes(t *testing.T, database *db.DB, scanRoot string) map[string]string {
	t.Helper()
	files, err := database.FilesUnderPrefix(scanRoot, ".")
	if err != nil {
		t.Fatalf("files under prefix: %v", err)
	}
	dirs, hashes := aggregateFolderHashes(files)
	out := make(map[string]string, len(dirs))
	for _, d := range dirs {
		out[d] = hashes[d]
	}
	return out
}

// dbFolderHashes reads the folders table for scanRoot into a map rel_path → hash.
func dbFolderHashes(t *testing.T, database *db.DB, scanRoot string) map[string]string {
	t.Helper()
	recs, err := database.AllFolders(scanRoot)
	if err != nil {
		t.Fatalf("all folders: %v", err)
	}
	out := make(map[string]string, len(recs))
	for _, r := range recs {
		out[r.RelPath] = r.Hash
	}
	return out
}

func assertFolderHashesConsistent(t *testing.T, database *db.DB, scanRoot string) {
	t.Helper()
	want := oracleFolderHashes(t, database, scanRoot)
	got := dbFolderHashes(t, database, scanRoot)
	if len(want) != len(got) {
		t.Errorf("[%s] folder count mismatch: db has %d (%v), fresh-scan would have %d (%v)",
			scanRoot, len(got), keys(got), len(want), keys(want))
	}
	for rel, wh := range want {
		if gh, ok := got[rel]; !ok {
			t.Errorf("[%s] folder %q missing from DB (fresh scan would create it)", scanRoot, rel)
		} else if gh != wh {
			t.Errorf("[%s] folder %q hash mismatch: db=%s fresh=%s", scanRoot, rel, gh, wh)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("[%s] stale folder %q present in DB (fresh scan would omit it)", scanRoot, rel)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Moving f1/f2/f3 → f4/ across two scan roots: both the source ancestors (f2, f1)
// and the target ancestors (f4) must end up consistent with the filesystem, and the
// emptied source leaf (f3) must be deleted from the folders table.
func TestMove_FolderHashesConsistent_BothChains(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcRootDir := filepath.Join(root, "src")  // scan root 1
	dstRootDir := filepath.Join(root, "dst")  // scan root 2

	// Source scan root: f1/f2/f3/file.txt  plus a sibling f1/other.txt so f1/f2 survive partially.
	moved := filepath.Join(srcRootDir, "f1", "f2", "f3", "data.bin")
	sibling := filepath.Join(srcRootDir, "f1", "keep.txt")
	writeFile(t, moved, "payload")
	writeFile(t, sibling, "sib")
	seedFile(t, database, srcRootDir, moved, hashOf(t, "payload"), 7)
	seedFile(t, database, srcRootDir, sibling, hashOf(t, "sib"), 3)

	// Target scan root: f4/existing.txt
	tExisting := filepath.Join(dstRootDir, "f4", "existing.txt")
	writeFile(t, tExisting, "exist")
	seedFile(t, database, dstRootDir, tExisting, hashOf(t, "exist"), 5)

	// Seed STALE folder hashes on both roots so recompute must actually fix them.
	seedFolder(t, database, srcRootDir, ".", "STALE", 1)
	seedFolder(t, database, srcRootDir, "f1", "STALE", 1)
	seedFolder(t, database, srcRootDir, "f1/f2", "STALE", 1)
	seedFolder(t, database, srcRootDir, "f1/f2/f3", "STALE", 1)
	seedFolder(t, database, dstRootDir, ".", "STALE", 1)
	seedFolder(t, database, dstRootDir, "f4", "STALE", 1)

	roots := []string{srcRootDir, dstRootDir}

	// Move the folder f3 into f4/ (becomes dst/f4/f3/...).
	src := filepath.Join(srcRootDir, "f1", "f2", "f3")
	dst := filepath.Join(dstRootDir, "f4", "f3")
	var res moveResult
	// Replicate runMove's affected-root recompute by calling the same helper afterwards.
	if err := moveSingleItem(src, dst, database, roots, &res); err != nil {
		t.Fatalf("moveSingleItem: %v", err)
	}
	for _, r := range roots {
		if err := recomputeFolderHashes(database, r); err != nil {
			t.Fatalf("recompute %s: %v", r, err)
		}
	}

	// Filesystem check: f3 moved under f4, sibling untouched.
	if exists(filepath.Join(srcRootDir, "f1", "f2", "f3")) {
		t.Error("source f3 should be gone after move")
	}
	if !exists(filepath.Join(dstRootDir, "f4", "f3", "data.bin")) {
		t.Error("f3/data.bin should now live under dst/f4")
	}

	// Folder-hash consistency on BOTH scan roots.
	assertFolderHashesConsistent(t, database, srcRootDir)
	assertFolderHashesConsistent(t, database, dstRootDir)

	// Emptied source leaf f1/f2/f3 must be deleted (no files left there).
	if _, found, _ := database.GetFolderByRelPath(srcRootDir, "f1/f2/f3"); found {
		t.Error("emptied source folder f1/f2/f3 must be deleted from folders table")
	}
	// f1/f2 also emptied (f3 was its only child) → deleted.
	if _, found, _ := database.GetFolderByRelPath(srcRootDir, "f1/f2"); found {
		t.Error("emptied source folder f1/f2 must be deleted from folders table")
	}
	// f1 survives (keep.txt remains) and must no longer be STALE.
	if h, found, _ := database.GetFolderByRelPath(srcRootDir, "f1"); !found || h.Hash == "STALE" {
		t.Errorf("source f1 should survive with a recomputed hash, got found=%v hash=%v", found, h)
	}
	// Target f4 gained f3 → hash must differ from STALE.
	if h, found, _ := database.GetFolderByRelPath(dstRootDir, "f4"); !found || h.Hash == "STALE" {
		t.Errorf("target f4 should be recomputed, got found=%v hash=%v", found, h)
	}
}

// --- atomic .git handling ---

// makeGitDir builds a fake .git tree at dir with the given leaf files (rel path → content).
// One file (objects/ro) is made read-only (0444) to exercise the git-objects permission case.
func makeGitDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		writeFile(t, p, content)
	}
	// Mark one object read-only if present.
	ro := filepath.Join(dir, "objects", "ro")
	if exists(ro) {
		if err := os.Chmod(ro, 0444); err != nil {
			t.Fatalf("chmod ro: %v", err)
		}
	}
}

func defaultGitFiles() map[string]string {
	return map[string]string{
		"HEAD":              "ref: refs/heads/main\n",
		"config":            "[core]\n\tbare = false\n",
		"refs/heads/main":   "abc123\n",
		"objects/ro":        "object-data",
	}
}

// collectTree returns every regular file under root as rel-path → content.
func collectTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		b, rErr := os.ReadFile(p)
		if rErr != nil {
			return rErr
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("collectTree: %v", err)
	}
	return out
}

func eqMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// No target .git: the whole .git is moved across intact.
func TestGit_NoConflict_MovedIntact(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcRepo := filepath.Join(root, "src", "repo")
	dstRepo := filepath.Join(root, "dst", "repo")
	srcGit := filepath.Join(srcRepo, ".git")
	makeGitDir(t, srcGit, defaultGitFiles())
	writeFile(t, filepath.Join(srcRepo, "readme.txt"), "hi")
	if err := os.MkdirAll(dstRepo, 0755); err != nil {
		t.Fatal(err)
	}

	before := collectTree(t, srcGit)

	var res moveResult
	if err := moveSingleItem(srcRepo, dstRepo, database, []string{root}, &res); err != nil {
		t.Fatalf("moveSingleItem: %v", err)
	}

	dstGit := filepath.Join(dstRepo, ".git")
	if !exists(dstGit) {
		t.Fatal(".git not moved to destination")
	}
	if exists(srcGit) {
		t.Error("source .git should be gone")
	}
	if got := collectTree(t, dstGit); !eqMap(before, got) {
		t.Errorf(".git tree not preserved intact:\n before=%v\n after =%v", before, got)
	}
	// readme.txt also moved (per-file path), .git counted as one moved unit.
	if !exists(filepath.Join(dstRepo, "readme.txt")) {
		t.Error("readme.txt not moved")
	}
}

// Identical target .git: source .git deleted, target untouched.
func TestGit_IdenticalDedupe(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcRepo := filepath.Join(root, "src", "repo")
	dstRepo := filepath.Join(root, "dst", "repo")
	srcGit := filepath.Join(srcRepo, ".git")
	dstGit := filepath.Join(dstRepo, ".git")
	makeGitDir(t, srcGit, defaultGitFiles())
	makeGitDir(t, dstGit, defaultGitFiles()) // byte-identical

	targetBefore := collectTree(t, dstGit)

	var res moveResult
	if err := moveSingleItem(srcRepo, dstRepo, database, []string{root}, &res); err != nil {
		t.Fatalf("moveSingleItem: %v", err)
	}

	if exists(srcGit) {
		t.Error("identical source .git should be deleted")
	}
	if res.deduped != 1 {
		t.Fatalf("want deduped=1, got %+v", res)
	}
	if got := collectTree(t, dstGit); !eqMap(targetBefore, got) {
		t.Error("target .git must be untouched on dedupe")
	}
	if exists(filepath.Join(dstRepo, ".git-1")) {
		t.Error("no .git-1 should be created on identical dedupe")
	}
}

// Different target .git: source lands at .git-1, both intact.
func TestGit_DifferentConflict_Suffixed(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcRepo := filepath.Join(root, "src", "repo")
	dstRepo := filepath.Join(root, "dst", "repo")
	srcGit := filepath.Join(srcRepo, ".git")
	dstGit := filepath.Join(dstRepo, ".git")

	srcFiles := defaultGitFiles()
	srcFiles["HEAD"] = "ref: refs/heads/feature\n" // differ from target
	makeGitDir(t, srcGit, srcFiles)
	makeGitDir(t, dstGit, defaultGitFiles())

	srcBefore := collectTree(t, srcGit)
	dstBefore := collectTree(t, dstGit)

	var res moveResult
	if err := moveSingleItem(srcRepo, dstRepo, database, []string{root}, &res); err != nil {
		t.Fatalf("moveSingleItem: %v", err)
	}

	git1 := filepath.Join(dstRepo, ".git-1")
	if !exists(git1) {
		t.Fatal(".git-1 not created on content conflict")
	}
	if exists(srcGit) {
		t.Error("source .git should be gone after suffixed move")
	}
	if res.renamed != 1 {
		t.Fatalf("want renamed=1, got %+v", res)
	}
	// Target .git unchanged; .git-1 equals the source tree.
	if got := collectTree(t, dstGit); !eqMap(dstBefore, got) {
		t.Error("target .git must be unchanged")
	}
	if got := collectTree(t, git1); !eqMap(srcBefore, got) {
		t.Error(".git-1 must equal the moved source tree")
	}
}

// A second conflicting move lands at .git-2 (deterministic increment).
func TestGit_SecondConflict_Increments(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	dstRepo := filepath.Join(root, "dst", "repo")
	dstGit := filepath.Join(dstRepo, ".git")
	makeGitDir(t, dstGit, defaultGitFiles())
	// Pre-existing .git-1 (from a prior move).
	makeGitDir(t, filepath.Join(dstRepo, ".git-1"), defaultGitFiles())

	// New source with yet different content.
	srcRepo := filepath.Join(root, "src", "repo")
	srcGit := filepath.Join(srcRepo, ".git")
	sf := defaultGitFiles()
	sf["HEAD"] = "ref: refs/heads/other\n"
	makeGitDir(t, srcGit, sf)

	var res moveResult
	if err := moveSingleItem(srcRepo, dstRepo, database, []string{root}, &res); err != nil {
		t.Fatalf("moveSingleItem: %v", err)
	}
	if !exists(filepath.Join(dstRepo, ".git-2")) {
		t.Error(".git-2 should be created when .git and .git-1 both exist")
	}
}

// dirContentHash: identical trees equal, one differing file differs.
func TestDirContentHash(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	c := filepath.Join(root, "c")
	makeGitDir(t, a, defaultGitFiles())
	makeGitDir(t, b, defaultGitFiles())
	cf := defaultGitFiles()
	cf["config"] = "[core]\n\tbare = true\n"
	makeGitDir(t, c, cf)

	ha, err := dirContentHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := dirContentHash(b)
	hc, _ := dirContentHash(c)
	if ha != hb {
		t.Error("identical trees must hash equal")
	}
	if ha == hc {
		t.Error("trees differing by one file must hash differently")
	}
}

// Regression: an ordinary (non-atomic) directory with the same name at target still merges
// file-by-file. "docs" is not in atomicDirNames.
func TestOrdinaryDir_StillPerFileMerge(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcRepo := filepath.Join(root, "src")
	dstRepo := filepath.Join(root, "dst")
	// docs present on both sides with a differing file.
	writeFile(t, filepath.Join(srcRepo, "docs", "pkg", "a.txt"), "SRC")
	writeFile(t, filepath.Join(dstRepo, "docs", "pkg", "a.txt"), "DST")

	var res moveResult
	if err := moveSingleItem(srcRepo, dstRepo, database, []string{root}, &res); err != nil {
		t.Fatalf("moveSingleItem: %v", err)
	}
	// Per-file conflict resolution: different hash → source tagged with -<hash>, no <name>-N logic.
	entries := collectTree(t, filepath.Join(dstRepo, "docs", "pkg"))
	if entries["a.txt"] != "DST" {
		t.Error("target docs file must be preserved")
	}
	// Exactly one tagged sibling should appear (the moved source).
	tagged := 0
	for name := range entries {
		if name != "a.txt" {
			tagged++
		}
	}
	if tagged != 1 {
		t.Errorf("expected 1 content-tagged sibling in docs, got %d: %v", tagged, entries)
	}
}

// node_modules (and the other default-exclude directories) are now handled atomically,
// exactly like .git: a differing target yields node_modules-1, not a per-file merge.
func TestAtomicDir_NodeModules(t *testing.T) {
	root := t.TempDir()
	database := openMoveTestDB(t)

	srcRepo := filepath.Join(root, "src")
	dstRepo := filepath.Join(root, "dst")
	writeFile(t, filepath.Join(srcRepo, "node_modules", "pkg", "a.js"), "SRC")
	writeFile(t, filepath.Join(dstRepo, "node_modules", "pkg", "a.js"), "DST")

	srcBefore := collectTree(t, filepath.Join(srcRepo, "node_modules"))

	var res moveResult
	if err := moveSingleItem(srcRepo, dstRepo, database, []string{root}, &res); err != nil {
		t.Fatalf("moveSingleItem: %v", err)
	}

	nm1 := filepath.Join(dstRepo, "node_modules-1")
	if !exists(nm1) {
		t.Fatal("node_modules-1 not created on atomic conflict")
	}
	if res.renamed != 1 {
		t.Fatalf("want renamed=1 (atomic), got %+v", res)
	}
	// Target node_modules untouched; the moved source is intact under node_modules-1.
	if b, _ := os.ReadFile(filepath.Join(dstRepo, "node_modules", "pkg", "a.js")); string(b) != "DST" {
		t.Error("target node_modules must be unchanged")
	}
	if got := collectTree(t, nm1); !eqMap(srcBefore, got) {
		t.Error("node_modules-1 must equal the moved source tree")
	}
	// No per-file content-tagged sibling should have been created inside the target tree.
	for name := range collectTree(t, filepath.Join(dstRepo, "node_modules", "pkg")) {
		if name != "a.js" {
			t.Errorf("unexpected per-file artifact inside atomic node_modules: %s", name)
		}
	}
}
