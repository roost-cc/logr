package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// openTestDB opens a migrated database in a temporary file.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := Open(filepath.Join(t.TempDir(), "logr.db"))
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

func TestSources_empty(t *testing.T) {
	got, err := Sources(context.Background(), openTestDB(t))
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Sources = %v, want empty", got)
	}
}

// TestSources_plan checks that Sources seeks logs_source_idx and never scans
// the whole logs table (F-100 D2).
func TestSources_plan(t *testing.T) {
	sqlDB := openTestDB(t)
	rows, err := sqlDB.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+sourcesSQL)
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
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "SEARCH logs USING COVERING INDEX logs_source_idx") {
		t.Errorf("plan has no SEARCH on logs_source_idx:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN logs") {
		t.Errorf("plan scans logs:\n%s", plan)
	}
}
