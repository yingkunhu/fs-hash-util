package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const apiURL = "https://api.github.com/repos/yingkunhu/fs-hash-util/releases/latest"

// Release holds the fields we need from the GitHub releases API.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset represents a single release artifact.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// CheckLatest fetches the latest release from GitHub. Returns an error when the
// network is unavailable or the response cannot be decoded; callers should treat
// any error as "skip upgrade check".
func CheckLatest() (*Release, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, apiURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API: HTTP %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// IsNewer reports whether the release tag (e.g. "v1.1.0") is a higher version
// than current (e.g. "1.0.0"). Returns false on any parse failure.
func IsNewer(tag, current string) bool {
	a := strings.TrimPrefix(tag, "v")
	return a != current && cmpSemver(a, current) > 0
}

// AssetURL returns the browser download URL of the binary matching the current
// OS/arch (e.g. "fshash-darwin-arm64").
func AssetURL(rel *Release) (string, error) {
	name := fmt.Sprintf("fshash-%s-%s", runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("no release asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// Upgrade downloads the binary at downloadURL, replaces the running executable
// with it (renamed to "fshash" in the same directory), verifies the new binary
// with --version, and removes the backup.
//
// On any failure the original binary is restored from backup before returning
// the error.
func Upgrade(downloadURL string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}
	exeDir := filepath.Dir(exePath)

	// Download into a temp file on the same filesystem as the binary so that
	// os.Rename is atomic (no cross-device copy).
	tmp, err := os.CreateTemp(exeDir, ".fshash-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Always remove the temp file on early return; ignored after successful rename.
	defer os.Remove(tmpPath)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(downloadURL)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write download: %w", err)
	}
	tmp.Close()

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Back up the current binary.
	backupPath := exePath + ".bak"
	if err := os.Rename(exePath, backupPath); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}

	// Install new binary as "fshash" in the same directory.
	newPath := filepath.Join(exeDir, "fshash")
	if err := os.Rename(tmpPath, newPath); err != nil {
		_ = os.Rename(backupPath, exePath) // restore
		return fmt.Errorf("install new binary: %w", err)
	}

	// Verify the new binary.
	out, err := exec.Command(newPath, "--version").CombinedOutput()
	if err != nil {
		_ = os.Rename(backupPath, exePath) // restore
		_ = os.Remove(newPath)
		return fmt.Errorf("version check failed: %w\n%s", err, out)
	}
	fmt.Printf("Verified: %s", out)

	// Remove backup only after a successful verification.
	_ = os.Remove(backupPath)
	return nil
}

// --- semver helpers ---

func cmpSemver(a, b string) int {
	pa, pb := parseSemver(a), parseSemver(b)
	for i := range pa {
		if pa[i] > pb[i] {
			return 1
		}
		if pa[i] < pb[i] {
			return -1
		}
	}
	return 0
}

func parseSemver(v string) [3]int {
	var major, minor, patch int
	fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch)
	return [3]int{major, minor, patch}
}
