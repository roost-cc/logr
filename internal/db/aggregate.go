package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
	var n int64
	err := db.QueryRowContext(ctx, totalSQL(opts.Where), opts.Args...).Scan(&n)
	return n, err
}

// totalSQL builds the SELECT that TotalCount runs. The plan tests read its plan.
func totalSQL(where string) string {
	if where == "" {
		where = "1"
	}
	return fmt.Sprintf(`SELECT count(*) FROM logs WHERE %s`, where)
}

// aggLimit is the most groups an aggregation returns.
const aggLimit = 20

// aggSQL builds the SELECT that aggQuery runs. It groups by the columns, not by
// COALESCE(…, '(none)'), so that an index on the columns gives the groups in
// order with no temporary B-tree (F-100 D3). A NULL level is one group, and the
// SELECT list names it '(none)'. A level with the text '(none)' is a second
// group with the same key, so the query has no ORDER BY or LIMIT: aggQuery
// merges the groups by key first, then sorts and keeps aggLimit.
func aggSQL(dim1, dim2, where string) string {
	if where == "" {
		where = "1"
	}
	if dim2 == "" {
		return fmt.Sprintf(`
			SELECT COALESCE(%s, '(none)'), count(*)
			FROM logs WHERE %s
			GROUP BY %s`, dim1, where, dim1)
	}
	return fmt.Sprintf(`
		SELECT COALESCE(%s, '(none)'), COALESCE(%s, '(none)'), count(*)
		FROM logs WHERE %s
		GROUP BY %s, %s`, dim1, dim2, where, dim1, dim2)
}

// mergeAggRows adds up the rows with the same keys, sorts the result by
// count, largest first, and keeps the first limit rows.
func mergeAggRows(rows []AggRow, limit int) []AggRow {
	var result []AggRow
	at := map[[2]string]int{}
	for _, r := range rows {
		k := [2]string{r.Key, r.Key2}
		if i, ok := at[k]; ok {
			result[i].Count += r.Count
			continue
		}
		at[k] = len(result)
		result = append(result, r)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Count > result[j].Count })
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func aggQuery(ctx context.Context, db *sql.DB, dim1, dim2 string, opts AggOpts) ([]AggRow, error) {
	rows, err := db.QueryContext(ctx, aggSQL(dim1, dim2, opts.Where), opts.Args...)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mergeAggRows(result, aggLimit), nil
}
