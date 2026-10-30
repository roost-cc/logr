package db

import (
	"context"
	"database/sql"
	"fmt"
)

// AggRow is one bucket in an aggregation result.
type AggRow struct {
	Key   string
	Key2  string // set for cross-dimension queries (source × level)
	Count int64
}

// AggOpts scopes an aggregation to the current filter.
type AggOpts struct {
	Where string
	Args  []any
}

func CountBySource(ctx context.Context, db *sql.DB, opts AggOpts) ([]AggRow, error) {
	return aggQuery(ctx, db, "source", "", opts)
}

func CountByLevel(ctx context.Context, db *sql.DB, opts AggOpts) ([]AggRow, error) {
	return aggQuery(ctx, db, "level", "", opts)
}

func CountBySourceLevel(ctx context.Context, db *sql.DB, opts AggOpts) ([]AggRow, error) {
	return aggQuery(ctx, db, "source", "level", opts)
}

func TotalCount(ctx context.Context, db *sql.DB, opts AggOpts) (int64, error) {
	where := "1"
	if opts.Where != "" {
		where = opts.Where
	}
	var n int64
	err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM logs WHERE %s`, where),
		opts.Args...,
	).Scan(&n)
	return n, err
}

func aggQuery(ctx context.Context, db *sql.DB, dim1, dim2 string, opts AggOpts) ([]AggRow, error) {
	where := "1"
	if opts.Where != "" {
		where = opts.Where
	}

	var q string
	if dim2 == "" {
		q = fmt.Sprintf(`
			SELECT COALESCE(%s, '(none)'), count(*)
			FROM logs WHERE %s
			GROUP BY 1 ORDER BY 2 DESC LIMIT 20`, dim1, where)
	} else {
		q = fmt.Sprintf(`
			SELECT COALESCE(%s, '(none)'), COALESCE(%s, '(none)'), count(*)
			FROM logs WHERE %s
			GROUP BY 1, 2 ORDER BY 3 DESC LIMIT 20`, dim1, dim2, where)
	}

	rows, err := db.QueryContext(ctx, q, opts.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []AggRow
	for rows.Next() {
		var r AggRow
		if dim2 == "" {
			if err := rows.Scan(&r.Key, &r.Count); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&r.Key, &r.Key2, &r.Count); err != nil {
				return nil, err
			}
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
