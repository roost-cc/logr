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
// group with the same key, so the query has no LIMIT: aggQuery merges the two
// groups, then keeps aggLimit. The source × level query orders by the columns,
// so that the groups of one source come together; the GROUP BY gives that
// order already, and the ORDER BY adds no sort step.
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
		GROUP BY %s, %s
		ORDER BY %s, %s`, dim1, dim2, where, dim1, dim2, dim1, dim2)
}

// topGroups keeps the limit largest groups that it was given, so that a stats
// call holds at most limit groups, whatever the number of groups in the data.
type topGroups struct {
	limit int
	rows  []AggRow
}

// add gives one group. When the list is full, the group takes the place of the
// smallest group if it is larger.
func (t *topGroups) add(r AggRow) {
	if len(t.rows) < t.limit {
		t.rows = append(t.rows, r)
		return
	}
	smallest := 0
	for i := range t.rows {
		if t.rows[i].Count < t.rows[smallest].Count {
			smallest = i
		}
	}
	if r.Count > t.rows[smallest].Count {
		t.rows[smallest] = r
	}
}

// aggHeldHook, if set, gets the number of groups aggQuery holds after each row.
// Only tests set it.
var aggHeldHook func(groups int)

// aggQuery reads the groups as a stream and keeps the aggLimit largest. It
// holds the '(none)' group (a NULL value and the text '(none)') aside until
// both parts have come: to the end of the rows for one column, and to the end
// of the source for source × level. source is NOT NULL, so the first key never
// has two parts.
func aggQuery(ctx context.Context, db *sql.DB, dim1, dim2 string, opts AggOpts) ([]AggRow, error) {
	rows, err := db.QueryContext(ctx, aggSQL(dim1, dim2, opts.Where), opts.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	top := topGroups{limit: aggLimit}
	var none *AggRow
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
			if none != nil && r.Key != none.Key {
				top.add(*none)
				none = nil
			}
		}
		if (dim2 == "" && r.Key == "(none)") || (dim2 != "" && r.Key2 == "(none)") {
			if none == nil {
				none = &r
			} else {
				none.Count += r.Count
			}
		} else {
			top.add(r)
		}
		if aggHeldHook != nil {
			held := len(top.rows)
			if none != nil {
				held++
			}
			aggHeldHook(held)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if none != nil {
		top.add(*none)
	}
	sort.SliceStable(top.rows, func(i, j int) bool { return top.rows[i].Count > top.rows[j].Count })
	return top.rows, nil
}
