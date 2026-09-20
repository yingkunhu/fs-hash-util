package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yhu/fs-hash-util/internal/db"
	"github.com/yhu/fs-hash-util/internal/hasher"
	"github.com/yhu/fs-hash-util/internal/scanner"
)

// buildBinary compiles fshash into a temp dir and returns the binary path.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fshash")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// makeTree creates a small file tree in dir.
func makeTree(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"a.txt":          "hello world",
		"sub/b.txt":      "sub file b",
		"sub/empty.txt":  "",
		"node_modules/c": "should be excluded",
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanBasic(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	makeTree(t, dir)
	dbPath := filepath.Join(dir, "fshash.db")

	out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("scan failed: %v\n%s", err, out)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	recs, err := database.List("", 100, 0)
	if err != nil {
		t.Fatal(err)
	}

	// node_modules should be excluded; expect exactly 3 files.
	if len(recs) != 3 {
		t.Fatalf("expected 3 records, got %d: %v", len(recs), recPaths(recs))
	}

	// Verify empty file uses EmptyFileHash.
	for _, r := range recs {
		if r.FileName == "empty.txt" {
			if r.Hash != hasher.EmptyFileHash {
				t.Errorf("empty file hash wrong: %s", r.Hash)
			}
			if r.Size != 0 {
				t.Errorf("empty file size wrong: %d", r.Size)
			}
		}
	}
}

func TestScanIncremental(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	makeTree(t, dir)
	dbPath := filepath.Join(dir, "fshash.db")

	// First scan.
	if out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput(); err != nil {
		t.Fatalf("scan 1 failed: %v\n%s", err, out)
	}

	// Second scan without changes — expect skipped == total.
	out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("scan 2 failed: %v\n%s", err, out)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "hashed=0") {
		t.Errorf("expected hashed=0 on re-scan of unchanged tree, got:\n%s", outStr)
	}
}

func TestScanDetectsChange(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	makeTree(t, dir)
	dbPath := filepath.Join(dir, "fshash.db")

	if out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput(); err != nil {
		t.Fatalf("scan 1 failed: %v\n%s", err, out)
	}

	// Modify a.txt and ensure mtime advances (at least 1 ns).
	aPath := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(aPath, []byte("modified content"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force mtime to be different even in sub-nanosecond environments.
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(aPath, future, future); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("scan 2 failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "hashed=1") {
		t.Errorf("expected hashed=1 after modifying one file, got:\n%s", string(out))
	}
}

func TestScanPrune(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	makeTree(t, dir)
	dbPath := filepath.Join(dir, "fshash.db")

	if out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput(); err != nil {
		t.Fatalf("scan 1 failed: %v\n%s", err, out)
	}

	// Delete sub/b.txt and re-scan with --prune.
	if err := os.Remove(filepath.Join(dir, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "--db", dbPath, "scan", "--prune", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("scan 2 (prune) failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pruned=1") {
		t.Errorf("expected pruned=1, got:\n%s", string(out))
	}

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	recs, _ := database.List("", 100, 0)
	for _, r := range recs {
		if r.RelPath == "sub/b.txt" {
			t.Error("sub/b.txt should have been pruned from DB")
		}
	}
}

func TestScanDBFileNotIndexed(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	makeTree(t, dir)
	// Place DB inside the scanned directory.
	dbPath := filepath.Join(dir, "fshash.db")

	if out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput(); err != nil {
		t.Fatalf("scan failed: %v\n%s", err, out)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	recs, _ := database.List("", 100, 0)
	for _, r := range recs {
		if strings.HasSuffix(r.RelPath, ".db") || strings.HasSuffix(r.RelPath, ".db-wal") || strings.HasSuffix(r.RelPath, ".db-shm") {
			t.Errorf("DB file should not be indexed, found: %s", r.RelPath)
		}
	}
}

func TestScanNanosecondMtime(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fshash.db")

	// Write a file and scan.
	fPath := filepath.Join(dir, "nano.txt")
	if err := os.WriteFile(fPath, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Set mtime to a specific nanosecond.
	t1 := time.Date(2024, 1, 1, 0, 0, 0, 123456789, time.UTC)
	if err := os.Chtimes(fPath, t1, t1); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput(); err != nil {
		t.Fatalf("scan 1: %v\n%s", err, out)
	}

	// Change content but keep same-second mtime, different nanoseconds.
	t2 := t1.Add(500 * time.Millisecond)
	if err := os.WriteFile(fPath, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fPath, t2, t2); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("scan 2: %v\n%s", err, out)
	}
	// Sub-second change must trigger re-hash.
	if !strings.Contains(string(out), "hashed=1") {
		t.Errorf("expected re-hash for sub-second mtime change, got:\n%s", string(out))
	}
}

func TestQuery(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	makeTree(t, dir)
	dbPath := filepath.Join(dir, "fshash.db")

	if out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput(); err != nil {
		t.Fatalf("scan failed: %v\n%s", err, out)
	}

	// Query by path.
	out, err := exec.Command(bin, "--db", dbPath, "query", "--path", "a.txt").CombinedOutput()
	if err != nil {
		t.Fatalf("query --path failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "a.txt") {
		t.Errorf("expected a.txt in query output, got:\n%s", string(out))
	}
}

func TestStats(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	makeTree(t, dir)
	dbPath := filepath.Join(dir, "fshash.db")

	if out, err := exec.Command(bin, "--db", dbPath, "scan", dir).CombinedOutput(); err != nil {
		t.Fatalf("scan failed: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--db", dbPath, "stats").CombinedOutput()
	if err != nil {
		t.Fatalf("stats failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Files") {
		t.Errorf("expected 'Files' in stats output, got:\n%s", string(out))
	}
}

func recPaths(recs []db.FileRecord) []string {
	var paths []string
	for _, r := range recs {
		paths = append(paths, r.RelPath)
	}
	return paths
}

// avoid unused import warning
var _ = scanner.DefaultExcludes
