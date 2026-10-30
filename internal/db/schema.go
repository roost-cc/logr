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
`

// Open opens (or creates) a SQLite database at path with WAL mode enabled.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite supports one writer at a time
	return db, nil
}

// Migrate applies the schema to the database. Safe to call on an existing DB.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
