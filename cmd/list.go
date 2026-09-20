package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yhu/fs-hash-util/internal/db"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List file records from the database",
	RunE:  runList,
}

var (
	listRoot   string
	listLimit  int
	listOffset int
)

func init() {
	listCmd.Flags().StringVar(&listRoot, "root", "", "filter by scan root directory")
	listCmd.Flags().IntVar(&listLimit, "limit", 50, "maximum number of records to show")
	listCmd.Flags().IntVar(&listOffset, "offset", 0, "offset for pagination")
	rootCmd.AddCommand(listCmd)
}

func runList(_ *cobra.Command, _ []string) error {
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	recs, err := database.List(listRoot, listLimit, listOffset)
	if err != nil {
		return err
	}

	printFileTable(recs)
	if !jsonOut {
		fmt.Printf("(%d records)\n", len(recs))
	}
	return nil
}
