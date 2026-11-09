package db

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestStats_plans checks that every stats query with no filter reads logs
// only through a covering index and groups with no temporary B-tree (F-100 D3).
func TestStats_plans(t *testing.T) {
	sqlDB := openTestDB(t)
	for name, q := range map[string]string{
		"TotalCount":         totalSQL(""),
		"CountBySource":      aggSQL("source", "", ""),
		"CountByLevel":       aggSQL("level", "", ""),
		"CountBySourceLevel": aggSQL("source", "level", ""),
	} {
		plan := queryPlan(t, sqlDB, q)
		for _, line := range strings.Split(plan, "\n") {
			if strings.Contains(line, " logs") && !strings.Contains(line, "USING COVERING INDEX") {
				t.Errorf("%s: plan reads logs with no covering index:\n%s", name, plan)
			}
		}
		if strings.Contains(plan, "USE TEMP B-TREE FOR GROUP BY") {
			t.Errorf("%s: plan groups in a temporary B-tree:\n%s", name, plan)
		}
	}
}

// TestStats_counts checks the counts, with '(none)' for a row with no level.
// A row with the text '(none)' as its level counts in the same group.
func TestStats_counts(t *testing.T) {
	sqlDB := openTestDB(t)
	ctx := context.Background()
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO logs (source, level, message) VALUES
		('api', 'info', 'm'), ('api', 'info', 'm'), ('api', NULL, 'm'), ('api', '(none)', 'm'),
		('web', 'error', 'm'), ('web', NULL, 'm'), ('web', NULL, 'm')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	total, err := TotalCount(ctx, sqlDB, AggOpts{})
	if err != nil || total != 7 {
		t.Errorf("TotalCount = %d, %v; want 7", total, err)
	}

	bySource, err := CountBySource(ctx, sqlDB, AggOpts{})
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, "CountBySource", bySource, []AggRow{{Key: "api", Count: 4}, {Key: "web", Count: 3}})

	byLevel, err := CountByLevel(ctx, sqlDB, AggOpts{})
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, "CountByLevel", byLevel, []AggRow{
		{Key: "(none)", Count: 4}, {Key: "info", Count: 2}, {Key: "error", Count: 1},
	})

	bySourceLevel, err := CountBySourceLevel(ctx, sqlDB, AggOpts{})
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, "CountBySourceLevel", bySourceLevel, []AggRow{
		{Key: "api", Key2: "info", Count: 2}, {Key: "web", Key2: "(none)", Count: 2},
		{Key: "api", Key2: "(none)", Count: 2}, {Key: "web", Key2: "error", Count: 1},
	})

	filtered, err := CountByLevel(ctx, sqlDB, AggOpts{Where: "source = ?", Args: []any{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, "CountByLevel source=web", filtered, []AggRow{{Key: "(none)", Count: 2}, {Key: "error", Count: 1}})
}

// TestStats_noneMergesBeforeLimit checks, for every stats count that groups by
// level, that a NULL level and the text '(none)' make one group, and that the
// groups merge before the limit of 20. There are 25 levels with 3 rows each.
// The NULL level and the text '(none)' have 2 rows each: apart, each is outside
// the first 20; together they are the largest group.
func TestStats_noneMergesBeforeLimit(t *testing.T) {
	sqlDB := openTestDB(t)
	ctx := context.Background()
	insert := func(level any, n int) {
		for i := 0; i < n; i++ {
			if _, err := sqlDB.ExecContext(ctx,
				`INSERT INTO logs (source, level, message) VALUES ('api', ?, 'm')`, level); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	for i := 0; i < 25; i++ {
		insert(fmt.Sprintf("L%02d", i), 3)
	}
	insert(nil, 2)
	insert("(none)", 2)

	for _, tc := range []struct {
		name  string
		count func(context.Context, *sql.DB, AggOpts) ([]AggRow, error)
		want  AggRow
	}{
		{"CountByLevel", CountByLevel, AggRow{Key: "(none)", Count: 4}},
		{"CountBySourceLevel", CountBySourceLevel, AggRow{Key: "api", Key2: "(none)", Count: 4}},
	} {
		got, err := tc.count(ctx, sqlDB, AggOpts{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != aggLimit {
			t.Errorf("%s: %d rows, want %d", tc.name, len(got), aggLimit)
		}
		if len(got) == 0 || got[0] != tc.want {
			t.Errorf("%s = %v, want %v first", tc.name, got, tc.want)
		}
		for _, r := range got[1:] {
			if r.Key2 == "(none)" || (tc.want.Key2 == "" && r.Key == "(none)") {
				t.Errorf("%s: more than one (none) group: %v", tc.name, got)
			}
		}
	}
}

// TestStats_manyGroupsBounded checks, for every stats count, that one call
// holds at most aggLimit groups plus the '(none)' group, with 2,000 more
// groups than that in the data, and that it returns the largest aggLimit
// groups of the old GROUP BY COALESCE(…, '(none)') query.
func TestStats_manyGroupsBounded(t *testing.T) {
	sqlDB := openTestDB(t)
	ctx := context.Background()
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(source string, level any, n int) {
		for i := 0; i < n; i++ {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO logs (source, level, message) VALUES (?, ?, 'm')`, source, level); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	// 'aaa' sorts before 'api': its '(none)' parts must not add to those of 'api'.
	insert("aaa", nil, 1)
	insert("aaa", "(none)", 1)
	for i := 0; i < 25; i++ {
		insert("api", fmt.Sprintf("B%02d", i), 30+2*i)
	}
	insert("api", nil, 20)
	insert("api", "(none)", 21)
	for i := 0; i < 2000; i++ {
		insert(fmt.Sprintf("s%04d", i), fmt.Sprintf("S%04d", i), 1)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		count func(context.Context, *sql.DB, AggOpts) ([]AggRow, error)
		old   string
	}{
		{"CountBySource", CountBySource, `SELECT COALESCE(source, '(none)'), '', count(*) FROM logs GROUP BY 1`},
		{"CountByLevel", CountByLevel, `SELECT COALESCE(level, '(none)'), '', count(*) FROM logs GROUP BY 1`},
		{"CountBySourceLevel", CountBySourceLevel,
			`SELECT COALESCE(source, '(none)'), COALESCE(level, '(none)'), count(*) FROM logs GROUP BY 1, 2`},
	} {
		held := 0
		aggHeldHook = func(n int) { held = max(held, n) }
		got, err := tc.count(ctx, sqlDB, AggOpts{})
		aggHeldHook = nil
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if held > aggLimit+1 {
			t.Errorf("%s held %d groups, want at most %d", tc.name, held, aggLimit+1)
		}
		assertTopGroups(t, tc.name, got, oldGroups(t, sqlDB, tc.old))
	}
}

// oldGroups runs q, which returns key, key2 and count, and returns the counts by keys.
func oldGroups(t *testing.T, sqlDB *sql.DB, q string) map[[2]string]int64 {
	t.Helper()
	rows, err := sqlDB.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	all := map[[2]string]int64{}
	for rows.Next() {
		var k [2]string
		var n int64
		if err := rows.Scan(&k[0], &k[1], &n); err != nil {
			t.Fatal(err)
		}
		all[k] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return all
}

// assertTopGroups checks that got holds the aggLimit largest groups of all,
// largest first. Groups with the same count as the last one kept may change
// places, so it compares the counts and checks each group's count in all.
func assertTopGroups(t *testing.T, name string, got []AggRow, all map[[2]string]int64) {
	t.Helper()
	var counts []int64
	for _, n := range all {
		counts = append(counts, n)
	}
	sort.Slice(counts, func(i, j int) bool { return counts[i] > counts[j] })
	if len(counts) > aggLimit {
		counts = counts[:aggLimit]
	}
	var gotCounts []int64
	seen := map[[2]string]bool{}
	for _, r := range got {
		k := [2]string{r.Key, r.Key2}
		if seen[k] || all[k] != r.Count {
			t.Errorf("%s: row %v is repeated or has the wrong count (want %d)", name, r, all[k])
		}
		seen[k] = true
		gotCounts = append(gotCounts, r.Count)
	}
	if !reflect.DeepEqual(gotCounts, counts) {
		t.Errorf("%s counts = %v, want %v", name, gotCounts, counts)
	}
}

// assertCounts compares rows with want as sets: rows with the same count may
// come in any order.
func assertCounts(t *testing.T, name string, got, want []AggRow) {
	t.Helper()
	index := func(rows []AggRow) map[AggRow]bool {
		m := map[AggRow]bool{}
		for _, r := range rows {
			m[r] = true
		}
		return m
	}
	if len(got) != len(want) || !reflect.DeepEqual(index(got), index(want)) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Count > got[i-1].Count {
			t.Errorf("%s is not sorted by count, descending: %v", name, got)
		}
	}
}
