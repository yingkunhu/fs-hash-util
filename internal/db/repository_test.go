package db

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateAndFinishScan(t *testing.T) {
	db := openTestDB(t)

	id, err := db.CreateScan("/tmp/root", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 {
		t.Fatalf("expected positive scan id, got %d", id)
	}

	if err := db.FinishScan(id, 2000); err != nil {
		t.Fatal(err)
	}

	var finished *int64
	row := db.QueryRow(`SELECT finished_at FROM scans WHERE id=?`, id)
	if err := row.Scan(&finished); err != nil {
		t.Fatal(err)
	}
	if finished == nil || *finished != 2000 {
		t.Fatalf("expected finished_at=2000, got %v", finished)
	}
}

func TestUpsertAndGetByRelPath(t *testing.T) {
	db := openTestDB(t)
	scanID, _ := db.CreateScan("/root", 1)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rec := FileRecord{
		ScanRoot: "/root", FileName: "a.txt", RelPath: "a.txt",
		ModifiedNS: 12345, Size: 10, Hash: "deadbeef", ScanID: scanID,
	}
	if err := db.Upsert(tx, rec); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, found, err := db.GetByRelPath("/root", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected record to be found")
	}
	if got.Hash != "deadbeef" {
		t.Fatalf("hash mismatch: %s", got.Hash)
	}
}

func TestUpsertIdempotent(t *testing.T) {
	db := openTestDB(t)
	scanID1, _ := db.CreateScan("/root", 1)
	scanID2, _ := db.CreateScan("/root", 2)

	rec := FileRecord{
		ScanRoot: "/root", FileName: "b.txt", RelPath: "b.txt",
		ModifiedNS: 999, Size: 5, Hash: "aaa", ScanID: scanID1,
	}

	tx1, _ := db.Begin()
	if err := db.Upsert(tx1, rec); err != nil {
		t.Fatal(err)
	}
	tx1.Commit()

	// Upsert same path with different hash / scanID.
	rec.Hash = "bbb"
	rec.ScanID = scanID2
	tx2, _ := db.Begin()
	if err := db.Upsert(tx2, rec); err != nil {
		t.Fatal(err)
	}
	tx2.Commit()

	got, _, _ := db.GetByRelPath("/root", "b.txt")
	if got.Hash != "bbb" {
		t.Fatalf("expected updated hash bbb, got %s", got.Hash)
	}
}

func TestQueryByHash(t *testing.T) {
	db := openTestDB(t)
	scanID, _ := db.CreateScan("/root", 1)

	tx, _ := db.Begin()
	db.Upsert(tx, FileRecord{ScanRoot: "/root", FileName: "x.txt", RelPath: "x.txt", ModifiedNS: 1, Size: 3, Hash: "cafebabe", ScanID: scanID})
	db.Upsert(tx, FileRecord{ScanRoot: "/root", FileName: "y.txt", RelPath: "y.txt", ModifiedNS: 1, Size: 3, Hash: "cafebabe", ScanID: scanID})
	tx.Commit()

	recs, err := db.QueryByHash("cafebabe")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
}

func TestDeleteNotSeen(t *testing.T) {
	db := openTestDB(t)
	scanID1, _ := db.CreateScan("/root", 1)
	scanID2, _ := db.CreateScan("/root", 2)

	tx, _ := db.Begin()
	db.Upsert(tx, FileRecord{ScanRoot: "/root", FileName: "old.txt", RelPath: "old.txt", ModifiedNS: 1, Size: 1, Hash: "h1", ScanID: scanID1})
	db.Upsert(tx, FileRecord{ScanRoot: "/root", FileName: "new.txt", RelPath: "new.txt", ModifiedNS: 2, Size: 2, Hash: "h2", ScanID: scanID2})
	tx.Commit()

	db.FinishScan(scanID2, 3)
	n, err := db.DeleteNotSeen("/root", scanID2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted, got %d", n)
	}

	_, found, _ := db.GetByRelPath("/root", "old.txt")
	if found {
		t.Fatal("old.txt should have been pruned")
	}
	_, found, _ = db.GetByRelPath("/root", "new.txt")
	if !found {
		t.Fatal("new.txt should still exist")
	}
}

func TestUpsertFolderAndGet(t *testing.T) {
	db := openTestDB(t)
	scanID, _ := db.CreateScan("/root", 1)

	tx, _ := db.Begin()
	if err := db.UpsertFolder(tx, FolderRecord{
		ScanRoot: "/root", RelPath: ".", Hash: "folderhash1", ScanID: scanID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rec, found, err := db.GetFolderByRelPath("/root", ".")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected folder record to be found")
	}
	if rec.Hash != "folderhash1" {
		t.Fatalf("hash mismatch: %s", rec.Hash)
	}

	// upsert updates hash
	scanID2, _ := db.CreateScan("/root", 2)
	tx2, _ := db.Begin()
	if err := db.UpsertFolder(tx2, FolderRecord{
		ScanRoot: "/root", RelPath: ".", Hash: "folderhash2", ScanID: scanID2,
	}); err != nil {
		t.Fatal(err)
	}
	tx2.Commit()

	rec2, _, _ := db.GetFolderByRelPath("/root", ".")
	if rec2.Hash != "folderhash2" {
		t.Fatalf("expected updated hash folderhash2, got %s", rec2.Hash)
	}
}

func TestDeleteFoldersNotSeen(t *testing.T) {
	db := openTestDB(t)
	scanID1, _ := db.CreateScan("/root", 1)
	scanID2, _ := db.CreateScan("/root", 2)

	tx, _ := db.Begin()
	db.UpsertFolder(tx, FolderRecord{ScanRoot: "/root", RelPath: ".", Hash: "old", ScanID: scanID1})
	db.UpsertFolder(tx, FolderRecord{ScanRoot: "/root", RelPath: "sub", Hash: "new", ScanID: scanID2})
	tx.Commit()

	n, err := db.DeleteFoldersNotSeen("/root", scanID2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted, got %d", n)
	}

	_, found, _ := db.GetFolderByRelPath("/root", ".")
	if found {
		t.Fatal("root folder should have been pruned")
	}
	_, found, _ = db.GetFolderByRelPath("/root", "sub")
	if !found {
		t.Fatal("sub folder should still exist")
	}
}

func TestGetByRelPath_notfound(t *testing.T) {
	db := openTestDB(t)
	_, found, err := db.GetByRelPath("/root", "missing.txt")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected not found")
	}
}

func TestTouchScanID(t *testing.T) {
	db := openTestDB(t)
	scanID1, _ := db.CreateScan("/root", 1)
	scanID2, _ := db.CreateScan("/root", 2)

	tx, _ := db.Begin()
	db.Upsert(tx, FileRecord{ScanRoot: "/root", FileName: "f.txt", RelPath: "f.txt", ModifiedNS: 1, Size: 1, Hash: "h", ScanID: scanID1})
	tx.Commit()

	rec, _, _ := db.GetByRelPath("/root", "f.txt")

	tx2, _ := db.Begin()
	if err := db.TouchScanID(tx2, rec.ID, scanID2); err != nil {
		t.Fatal(err)
	}
	tx2.Commit()

	updated, _, _ := db.GetByRelPath("/root", "f.txt")
	if updated.ScanID != scanID2 {
		t.Fatalf("expected scan_id=%d, got %d", scanID2, updated.ScanID)
	}
}
