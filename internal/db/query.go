package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Row is a log record returned from a query.
type Row struct {
	ID         int64
	IngestedAt time.Time
	LogTime    *time.Time
	Source     string
	Level      string
	Message    string
	Data       map[string]any
}

// QueryOpts controls what rows are returned.
type QueryOpts struct {
	Where    string
	Args     []any
	Limit    int
	After    int64  // return rows with id > After (for tail polling)
	Before   int64  // return rows with id < Before
	SortCol  string // column to sort by (empty = id)
	SortDesc bool   // true = DESC, false = ASC
}

// Query returns rows matching opts, ordered newest first (or oldest first for tail).
func Query(ctx context.Context, db *sql.DB, opts QueryOpts) ([]Row, error) {
	where := "1"
	args := append([]any{}, opts.Args...)

	if opts.Where != "" {
		where = opts.Where
	}

	var orderExpr string
	if opts.After > 0 {
		where = fmt.Sprintf("(%s) AND id > ?", where)
		args = append(args, opts.After)
		orderExpr = "id ASC"
	} else if opts.Before > 0 {
		where = fmt.Sprintf("(%s) AND id < ?", where)
		args = append(args, opts.Before)
		orderExpr = "id DESC"
	} else if opts.SortCol != "" {
		dir := "ASC"
		if opts.SortDesc {
			dir = "DESC"
		}
		orderExpr = sortColSQL(opts.SortCol) + " " + dir
	} else {
		orderExpr = "id DESC"
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 200
	}

	q := fmt.Sprintf(`
		SELECT id, ingested_at, log_time, source, level, message, data
		FROM logs
		WHERE %s
		ORDER BY %s
		LIMIT ?`, where, orderExpr)
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var result []Row
	for rows.Next() {
		var r Row
		var ingestedAt string
		var logTimeStr sql.NullString
		var level sql.NullString
		var dataStr string

		if err := rows.Scan(&r.ID, &ingestedAt, &logTimeStr, &r.Source, &level, &r.Message, &dataStr); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}

		r.IngestedAt = parseTime(ingestedAt)
		if logTimeStr.Valid {
			t := parseTime(logTimeStr.String)
			r.LogTime = &t
		}
		if level.Valid {
			r.Level = level.String
		}
		if err := json.Unmarshal([]byte(dataStr), &r.Data); err != nil {
			r.Data = map[string]any{}
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Tail returns ASC; reverse so callers always get newest-first.
	if opts.After > 0 {
		for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
			result[i], result[j] = result[j], result[i]
		}
	}

	return result, nil
}

// MaxID returns the current maximum log id.
// sortColSQL maps a column key to a safe SQL ORDER BY expression.
func sortColSQL(col string) string {
	switch col {
	case "log_time":
		return "COALESCE(log_time, ingested_at)"
	case "ingested_at":
		return "ingested_at"
	case "source", "level", "message":
		return col
	default:
		if len(col) > 5 && col[:5] == "data." {
			key := strings.ReplaceAll(col[5:], "'", "''")
			return fmt.Sprintf("json_extract(data, '$.%s')", key)
		}
		return "id"
	}
}

func MaxID(ctx context.Context, db *sql.DB) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM logs`).Scan(&id)
	return id, err
}

// Purge deletes logs older than the given duration.
func Purge(ctx context.Context, db *sql.DB, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan).UTC().Format(time.RFC3339Nano)
	res, err := db.ExecContext(ctx, `DELETE FROM logs WHERE ingested_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Sources returns the distinct sources present in the logs table.
func Sources(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT source FROM logs ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var srcs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		srcs = append(srcs, s)
	}
	return srcs, rows.Err()
}

func parseTime(s string) time.Time {
	for _, f := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z07:00"} {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
