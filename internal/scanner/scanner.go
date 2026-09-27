package scanner

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/yhu/fs-hash-util/internal/db"
)

// Job is either a HashJob (file needs hashing) or a TouchJob (file unchanged, only scan_id update).
type Job interface{ jobKind() }

type HashJob struct {
	AbsPath    string
	RelPath    string
	FileName   string
	Stat       os.FileInfo
	ScanID     int64
	BirthTS    *int64
}

type TouchJob struct {
	RecordID int64
	ScanID   int64
}

func (HashJob) jobKind()  {}
func (TouchJob) jobKind() {}

// ScanOpts controls scanner behavior.
type ScanOpts struct {
	Matcher     *Matcher
	Database    *db.DB
	ScanID      int64
	Concurrency int
	OnFolder    func(relPath string) // called when a non-root directory is entered; nil = no-op
}

// Walk traverses root and sends Jobs to the returned channel.
// The channel is closed when the walk finishes (or ctx is cancelled).
// Any walk-level error is returned after the channel is closed.
func Walk(ctx context.Context, root string, opts ScanOpts) (<-chan Job, <-chan error) {
	jobs := make(chan Job, opts.Concurrency*2)
	errCh := make(chan error, 1)

	go func() {
		defer close(jobs)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				if path == root {
					return werr // fatal: can't read scan root
				}
				// non-root walk error: skip this entry, let scan continue
				return nil
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			absPath := filepath.Clean(path)

			// Absolute-path exclusion (DB files etc.)
			if opts.Matcher.ShouldSkipAbs(absPath) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			// Relative-path exclusion
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)

			if opts.Matcher.ShouldSkipRel(relSlash) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if d.IsDir() {
				if opts.OnFolder != nil && relSlash != "." {
					opts.OnFolder(relSlash)
				}
				return nil // descend
			}

			// Only process regular files
			if !d.Type().IsRegular() {
				return nil
			}

			fi, err := os.Lstat(path)
			if err != nil {
				return nil // non-fatal: skip unreadable file
			}

			existing, found, err := opts.Database.GetByRelPath(root, relSlash)
			if err != nil {
				return nil // non-fatal
			}

			if found && existing.Size == fi.Size() && existing.ModifiedNS == fi.ModTime().UnixNano() {
				select {
				case jobs <- TouchJob{RecordID: existing.ID, ScanID: opts.ScanID}:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}

			job := HashJob{
				AbsPath:  absPath,
				RelPath:  relSlash,
				FileName: d.Name(),
				Stat:     fi,
				ScanID:   opts.ScanID,
				BirthTS:  birthTimeNS(fi),
			}
			select {
			case jobs <- job:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
		if err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	return jobs, errCh
}
