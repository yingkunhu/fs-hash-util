package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/yhu/fs-hash-util/internal/updater"
)

// checkAndOfferUpgrade queries GitHub for the latest release. If a newer version
// exists it prompts the user; on confirmation it downloads, installs, and exits.
// Any network error is silently ignored so startup is never blocked.
func checkAndOfferUpgrade() {
	rel, err := updater.CheckLatest()
	if err != nil {
		return // network unavailable or API error — skip silently
	}
	if !updater.IsNewer(rel.TagName, version) {
		return
	}

	newVer := strings.TrimPrefix(rel.TagName, "v")
	fmt.Printf("New version available: %s (current: %s)\n", newVer, version)
	fmt.Print("Upgrade now? [y/N] ")

	var answer string
	fmt.Scanln(&answer)
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return
	}

	assetURL, err := updater.AssetURL(rel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "upgrade: %v\n", err)
		return
	}

	fmt.Println("Downloading...")
	if err := updater.Upgrade(assetURL); err != nil {
		fmt.Fprintf(os.Stderr, "upgrade failed: %v\n", err)
		return
	}

	fmt.Println("Upgrade complete. Please re-run the command.")
	os.Exit(0)
}
