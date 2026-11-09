package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	"database/sql/driver"

	sqlite "modernc.org/sqlite"
)

func init() {
	// Register REGEXP function: `x REGEXP y` calls regexp(y, x) in SQLite.
	// Prepending (?i) to the pattern enables case-insensitive matching.
	sqlite.MustRegisterScalarFunction("regexp", 2,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			pattern, _ := args[0].(string)
			text, _ := args[1].(string)
			ok, err := regexp.MatchString(pattern, text)
			if err != nil {
				return nil, err
			}
			if ok {
				return int64(1), nil
			}
			return int64(0), nil
		},
	)
}

const ddl = `
CREATE TABLE IF NOT EXISTS logs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	ingested_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	log_time    TEXT,
	source      TEXT    NOT NULL,
	level       TEXT,
	message     TEXT    NOT NULL,
	data        TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS logs_log_time_idx    ON logs (log_time DESC);
CREATE INDEX IF NOT EXISTS logs_ingested_at_idx ON logs (ingested_at DESC);
CREATE INDEX IF NOT EXISTS logs_source_idx      ON logs (source);
CREATE INDEX IF NOT EXISTS logs_level_idx       ON logs (level);
CREATE INDEX IF NOT EXISTS logs_time_idx         ON logs (` + timeSortSQL + `);
CREATE INDEX IF NOT EXISTS logs_source_level_idx ON logs (source, level);
`

// dsnParams are the connection parameters that Open and OpenRead share.
const dsnParams = "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"

// Open opens (or creates) a SQLite database at path with WAL mode enabled.
// It is the single write connection: Migrate, the ingest flush and Purge use it.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite supports one writer at a time
	return db, nil
}

// OpenRead opens a pool of read-only connections to the database at path.
// In WAL mode its reads run while the write connection from Open writes, so
// a long query does not hold up ingest (F-100 D4).
func OpenRead(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path+dsnParams+"&_pragma=query_only(1)")
}

// Migrate applies the schema to the database. Safe to call on an existing DB.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
