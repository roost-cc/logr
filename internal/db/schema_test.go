package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// openTestPools opens a migrated database in a temporary file and returns its
// write connection from Open and a read pool from OpenRead.
func openTestPools(t *testing.T) (write, read *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logr.db")
	write = openTestDBAt(t, path)
	read, err := OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	t.Cleanup(func() { read.Close() })
	return write, read
}

// TestOpenRead_rejectsWrites checks that a write through the read pool fails.
func TestOpenRead_rejectsWrites(t *testing.T) {
	_, read := openTestPools(t)
	if _, err := read.ExecContext(context.Background(),
		`INSERT INTO logs (source, message) VALUES ('api', 'm')`); err == nil {
		t.Fatal("INSERT through the read pool succeeded, want an error")
	}
}

// TestOpenRead_flushDuringRead holds a read transaction open on the read pool
// and checks that an ingest flush on the write connection commits during it
// (F-100 D4). If the read used the write connection, the flush would wait for
// it and the test would time out.
func TestOpenRead_flushDuringRead(t *testing.T) {
	write, read := openTestPools(t)
	ctx := context.Background()

	tx, err := read.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM logs`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}

	ins := NewInserter(write)
	if err := ins.Add(ctx, Record{Source: "api", Message: "m"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- ins.Flush(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Flush: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Flush did not commit while a read was open")
	}

	// The open read still sees its snapshot; a new read sees the flushed row.
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM logs`).Scan(&n); err != nil || n != 0 {
		t.Errorf("open read: count = %d, %v; want 0", n, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("end read: %v", err)
	}
	if err := read.QueryRowContext(ctx, `SELECT count(*) FROM logs`).Scan(&n); err != nil || n != 1 {
		t.Errorf("new read: count = %d, %v; want 1", n, err)
	}
}
