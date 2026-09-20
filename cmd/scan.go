package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/yhu/fs-hash-util/internal/db"
	"github.com/yhu/fs-hash-util/internal/hasher"
	"github.com/yhu/fs-hash-util/internal/scanner"
)

const batchSize = 500

var (
	excludePatterns   []string
	noDefaultExcludes bool
	concurrency       int
	prune             bool
)

func init() {
	scanCmd.Flags().StringArrayVar(&excludePatterns, "exclude", nil, "additional regex patterns to exclude (repeatable)")
	scanCmd.Flags().BoolVar(&noDefaultExcludes, "no-default-excludes", false, "disable built-in default exclude patterns")
	scanCmd.Flags().IntVar(&concurrency, "concurrency", 8, "number of concurrent hash workers")
	scanCmd.Flags().BoolVar(&prune, "prune", false, "remove DB records for files no longer present (only on successful scan)")
	rootCmd.AddCommand(scanCmd)
}

var scanCmd = &cobra.Command{
	Use:   "scan <dir>",
	Short: "Scan a directory and store file hashes in the database",
	Args:  cobra.ExactArgs(1),
	RunE:  runScan,
}

func runScan(cmd *cobra.Command, args []string) error {
	dir := args[0]

	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", dir, err)
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		root = abs
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	dbAbs, err := filepath.Abs(dbPath)
	if err != nil {
		return err
	}
	dbAbs = filepath.Clean(dbAbs)
	// Resolve symlinks so the path matches what WalkDir will report inside a canonical root.
	if resolved, rErr := filepath.EvalSymlinks(filepath.Dir(dbAbs)); rErr == nil {
		dbAbs = filepath.Join(resolved, filepath.Base(dbAbs))
	}
	absExcludes := []string{dbAbs, dbAbs + "-wal", dbAbs + "-shm"}

	matcher, err := scanner.NewMatcher(excludePatterns, noDefaultExcludes, absExcludes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	scanID, err := database.CreateScan(root, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("create scan record: %w", err)
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	opts := scanner.ScanOpts{
		Matcher:     matcher,
		Database:    database,
		ScanID:      scanID,
		Concurrency: concurrency,
	}

	jobs, walkErrCh := scanner.Walk(ctx, root, opts)

	results := make(chan scanResult, concurrency*2)

	eg, egCtx := errgroup.WithContext(ctx)
	for i := 0; i < concurrency; i++ {
		eg.Go(func() error {
			for j := range jobs {
				select {
				case <-egCtx.Done():
					return egCtx.Err()
				default:
				}
				switch job := j.(type) {
				case scanner.TouchJob:
					results <- scanResult{touch: &job}
				case scanner.HashJob:
					h, hashErr := hasher.HashFile(job.AbsPath, job.Stat.Size())
					if hashErr != nil {
						fmt.Fprintf(os.Stderr, "hash error %s: %v\n", job.AbsPath, hashErr)
						results <- scanResult{isErr: true}
						continue
					}
					results <- scanResult{hashJob: &job, hash: h}
				}
			}
			return nil
		})
	}

	go func() {
		eg.Wait()
		close(results)
	}()

	// DB writer: main goroutine, single writer.
	var (
		totalFiles int64
		hashed     int64
		skipped    int64
		errors     int64
		pending    int
	)

	tx, err := database.Begin()
	if err != nil {
		return err
	}

	flushTx := func() error {
		if pending > 0 {
			if cErr := tx.Commit(); cErr != nil {
				_ = tx.Rollback()
				return cErr
			}
		} else {
			_ = tx.Rollback()
		}
		pending = 0
		var tErr error
		tx, tErr = database.Begin()
		return tErr
	}

	for res := range results {
		totalFiles++
		if res.isErr {
			errors++
			continue
		}
		if res.touch != nil {
			skipped++
			if tErr := database.TouchScanID(tx, res.touch.RecordID, res.touch.ScanID); tErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("touch scan_id: %w", tErr)
			}
		} else {
			hashed++
			rec := db.FileRecord{
				ScanRoot:   root,
				FileName:   res.hashJob.FileName,
				RelPath:    res.hashJob.RelPath,
				BirthTS:    res.hashJob.BirthTS,
				ModifiedNS: res.hashJob.Stat.ModTime().UnixNano(),
				Size:       res.hashJob.Stat.Size(),
				Hash:       res.hash,
				ScanID:     res.hashJob.ScanID,
			}
			if uErr := database.Upsert(tx, rec); uErr != nil {
				_ = tx.Rollback()
				return fmt.Errorf("upsert: %w", uErr)
			}
		}
		pending++
		if pending >= batchSize {
			if fErr := flushTx(); fErr != nil {
				return fErr
			}
		}
	}

	// Commit remaining (or rollback empty tx).
	if pending > 0 {
		if cErr := tx.Commit(); cErr != nil {
			_ = tx.Rollback()
			return cErr
		}
	} else {
		_ = tx.Rollback()
	}

	// Check for walk-level error; abort without finishing scan.
	walkErr := <-walkErrCh
	if walkErr != nil {
		fmt.Fprintf(os.Stderr, "scan aborted: %v\n", walkErr)
		return walkErr
	}

	if err := database.FinishScan(scanID, time.Now().UnixNano()); err != nil {
		return err
	}

	var pruned int64
	if prune {
		pruned, err = database.DeleteNotSeen(root, scanID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "prune error: %v\n", err)
		}
	}

	fmt.Printf("Scan complete: root=%s  db=%s  scan_id=%d\n", root, dbPath, scanID)
	fmt.Printf("  total=%d  hashed=%d  skipped=%d  errors=%d", totalFiles, hashed, skipped, errors)
	if prune {
		fmt.Printf("  pruned=%d", pruned)
	}
	fmt.Println()

	if errors > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d file(s) could not be hashed\n", errors)
	}
	return nil
}

type scanResult struct {
	touch   *scanner.TouchJob
	hashJob *scanner.HashJob
	hash    string
	isErr   bool
}
