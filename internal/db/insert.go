package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Record is a parsed log entry ready for storage.
type Record struct {
	LogTime *time.Time
	Source  string
	Level   string
	Message string
	Data    map[string]any
}

// Inserter batches records and flushes to SQLite in transactions.
// Safe for concurrent use.
type Inserter struct {
	mu     sync.Mutex
	db     *sql.DB
	buf    []Record
	maxBuf int
}

func NewInserter(db *sql.DB) *Inserter {
	return &Inserter{db: db, maxBuf: 200}
}

// Add enqueues a record and flushes synchronously when the buffer is full.
func (ins *Inserter) Add(ctx context.Context, r Record) error {
	ins.mu.Lock()
	ins.buf = append(ins.buf, r)
	flush := len(ins.buf) >= ins.maxBuf
	ins.mu.Unlock()

	if flush {
		return ins.Flush(ctx)
	}
	return nil
}

// Flush writes all buffered records to SQLite in a single transaction.
func (ins *Inserter) Flush(ctx context.Context) error {
	ins.mu.Lock()
	if len(ins.buf) == 0 {
		ins.mu.Unlock()
		return nil
	}
	batch := ins.buf
	ins.buf = make([]Record, 0, ins.maxBuf)
	ins.mu.Unlock()

	tx, err := ins.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("flush begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO logs (log_time, source, level, message, data) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("flush prepare: %w", err)
	}
	defer stmt.Close()

	for _, r := range batch {
		data, _ := json.Marshal(r.Data)
		if data == nil {
			data = []byte("{}")
		}
		_, err := stmt.ExecContext(ctx,
			formatTime(r.LogTime),
			r.Source,
			nilIfEmpty(r.Level),
			r.Message,
			string(data),
		)
		if err != nil {
			return fmt.Errorf("flush insert: %w", err)
		}
	}

	return tx.Commit()
}

func formatTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
