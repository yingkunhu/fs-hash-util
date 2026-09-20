package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schemaVersion = "1"

const ddl = `
CREATE TABLE IF NOT EXISTS scans (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_root   TEXT    NOT NULL,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER
);

CREATE TABLE IF NOT EXISTS files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_root    TEXT    NOT NULL,
    file_name    TEXT    NOT NULL,
    rel_path     TEXT    NOT NULL,
    birth_ts     INTEGER,
    modified_ns  INTEGER NOT NULL,
    size         INTEGER NOT NULL,
    hash         TEXT    NOT NULL,
    scan_id      INTEGER NOT NULL REFERENCES scans(id),
    UNIQUE(scan_root, rel_path)
);

CREATE INDEX IF NOT EXISTS idx_files_hash    ON files(hash);
CREATE INDEX IF NOT EXISTS idx_files_relpath ON files(rel_path);
CREATE INDEX IF NOT EXISTS idx_files_scan_id ON files(scan_id);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// DB wraps *sql.DB with helpers.
type DB struct {
	*sql.DB
}

// Open opens (or creates) the SQLite database at path, runs migrations, and returns a DB.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_foreign_keys=ON",
		path,
	)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqldb.SetMaxOpenConns(4) // WAL mode allows concurrent reads; PRAGMAs set via DSN per-connection
	sqldb.SetMaxIdleConns(4)

	if err := migrate(sqldb); err != nil {
		sqldb.Close()
		return nil, err
	}
	return &DB{sqldb}, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	_, err := db.Exec(
		`INSERT INTO meta(key, value) VALUES('schema_version', ?) ON CONFLICT(key) DO NOTHING`,
		schemaVersion,
	)
	return err
}
