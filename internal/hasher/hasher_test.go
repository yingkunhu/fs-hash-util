package hasher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHashFile_empty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := HashFile(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != EmptyFileHash {
		t.Fatalf("empty file hash: got %s, want %s", got, EmptyFileHash)
	}
}

func TestHashFile_content(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hello.txt")
	content := []byte("hello world\n")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := HashFile(p, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello world\n")
	const want = "a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447"
	if got != want {
		t.Fatalf("content hash: got %s, want %s", got, want)
	}
}

func TestHashFile_notfound(t *testing.T) {
	_, err := HashFile("/nonexistent/path/file.txt", 42)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
