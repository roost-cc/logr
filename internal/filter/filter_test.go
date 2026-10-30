package filter

import (
	"strings"
	"testing"
)

// helper: parse and return SQL + args, or fail.
func mustParse(t *testing.T, expr string) *Result {
	t.Helper()
	r, err := Parse(expr, 1)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", expr, err)
	}
	return r
}

// --- Equality / inequality ---

func TestEq(t *testing.T) {
	r := mustParse(t, "source=storage")
	if !strings.Contains(r.SQL, "source") {
		t.Errorf("SQL missing 'source': %s", r.SQL)
	}
	if len(r.Args) != 1 || r.Args[0] != "storage" {
		t.Errorf("args = %v, want [storage]", r.Args)
	}
}

func TestNeq(t *testing.T) {
	r := mustParse(t, "level!=info")
	if !strings.Contains(r.SQL, "!=") {
		t.Errorf("SQL missing '!=': %s", r.SQL)
	}
	if r.Args[0] != "info" {
		t.Errorf("args[0] = %v, want info", r.Args[0])
	}
}

// --- IN ---

func TestIn(t *testing.T) {
	r := mustParse(t, "source IN (storage, account, synapse)")
	if !strings.Contains(r.SQL, "IN") {
		t.Errorf("SQL missing IN: %s", r.SQL)
	}
	if len(r.Args) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(r.Args), r.Args)
	}
	got := map[any]bool{}
	for _, a := range r.Args {
		got[a] = true
	}
	for _, want := range []string{"storage", "account", "synapse"} {
		if !got[want] {
			t.Errorf("arg %q missing from %v", want, r.Args)
		}
	}
}

// --- CONTAINS ---

func TestContains(t *testing.T) {
	r := mustParse(t, `message CONTAINS "routing"`)
	if !strings.Contains(r.SQL, "LIKE") {
		t.Errorf("SQL missing LIKE: %s", r.SQL)
	}
	if r.Args[0] != "%routing%" {
		t.Errorf("args[0] = %q, want %%routing%%", r.Args[0])
	}
}

// --- MATCHES (regex) ---

func TestMatches_caseSensitive(t *testing.T) {
	r := mustParse(t, `message MATCHES "^ERROR"`)
	if !strings.Contains(r.SQL, "REGEXP") {
		t.Errorf("SQL should use REGEXP: %s", r.SQL)
	}
	if r.Args[0] != "^ERROR" {
		t.Errorf("args[0] = %q, want ^ERROR", r.Args[0])
	}
}

func TestMatches_caseInsensitive(t *testing.T) {
	r := mustParse(t, `message MATCHES* "timeout"`)
	if !strings.Contains(r.SQL, "REGEXP") {
		t.Errorf("SQL should use REGEXP: %s", r.SQL)
	}
	arg, _ := r.Args[0].(string)
	if !strings.HasPrefix(arg, "(?i)") {
		t.Errorf("case-insensitive pattern should start with (?i): %q", arg)
	}
}

func TestNot_matches(t *testing.T) {
	r := mustParse(t, `NOT (data.path MATCHES "^/(health|metrics)")`)
	if !strings.Contains(r.SQL, "NOT") || !strings.Contains(r.SQL, "REGEXP") {
		t.Errorf("SQL should use NOT ... REGEXP: %s", r.SQL)
	}
}

func TestNot_matchesCaseInsensitive(t *testing.T) {
	r := mustParse(t, `NOT (message MATCHES* "debug")`)
	if !strings.Contains(r.SQL, "NOT") || !strings.Contains(r.SQL, "REGEXP") {
		t.Errorf("SQL should use NOT ... REGEXP: %s", r.SQL)
	}
	arg, _ := r.Args[0].(string)
	if !strings.HasPrefix(arg, "(?i)") {
		t.Errorf("case-insensitive pattern should start with (?i): %q", arg)
	}
}

func TestNot_contains(t *testing.T) {
	r := mustParse(t, `NOT (message CONTAINS "health")`)
	if !strings.Contains(r.SQL, "NOT") || !strings.Contains(r.SQL, "LIKE") {
		t.Errorf("SQL should use NOT ... LIKE: %s", r.SQL)
	}
	if r.Args[0] != "%health%" {
		t.Errorf("args[0] = %q, want %%health%%", r.Args[0])
	}
}

func TestNot_in(t *testing.T) {
	r := mustParse(t, `NOT (source IN (account, residence))`)
	if !strings.Contains(r.SQL, "NOT") || !strings.Contains(r.SQL, "IN") {
		t.Errorf("SQL should use NOT ... IN: %s", r.SQL)
	}
	if len(r.Args) != 2 {
		t.Errorf("expected 2 args, got %d: %v", len(r.Args), r.Args)
	}
}

func TestNot_simple(t *testing.T) {
	r := mustParse(t, `NOT level = error`)
	if !strings.Contains(r.SQL, "NOT") {
		t.Errorf("SQL should use NOT: %s", r.SQL)
	}
}

// --- SQLite field access ---

func TestJSONBField(t *testing.T) {
	r := mustParse(t, "data.status=500")
	if !strings.Contains(r.SQL, "json_extract(data, '$.status')") {
		t.Errorf("SQL missing json_extract accessor: %s", r.SQL)
	}
}

