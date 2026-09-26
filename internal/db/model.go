package db

// FileRecord represents a row in the files table.
type FileRecord struct {
	ID         int64
	ScanRoot   string
	FileName   string
	RelPath    string
	BirthTS    *int64 // NULL when platform cannot provide birth time
	ModifiedNS int64
	Size       int64
	Hash       string
	ScanID     int64
}

// ScanRecord represents a row in the scans table.
type ScanRecord struct {
	ID         int64
	ScanRoot   string
	StartedAt  int64
	FinishedAt *int64 // NULL = in-progress or failed
}

// FolderRecord represents a row in the folders table.
// Hash is the SHA-256 of sorted direct-children content hashes (file hashes + sub-folder hashes).
// File names, attributes, and timestamps are excluded from the hash.
type FolderRecord struct {
	ID       int64
	ScanRoot string
	RelPath  string
	Hash     string
	ScanID   int64
}
