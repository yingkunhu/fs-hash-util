package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yhu/fs-hash-util/internal/db"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Print summary statistics from the database",
	RunE:  runStats,
}

var statsRoot string

func init() {
	statsCmd.Flags().StringVar(&statsRoot, "root", "", "filter by scan root directory")
	rootCmd.AddCommand(statsCmd)
}

func runStats(_ *cobra.Command, _ []string) error {
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	total, totalSize, dupCount, err := database.Stats(statsRoot)
	if err != nil {
		return err
	}

	fmt.Printf("Files    : %d\n", total)
	fmt.Printf("Total    : %d bytes\n", totalSize)
	fmt.Printf("Dup hash : %d groups\n", dupCount)
	return nil
}