func TestJSONBField_matches(t *testing.T) {
	r := mustParse(t, `data.path MATCHES "^/api"`)
	if !strings.Contains(r.SQL, "json_extract(data, '$.path')") {
		t.Errorf("SQL missing json_extract accessor: %s", r.SQL)
	}
	if !strings.Contains(r.SQL, "REGEXP") {
		t.Errorf("SQL missing REGEXP: %s", r.SQL)
	}
}

// --- Relative time ---

func TestRelativeTime_minutes(t *testing.T) {
	r := mustParse(t, "log_time > 10m")
	// SQLite mode: cutoff is passed as an arg, not computed in SQL
	if len(r.Args) != 1 {
		t.Errorf("expected 1 arg (cutoff string), got %d", len(r.Args))
	}
	if !strings.Contains(r.SQL, "?") {
		t.Errorf("SQL should have ? placeholder: %s", r.SQL)
	}
}

func TestRelativeTime_hours(t *testing.T) {
	r := mustParse(t, "log_time > 2h")
	if len(r.Args) != 1 {
		t.Errorf("expected 1 arg (cutoff string), got %d", len(r.Args))
	}
}

func TestRelativeTime_days(t *testing.T) {
	r := mustParse(t, "log_time > 1d")
	if len(r.Args) != 1 {
		t.Errorf("expected 1 arg (cutoff string), got %d", len(r.Args))
	}
}

func TestRelativeTime_fallbackToIngestedAt(t *testing.T) {
	r := mustParse(t, "log_time > 1h")
	if !strings.Contains(r.SQL, "ingested_at") {
		t.Errorf("SQL should fall back to ingested_at: %s", r.SQL)
	}
}

// --- Boolean operators ---

func TestAnd(t *testing.T) {
	r := mustParse(t, "source=storage AND level=error")
	if !strings.Contains(r.SQL, "AND") {
		t.Errorf("SQL missing AND: %s", r.SQL)
	}
	if len(r.Args) != 2 {
		t.Errorf("expected 2 args, got %d", len(r.Args))
	}
}

func TestOr(t *testing.T) {
	r := mustParse(t, "level=error OR level=fatal")
	if !strings.Contains(r.SQL, "OR") {
		t.Errorf("SQL missing OR: %s", r.SQL)
	}
}

func TestGrouping(t *testing.T) {
	r := mustParse(t, "(source=storage OR source=account) AND level=error")
	if !strings.Contains(r.SQL, "AND") || !strings.Contains(r.SQL, "OR") {
		t.Errorf("SQL missing expected operators: %s", r.SQL)
	}
	if len(r.Args) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(r.Args), r.Args)
	}
}

// --- startArg is ignored (SQLite uses ? positional placeholders) ---

func TestStartArgOffset(t *testing.T) {
	// startArg is accepted for API compat but ignored; all placeholders are ?
	r, err := Parse("source=storage", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(r.SQL, "?") {
		t.Errorf("SQL should contain ? placeholder: %s", r.SQL)
	}
	if strings.Contains(r.SQL, "$") {
		t.Errorf("SQL should not contain $ placeholders: %s", r.SQL)
	}
}

// --- Compound real-world expressions ---

func TestComplex_realWorld(t *testing.T) {
	cases := []string{
		`source IN (storage, account) AND level!=info AND log_time > 1h`,
		`(source=storage OR source=account) AND level=error`,
		`message MATCHES "^(ERROR|FATAL)" AND log_time > 30m`,
		`data.status=500 AND source=storage`,
		`level=error AND NOT (message MATCHES* "health")`,
	}
	for _, expr := range cases {
		t.Run(expr, func(t *testing.T) {
			r, err := Parse(expr, 1)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", expr, err)
			}
			if r.SQL == "" {
				t.Error("expected non-empty SQL")
			}
			// Arg count must match placeholder count in SQL
			count := strings.Count(r.SQL, "?")
			if count != len(r.Args) {
				t.Errorf("placeholder count %d != arg count %d in SQL: %s", count, len(r.Args), r.SQL)
			}
		})
	}
}

// --- Error cases ---

func TestErrors(t *testing.T) {
	bad := []string{
		"",                  // empty — should return empty/err gracefully or trivially
		"source",            // field with no operator
		"source IN storage", // IN without parens
	}
	for _, expr := range bad {
		t.Run(expr, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Parse(%q) panicked: %v", expr, r)
				}
			}()
			Parse(expr, 1) //nolint:errcheck
		})
	}
}

// --- parseDuration ---

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in      string
		wantSec float64
		wantOk  bool
	}{
		{"10m", 600, true},
		{"1h", 3600, true},
		{"2h", 7200, true},
		{"1d", 86400, true},
		{"30s", 30, true},
		{"bad", 0, false},
		{"m", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		d, ok := parseDuration(c.in)
		if ok != c.wantOk {
			t.Errorf("parseDuration(%q) ok=%v, want %v", c.in, ok, c.wantOk)
			continue
		}
		if ok && d.Seconds() != c.wantSec {
			t.Errorf("parseDuration(%q) = %v sec, want %v", c.in, d.Seconds(), c.wantSec)
		}
	}
}
