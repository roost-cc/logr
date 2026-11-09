package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
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

// TestOpenRead_poolLimit runs more readers at once than each pool allows, on
// the read pool and on the write connection. No reader fails, and no pool
// opens more connections than its limit.
func TestOpenRead_poolLimit(t *testing.T) {
	write, read := openTestPools(t)
	ctx := context.Background()
	if _, err := write.ExecContext(ctx, `INSERT INTO logs (source, level, message) VALUES
		('api', 'info', 'm'), ('web', NULL, 'm')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, tc := range []struct {
		name  string
		pool  *sql.DB
		limit int
	}{
		{"read pool", read, readConns},
		{"write connection", write, 1},
	} {
		if got := tc.pool.Stats().MaxOpenConnections; got != tc.limit {
			t.Errorf("%s: MaxOpenConnections = %d, want %d", tc.name, got, tc.limit)
		}
		const readers = 20
		var wg sync.WaitGroup
		errs := make(chan error, readers)
		open := make(chan int, readers)
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := CountBySourceLevel(ctx, tc.pool, AggOpts{})
				errs <- err
				open <- tc.pool.Stats().OpenConnections
			}()
		}
		wg.Wait()
		close(errs)
		close(open)
		for err := range errs {
			if err != nil {
				t.Errorf("%s: reader failed: %v", tc.name, err)
			}
		}
		for n := range open {
			if n > tc.limit {
				t.Errorf("%s: %d connections open, want at most %d", tc.name, n, tc.limit)
			}
		}
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
