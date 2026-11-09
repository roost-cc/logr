package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/roost-cc/logr/internal/filter"
)

// openTestDB opens a migrated database in a temporary file.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return openTestDBAt(t, filepath.Join(t.TempDir(), "logr.db"))
}

// openTestDBAt opens and migrates the database at path with Open.
func openTestDBAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	sqlDB, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := Migrate(context.Background(), sqlDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return sqlDB
}

func TestSources_sorted(t *testing.T) {
	sqlDB := openTestDB(t)
	ctx := context.Background()
	for _, src := range []string{"web", "api", "worker", "api", "web", "api"} {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO logs (source, message) VALUES (?, 'm')`, src); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	got, err := Sources(ctx, sqlDB)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	want := []string{"api", "web", "worker"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sources = %v, want %v", got, want)
	}
}

// TestSources_edgeValues checks that the loose index scan keeps an empty
// source and sources that differ only in case, in the same order as the old
// SELECT DISTINCT source ORDER BY source.
func TestSources_edgeValues(t *testing.T) {
	sqlDB := openTestDB(t)
	ctx := context.Background()
	for _, src := range []string{"web", "", "Web", "api", ""} {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO logs (source, message) VALUES (?, 'm')`, src); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	got, err := Sources(ctx, sqlDB)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	want := []string{"", "Web", "api", "web"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sources = %q, want %q", got, want)
	}
}

func TestSources_empty(t *testing.T) {
	got, err := Sources(context.Background(), openTestDB(t))
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Sources = %v, want empty", got)
	}
}

// queryPlan returns the EXPLAIN QUERY PLAN detail lines of q, one per line.
func queryPlan(t *testing.T, sqlDB *sql.DB, q string, args ...any) string {
	t.Helper()
	rows, err := sqlDB.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return strings.Join(details, "\n")
}

// filterExprs are the filter shapes the plan tests run each query with: none,
// and one of each kind that internal/filter builds.
var filterExprs = []string{
	"",
	"source=storage",
	"level!=info",
	"source IN (storage, account, synapse)",
	`message CONTAINS "routing"`,
	`message MATCHES "^ERROR"`,
	`NOT (source IN (account, residence))`,
	"data.status=500",
	"log_time > 10m",
	"log_time <= 1d",
	"ingested_at > 30s",
	"log_time > 2026-11-01T00:00:00Z",
	"log_time < 2026-11-01T00:00:00Z",
	"log_time = 2026-11-01T00:00:00.000Z",
	"source=storage AND level=error",
	"level=error OR level=fatal",
}

// queryShape is one query the server runs, with its arguments.
type queryShape struct {
	name string
	sql  string
	args []any
}

// queryShapes returns every Query and stats query shape, each with every filter.
func queryShapes(t *testing.T) []queryShape {
	t.Helper()
	var shapes []queryShape
	for _, expr := range filterExprs {
		where, fargs := "", []any(nil)
		if expr != "" {
			r, err := filter.Parse(expr, 1)
			if err != nil {
				t.Fatalf("filter.Parse(%q): %v", expr, err)
			}
			where, fargs = r.SQL, r.Args
		}
		opts := []struct {
			name string
			opts QueryOpts
		}{
			{"newest", QueryOpts{}},
			{"tail", QueryOpts{After: 100}},
			{"page", QueryOpts{Before: 100}},
		}
		for _, col := range []string{"log_time", "ingested_at", "source", "level", "message", "data.path"} {
			opts = append(opts,
				struct {
					name string
					opts QueryOpts
				}{"sort " + col + " ASC", QueryOpts{SortCol: col}},
				struct {
					name string
					opts QueryOpts
				}{"sort " + col + " DESC", QueryOpts{SortCol: col, SortDesc: true}},
			)
		}
		for _, o := range opts {
			o.opts.Where, o.opts.Args, o.opts.Limit = where, fargs, 500
			q, args := querySQL(o.opts)
			shapes = append(shapes, queryShape{"Query " + o.name + " [" + expr + "]", q, args})
		}
		shapes = append(shapes,
			queryShape{"TotalCount [" + expr + "]", totalSQL(where), fargs},
			queryShape{"CountBySource [" + expr + "]", aggSQL("source", "", where), fargs},
			queryShape{"CountByLevel [" + expr + "]", aggSQL("level", "", where), fargs},
			queryShape{"CountBySourceLevel [" + expr + "]", aggSQL("source", "level", where), fargs},
		)
	}
	return shapes
}

// TestQueryShapes_everyIndexUsed lists each query shape and its plan (go test
// -v), and fails if an index on logs is in no plan: each index makes ingest
// slower, so an index no query uses must go (F-100 D3).
func TestQueryShapes_everyIndexUsed(t *testing.T) {
	sqlDB := openTestDB(t)
	rows, err := sqlDB.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'logs' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	unused := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		unused[name] = true
	}
	rows.Close()

	for _, s := range queryShapes(t) {
		plan := queryPlan(t, sqlDB, s.sql, s.args...)
		t.Logf("%s\n%s", s.name, plan)
		for _, line := range strings.Split(plan, "\n") {
			for name := range unused {
				if strings.Contains(line+" ", " INDEX "+name+" ") {
					delete(unused, name)
				}
			}
		}
	}
	if len(unused) > 0 {
		t.Errorf("no query shape uses these indexes: %v", unused)
	}
}

