package hasher

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// EmptyFileHash is the SHA-256 of an empty byte sequence.
const EmptyFileHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// HashFile computes the SHA-256 hash of the file at path and returns its hex string.
// For empty files (size == 0) it returns EmptyFileHash directly without opening the file.
func HashFile(path string, size int64) (string, error) {
	if size == 0 {
		return EmptyFileHash, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
