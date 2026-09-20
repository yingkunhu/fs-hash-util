package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yhu/fs-hash-util/internal/db"
)

var queryCmd = &cobra.Command{
	Use:   "query",
	Short: "Query file records by hash, relative path, or filename pattern",
	RunE:  runQuery,
}

var (
	queryHash string
	queryPath string
	queryName string
)

func init() {
	queryCmd.Flags().StringVar(&queryHash, "hash", "", "exact SHA-256 hash to look up")
	queryCmd.Flags().StringVar(&queryPath, "path", "", "exact relative path (/-normalized) to look up")
	queryCmd.Flags().StringVar(&queryName, "name", "", "filename/path pattern (* wildcard, e.g. '*汉化组*')")
	queryCmd.MarkFlagsOneRequired("hash", "path", "name")
	rootCmd.AddCommand(queryCmd)
}

func runQuery(_ *cobra.Command, _ []string) error {
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	var recs []db.FileRecord
	switch {
	case queryHash != "":
		recs, err = database.QueryByHash(queryHash)
	case queryPath != "":
		recs, err = database.QueryByRelPath(queryPath)
	default:
		// Convert * wildcard to SQL LIKE %
		likePattern := strings.ReplaceAll(queryName, "*", "%")
		recs, err = database.QueryByNamePattern(likePattern)
	}
	if err != nil {
		return err
	}

	if len(recs) == 0 {
		fmt.Fprintln(os.Stderr, "no records found")
		return nil
	}

	printFileTable(recs)
	if !jsonOut {
		fmt.Printf("(%d records)\n", len(recs))
	}
	return nil
}