// hasFullScan reports whether a plan reads the whole logs table with no index.
func hasFullScan(plan string) bool {
	for _, line := range strings.Split(plan, "\n") {
		if line == "SCAN logs" {
			return true
		}
	}
	return false
}

// TestQuery_timeSortPlan checks that a sort on log_time with no filter walks
// logs_time_idx in both directions, with no full scan and no sort step (F-100 D3).
func TestQuery_timeSortPlan(t *testing.T) {
	sqlDB := openTestDB(t)
	for _, desc := range []bool{false, true} {
		q, args := querySQL(QueryOpts{SortCol: "log_time", SortDesc: desc, Limit: 500})
		plan := queryPlan(t, sqlDB, q, args...)
		if !strings.Contains(plan, "USING INDEX logs_time_idx") {
			t.Errorf("desc=%v: plan does not use logs_time_idx:\n%s", desc, plan)
		}
		if hasFullScan(plan) || strings.Contains(plan, "USE TEMP B-TREE") {
			t.Errorf("desc=%v: plan scans logs or sorts in a temporary B-tree:\n%s", desc, plan)
		}
	}
}

// TestQuery_tailSourcePlan checks that the tail query with a source filter
// still reads only the rows after the last id of logs_source_idx.
func TestQuery_tailSourcePlan(t *testing.T) {
	sqlDB := openTestDB(t)
	for _, expr := range []string{"source=storage", "source IN (storage, account)"} {
		r, err := filter.Parse(expr, 1)
		if err != nil {
			t.Fatalf("filter.Parse(%q): %v", expr, err)
		}
		q, args := querySQL(QueryOpts{Where: r.SQL, Args: r.Args, After: 100, Limit: 500})
		plan := queryPlan(t, sqlDB, q, args...)
		if !strings.Contains(plan, "logs_source_idx (source=? AND rowid>?)") {
			t.Errorf("%s: plan has no logs_source_idx (source=? AND rowid>?):\n%s", expr, plan)
		}
	}
}

// TestMigrate_existingDB checks that Migrate adds the new indexes to a
// database made by the schema before F-100, and keeps its rows.
func TestMigrate_existingDB(t *testing.T) {
	sqlDB, err := Open(filepath.Join(t.TempDir(), "logr.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sqlDB.Close()
	ctx := context.Background()
	if _, err := sqlDB.ExecContext(ctx, `
		CREATE TABLE logs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			ingested_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			log_time    TEXT,
			source      TEXT    NOT NULL,
			level       TEXT,
			message     TEXT    NOT NULL,
			data        TEXT    NOT NULL DEFAULT '{}'
		);
		CREATE INDEX logs_log_time_idx    ON logs (log_time DESC);
		CREATE INDEX logs_ingested_at_idx ON logs (ingested_at DESC);
		CREATE INDEX logs_source_idx      ON logs (source);
		CREATE INDEX logs_level_idx       ON logs (level);
		INSERT INTO logs (source, level, message) VALUES ('api', 'info', 'm'), ('web', NULL, 'm');`); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, name := range []string{"logs_time_idx", "logs_source_level_idx"} {
		var n int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("index %s missing after Migrate", name)
		}
	}
	if n, err := TotalCount(ctx, sqlDB, AggOpts{}); err != nil || n != 2 {
		t.Errorf("TotalCount = %d, %v; want 2", n, err)
	}
}

// TestSources_plan checks that Sources seeks logs_source_idx and never scans
// the whole logs table (F-100 D2).
func TestSources_plan(t *testing.T) {
	plan := queryPlan(t, openTestDB(t), sourcesSQL)
	if !strings.Contains(plan, "SEARCH logs USING COVERING INDEX logs_source_idx") {
		t.Errorf("plan has no SEARCH on logs_source_idx:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN logs") {
		t.Errorf("plan scans logs:\n%s", plan)
	}
}

// TestPurge_batches checks that Purge deletes old rows in more than one batch
// until none is left, and keeps every row newer than the cutoff (F-100 D5).
func TestPurge_batches(t *testing.T) {
	old := purgeBatch
	purgeBatch = 3
	t.Cleanup(func() { purgeBatch = old })

	sqlDB := openTestDB(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO logs (ingested_at, source, message) VALUES ('2026-01-01T00:00:00.000Z', 'api', 'old')`); err != nil {
			t.Fatalf("insert old: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO logs (source, message) VALUES ('api', 'new')`); err != nil {
			t.Fatalf("insert new: %v", err)
		}
	}
	n, err := Purge(ctx, sqlDB, time.Hour)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 10 {
		t.Errorf("Purge deleted %d rows, want 10", n)
	}
	var kept int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM logs WHERE message = 'new'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if total, err := TotalCount(ctx, sqlDB, AggOpts{}); err != nil || total != 2 || kept != 2 {
		t.Errorf("after Purge: %d rows (%d new), %v; want 2 new rows", total, kept, err)
	}
}

// TestPurge_plan checks that a purge batch seeks logs_ingested_at_idx and
// never scans the whole logs table (F-100 D5).
func TestPurge_plan(t *testing.T) {
	plan := queryPlan(t, openTestDB(t), purgeSQL, "2026-01-01T00:00:00Z", purgeBatch)
	if !strings.Contains(plan, "logs_ingested_at_idx (ingested_at<?)") {
		t.Errorf("plan does not seek logs_ingested_at_idx:\n%s", plan)
	}
	if hasFullScan(plan) {
		t.Errorf("plan scans logs:\n%s", plan)
	}
}
