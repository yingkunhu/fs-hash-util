package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	dbPath           string
	verbose          bool
	jsonOut          bool
	version          string
	skipUpgradeCheck bool
)

var rootCmd = &cobra.Command{
	Use:   "fshash",
	Short: "Local file hash utility — scan, store, query SHA-256 hashes",
	Long:  `fshash scans directories, computes SHA-256 hashes, and maintains results in a SQLite database.`,
}

func SetVersion(v string) {
	version = v
	rootCmd.Version = v
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.PersistentFlags().StringVar(&dbPath, "db", "fshash.db", "path to SQLite database file")
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "enable verbose output")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	rootCmd.PersistentFlags().BoolVar(&skipUpgradeCheck, "skip-upgrade-check", false, "skip the new-version check on startup")
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if !skipUpgradeCheck {
			checkAndOfferUpgrade()
		}
		return nil
	}
}
