package db

import (
	"database/sql"
	"fmt"
)

// CreateScan inserts a new scan record and returns the generated scan ID.
func (d *DB) CreateScan(scanRoot string, startedAt int64) (int64, error) {
	res, err := d.Exec(
		`INSERT INTO scans(scan_root, started_at) VALUES(?, ?)`,
		scanRoot, startedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("create scan: %w", err)
	}
	return res.LastInsertId()
}

// FinishScan sets finished_at for the given scan ID.
func (d *DB) FinishScan(scanID int64, finishedAt int64) error {
	_, err := d.Exec(
		`UPDATE scans SET finished_at=? WHERE id=?`,
		finishedAt, scanID,
	)
	return err
}

// GetByRelPath fetches an existing FileRecord by (scanRoot, relPath).
// Returns (record, true, nil) if found, (nil, false, nil) if not found.
func (d *DB) GetByRelPath(scanRoot, relPath string) (*FileRecord, bool, error) {
	row := d.QueryRow(
		`SELECT id, scan_root, file_name, rel_path, birth_ts, modified_ns, size, hash, scan_id
		 FROM files WHERE scan_root=? AND rel_path=?`,
		scanRoot, relPath,
	)
	rec, err := scanFileRow(row)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// Upsert inserts or updates a FileRecord.
func (d *DB) Upsert(tx *sql.Tx, rec FileRecord) error {
	_, err := tx.Exec(
		`INSERT INTO files(scan_root, file_name, rel_path, birth_ts, modified_ns, size, hash, scan_id)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(scan_root, rel_path) DO UPDATE SET
		   file_name=excluded.file_name,
		   birth_ts=excluded.birth_ts,
		   modified_ns=excluded.modified_ns,
		   size=excluded.size,
		   hash=excluded.hash,
		   scan_id=excluded.scan_id`,
		rec.ScanRoot, rec.FileName, rec.RelPath, rec.BirthTS,
		rec.ModifiedNS, rec.Size, rec.Hash, rec.ScanID,
	)
	return err
}

// TouchScanID updates scan_id for an existing record (incremental skip path).
func (d *DB) TouchScanID(tx *sql.Tx, id, scanID int64) error {
	_, err := tx.Exec(`UPDATE files SET scan_id=? WHERE id=?`, scanID, id)
	return err
}

// List returns up to limit records for the given scanRoot, ordered by rel_path.
// If scanRoot is empty, all roots are returned.
func (d *DB) List(scanRoot string, limit, offset int) ([]FileRecord, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if scanRoot == "" {
		rows, err = d.Query(
			`SELECT id, scan_root, file_name, rel_path, birth_ts, modified_ns, size, hash, scan_id
			 FROM files ORDER BY rel_path LIMIT ? OFFSET ?`,
			limit, offset,
		)
	} else {
		rows, err = d.Query(
			`SELECT id, scan_root, file_name, rel_path, birth_ts, modified_ns, size, hash, scan_id
			 FROM files WHERE scan_root=? ORDER BY rel_path LIMIT ? OFFSET ?`,
			scanRoot, limit, offset,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFileRows(rows)
}

// QueryByHash returns all records matching the given exact SHA-256 hash.
func (d *DB) QueryByHash(hash string) ([]FileRecord, error) {
	rows, err := d.Query(
		`SELECT id, scan_root, file_name, rel_path, birth_ts, modified_ns, size, hash, scan_id
		 FROM files WHERE hash=?`,
		hash,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFileRows(rows)
}

// QueryByNamePattern searches records whose file_name or rel_path matches a SQL LIKE pattern.
// Use % as wildcard (or pass a string with * which is auto-converted to %).
func (d *DB) QueryByNamePattern(pattern string) ([]FileRecord, error) {
	rows, err := d.Query(
		`SELECT id, scan_root, file_name, rel_path, birth_ts, modified_ns, size, hash, scan_id
		 FROM files WHERE file_name LIKE ? OR rel_path LIKE ?`,
		pattern, pattern,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFileRows(rows)
}

// QueryByRelPath returns all records matching the exact relative path.
func (d *DB) QueryByRelPath(relPath string) ([]FileRecord, error) {
	rows, err := d.Query(
		`SELECT id, scan_root, file_name, rel_path, birth_ts, modified_ns, size, hash, scan_id
		 FROM files WHERE rel_path=?`,
		relPath,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectFileRows(rows)
}

// DeleteNotSeen removes records for the given scanRoot whose scan_id is strictly less than scanID.
// Must only be called after FinishScan.
func (d *DB) DeleteNotSeen(scanRoot string, scanID int64) (int64, error) {
	res, err := d.Exec(
		`DELETE FROM files WHERE scan_root=? AND scan_id < ?`,
		scanRoot, scanID,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Stats returns aggregate statistics.
func (d *DB) Stats(scanRoot string) (total int64, totalSize int64, dupCount int64, err error) {
	var base string
	var args []any
	if scanRoot != "" {
		base = "WHERE scan_root=?"
		args = []any{scanRoot}
	}

	row := d.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM files `+base, args...)
	if err = row.Scan(&total, &totalSize); err != nil {
		return
	}

	row = d.QueryRow(
		`SELECT COUNT(*) FROM (
		   SELECT hash FROM files `+base+` GROUP BY hash HAVING COUNT(*)>1
		 )`,
		args...,
	)
	err = row.Scan(&dupCount)
	return
}

// --- helpers ---

func scanFileRow(row *sql.Row) (*FileRecord, error) {
	var rec FileRecord
	err := row.Scan(
		&rec.ID, &rec.ScanRoot, &rec.FileName, &rec.RelPath,
		&rec.BirthTS, &rec.ModifiedNS, &rec.Size, &rec.Hash, &rec.ScanID,
	)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func collectFileRows(rows *sql.Rows) ([]FileRecord, error) {
	var out []FileRecord
	for rows.Next() {
		var rec FileRecord
		if err := rows.Scan(
			&rec.ID, &rec.ScanRoot, &rec.FileName, &rec.RelPath,
			&rec.BirthTS, &rec.ModifiedNS, &rec.Size, &rec.Hash, &rec.ScanID,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
