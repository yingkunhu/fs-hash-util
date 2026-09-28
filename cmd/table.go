package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/yhu/fs-hash-util/internal/db"
)

type jsonRecord struct {
	ID         int64   `json:"id"`
	ScanRoot   string  `json:"scan_root"`
	FileName   string  `json:"file_name"`
	RelPath    string  `json:"rel_path"`
	BirthTS    *int64  `json:"birth_ts"`
	ModifiedNS int64   `json:"modified_ns"`
	ModifiedAt string  `json:"modified_at"`
	Size       int64   `json:"size"`
	Hash       string  `json:"hash"`
	ScanID     int64   `json:"scan_id"`
}

func printFileTable(recs []db.FileRecord) {
	if jsonOut {
		printJSON(recs)
		return
	}
	if len(recs) == 0 {
		return
	}
	fmt.Printf("%-12s %-64s %10s  %-64s  %s\n", "ID", "REL_PATH", "SIZE", "HASH", "MODIFIED")
	fmt.Printf("%-12s %-64s %10s  %-64s  %s\n", "---", "---", "---", "---", "---")
	for _, r := range recs {
		mtime := time.Unix(0, r.ModifiedNS).Format("2006-01-02T15:04:05")
		fmt.Printf("%-12d %-64s %10d  %s  %s\n", r.ID, r.RelPath, r.Size, r.Hash, mtime)
	}
}

func printJSON(recs []db.FileRecord) {
	out := make([]jsonRecord, len(recs))
	for i, r := range recs {
		out[i] = jsonRecord{
			ID:         r.ID,
			ScanRoot:   r.ScanRoot,
			FileName:   r.FileName,
			RelPath:    r.RelPath,
			BirthTS:    r.BirthTS,
			ModifiedNS: r.ModifiedNS,
			ModifiedAt: time.Unix(0, r.ModifiedNS).UTC().Format(time.RFC3339Nano),
			Size:       r.Size,
			Hash:       r.Hash,
			ScanID:     r.ScanID,
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep non-ASCII chars as-is
	_ = enc.Encode(out)
}

// humanSize formats a byte count as a human-readable string (e.g. "1.2 MB", "456 KB").
func humanSize(n int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
