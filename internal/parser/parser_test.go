package parser

import (
	"testing"
	"time"
)

// Real log lines captured from the running containers.

func TestStripDockerPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"storage-1  | <- [200] /health", "<- [200] /health"},
		{"account-1  | checking GET '/health'", "checking GET '/health'"},
		{"synapse-1  | 2026-04-17 23:20:58,792 - synapse - INFO - msg", "2026-04-17 23:20:58,792 - synapse - INFO - msg"},
		{"no-prefix line", "no-prefix line"},
		{"svc-10  | indented", "indented"},
	}
	for _, c := range cases {
		got := stripDockerPrefix(c.in)
		if got != c.want {
			t.Errorf("stripDockerPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- JSON parser ---

func TestParseJSON_basic(t *testing.T) {
	raw := `{"level":"info","msg":"server started","port":8080}`
	r := Parse("myapp", raw)
	if r.Source != "myapp" {
		t.Errorf("source = %q, want myapp", r.Source)
	}
	if r.Level != "INFO" {
		t.Errorf("level = %q, want INFO", r.Level)
	}
	if r.Message != "server started" {
		t.Errorf("message = %q, want 'server started'", r.Message)
	}
	if r.Data["_raw"] != raw {
		t.Errorf("_raw not preserved")
	}
	if r.Data["port"] != float64(8080) {
		t.Errorf("extra field 'port' not preserved")
	}
}

func TestParseJSON_timestampFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"time field", `{"time":"2026-04-17T23:20:58Z","level":"error","message":"boom"}`},
		{"timestamp field", `{"timestamp":"2026-04-17T23:20:58Z","severity":"error","msg":"boom"}`},
		{"ts field", `{"ts":"2026-04-17T23:20:58Z","lvl":"error","message":"boom"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Parse("svc", c.raw)
			if r.LogTime == nil {
				t.Error("expected LogTime to be parsed, got nil")
			}
			if r.Level != "ERROR" {
				t.Errorf("level = %q, want ERROR", r.Level)
			}
			if r.Message != "boom" {
				t.Errorf("message = %q, want boom", r.Message)
			}
		})
	}
}

func TestParseJSON_dockerPrefix(t *testing.T) {
	raw := `storage-1  | {"level":"warn","msg":"slow query","ms":320}`
	r := Parse("storage", raw)
	if r.Level != "WARN" {
		t.Errorf("level = %q, want WARN", r.Level)
	}
	if r.Message != "slow query" {
		t.Errorf("message = %q, want 'slow query'", r.Message)
	}
}

func TestParseJSON_noMessage_usesRawLine(t *testing.T) {
	raw := `{"foo":"bar","count":3}`
	r := Parse("svc", raw)
	if r.Message == "" {
		t.Error("expected non-empty message fallback for JSON without msg field")
	}
}

// --- Synapse / Python logging ---

func TestParseSynapse_realLine(t *testing.T) {
	raw := `synapse-1  | 2026-04-17 23:20:58,792 - synapse.access.http.8008 - 643 - INFO - 127.0.0.1 - 8008 - {None} Processed request: 0.000sec/0.000sec 200 "GET /_matrix/client/versions HTTP/1.1"`
	r := Parse("synapse", raw)
	if r.Level != "INFO" {
		t.Errorf("level = %q, want INFO", r.Level)
	}
	if r.LogTime == nil {
		t.Error("expected LogTime to be parsed from synapse line")
	}
	if r.Source != "synapse" {
		t.Errorf("source = %q, want synapse", r.Source)
	}
	if r.Data["logger"] != "synapse.access.http.8008" {
		t.Errorf("logger field = %q, want 'synapse.access.http.8008'", r.Data["logger"])
	}
	wantYear := 2026
	if r.LogTime.Year() != wantYear {
		t.Errorf("LogTime year = %d, want %d", r.LogTime.Year(), wantYear)
	}
}

func TestParseSynapse_levels(t *testing.T) {
	cases := []struct{ raw, wantLevel string }{
		{`2026-04-17 10:00:00,000 - synapse.foo - 1 - DEBUG - msg`, "DEBUG"},
		{`2026-04-17 10:00:00,000 - synapse.foo - 1 - WARNING - msg`, "WARN"},
		{`2026-04-17 10:00:00,000 - synapse.foo - 1 - ERROR - msg`, "ERROR"},
		{`2026-04-17 10:00:00,000 - synapse.foo - 1 - CRITICAL - msg`, "ERROR"},
	}
	for _, c := range cases {
		r := Parse("synapse", c.raw)
		if r.Level != c.wantLevel {
			t.Errorf("line %q: level = %q, want %q", c.raw, r.Level, c.wantLevel)
		}
	}
}

// --- uvicorn / FastAPI ---

func TestParseUvicorn_accessLog(t *testing.T) {
	cases := []struct {
		raw        string
		wantMethod string
		wantPath   string
		wantStatus string
		wantLevel  string
	}{
		{
			raw:        `assistant-1  | INFO:     127.0.0.1:59368 - "GET /health HTTP/1.1" 200 OK`,
			wantMethod: "GET",
			wantPath:   "/health",
			wantStatus: "200",
			wantLevel:  "INFO",
		},
		{
			raw:        `assistant-1  | WARNING:     10.0.0.1:12345 - "POST /api/v1/chat HTTP/1.1" 422 Unprocessable Entity`,
			wantMethod: "POST",
			wantPath:   "/api/v1/chat",
			wantStatus: "422",
			wantLevel:  "WARN",
		},
	}
	for _, c := range cases {
		r := Parse("assistant", c.raw)
		if r.Level != c.wantLevel {
			t.Errorf("level = %q, want %q (raw: %s)", r.Level, c.wantLevel, c.raw)
		}
		if r.Data["method"] != c.wantMethod {
			t.Errorf("method = %q, want %q", r.Data["method"], c.wantMethod)
		}
		if r.Data["path"] != c.wantPath {
			t.Errorf("path = %q, want %q", r.Data["path"], c.wantPath)
		}
		if r.Data["status"] != c.wantStatus {
			t.Errorf("status = %q, want %q", r.Data["status"], c.wantStatus)
		}
	}
}

func TestParseUvicorn_logLine(t *testing.T) {
	raw := `INFO:root:Application startup complete`
	r := Parse("assistant", raw)
	if r.Level != "INFO" {
		t.Errorf("level = %q, want INFO", r.Level)
	}
	if r.Message != "Application startup complete" {
		t.Errorf("message = %q, want 'Application startup complete'", r.Message)
	}
}

// --- Node.js router ---

func TestParseNodeRouter(t *testing.T) {
	cases := []struct {
		raw           string
		wantDirection string
		wantMethod    string
	}{
		{"storage-1  | -> [GET] /health", "request", "GET"},
		{"account-1  | -> [POST] /auth", "request", "POST"},
		{"storage-1  | <- [200] /health", "response", "200"},
		{"account-1  | <- [403] /auth", "response", "403"},
	}
	for _, c := range cases {
		r := Parse("storage", c.raw)
		if r.Data["direction"] != c.wantDirection {
			t.Errorf("%q: direction = %q, want %q", c.raw, r.Data["direction"], c.wantDirection)
		}
		if r.Data["method"] != c.wantMethod {
			t.Errorf("%q: method = %q, want %q", c.raw, r.Data["method"], c.wantMethod)
		}
		if r.Level != "INFO" {
			t.Errorf("%q: level = %q, want INFO", c.raw, r.Level)
		}
	}
}

// --- Generic fallback ---

func TestParseGeneric(t *testing.T) {
	raw := "checking GET '/health'"
	r := Parse("storage", raw)
	if r.Message != raw {
		t.Errorf("message = %q, want %q", r.Message, raw)
	}
	if r.Level != "" {
		t.Errorf("level = %q, want empty string for generic fallback", r.Level)
	}
	if r.Data["_raw"] != raw {
		t.Error("_raw not set on generic fallback")
	}
}

func TestParseGeneric_dockerPrefixStripped(t *testing.T) {
	raw := "storage-1  | routing GET '/health'"
	r := Parse("storage", raw)
	if r.Message == raw {
		t.Error("docker prefix should be stripped from message")
	}
}

// --- Level normalisation ---

func TestNormaliseLevel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"info", "INFO"},
		{"INFO", "INFO"},
		{"warn", "WARN"},
		{"warning", "WARN"},
		{"WARNING", "WARN"},
		{"error", "ERROR"},
		{"ERROR", "ERROR"},
		{"err", "ERROR"},
		{"critical", "ERROR"},
		{"fatal", "FATAL"},
		{"panic", "FATAL"},
		{"debug", "DEBUG"},
		{"trace", "TRACE"},
		{"notice", "INFO"},
		{"unknown", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := normaliseLevel(c.in)
		if got != c.want {
			t.Errorf("normaliseLevel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- Time parsing ---

func TestTryParseTime(t *testing.T) {
	valid := []string{
		"2026-04-17T23:20:58Z",
		"2026-04-17T23:20:58.123456789Z",
		"2026-04-17 23:20:58.792",
		"2026-04-17 23:20:58",
	}
	for _, s := range valid {
		if got := tryParseTime(s); got == nil {
			t.Errorf("tryParseTime(%q) returned nil, want non-nil", s)
		}
	}

	invalid := []string{"not-a-date", "", "2026/04/17"}
	for _, s := range invalid {
		if got := tryParseTime(s); got != nil {
			t.Errorf("tryParseTime(%q) = %v, want nil", s, got)
		}
	}

	// Verify a specific value parses correctly
	ts := tryParseTime("2026-04-17T23:20:58Z")
	if ts == nil {
		t.Fatal("unexpected nil")
	}
	want := time.Date(2026, 4, 17, 23, 20, 58, 0, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("parsed time = %v, want %v", ts, want)
	}
}
