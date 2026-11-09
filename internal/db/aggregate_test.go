package db

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
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
