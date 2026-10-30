package cdp

import (
	"encoding/json"
	"testing"
	"time"
)

// --- cdpLevelToLevel ---

func TestCdpLevelToLevel(t *testing.T) {
	cases := [][2]string{
		{"verbose", "DEBUG"},
		{"debug", "DEBUG"},
		{"info", "INFO"},
		{"log", "INFO"},
		{"dir", "INFO"},
		{"dirxml", "INFO"},
		{"table", "INFO"},
		{"group", "INFO"},
		{"groupcollapsed", "INFO"},
		{"count", "INFO"},
		{"timeend", "INFO"},
		{"warning", "WARN"},
		{"warn", "WARN"},
		{"error", "ERROR"},
		{"assert", "ERROR"},
		{"INFO", "INFO"}, // uppercase
		{"WARNING", "WARN"},
		{"unknown_type", "INFO"},
	}
	for _, c := range cases {
		got := cdpLevelToLevel(c[0])
		if got != c[1] {
			t.Errorf("cdpLevelToLevel(%q)=%q want %q", c[0], got, c[1])
		}
	}
}

// --- SourceName ---

func TestSourceName(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		target   Target
		want     string
	}{
		{
			name:     "node target with file URL",
			endpoint: "http://localhost:9229",
			target:   Target{Type: "node", URL: "file:///app/dist/main.js"},
			want:     "node:main.js",
		},
		{
			name:     "node target with simple path",
			endpoint: "http://localhost:9230",
			target:   Target{Type: "node", URL: "file:///server.js"},
			want:     "node:server.js",
		},
		{
			name:     "node target fallback",
			endpoint: "http://localhost:9229",
			target:   Target{Type: "node", URL: ""},
			want:     "node",
		},
		{
			name:     "browser page with subdomain host",
			endpoint: "http://localhost:9222",
			target:   Target{Type: "page", URL: "http://app.localhost.example.test/home"},
			want:     "browser:app",
		},
		{
			name:     "browser page simple host",
			endpoint: "http://localhost:9222",
			target:   Target{Type: "page", URL: "http://localhost:3000/"},
			want:     "browser:localhost",
		},
		{
			name:     "browser with title fallback",
			endpoint: "http://localhost:9222",
			target:   Target{Type: "page", Title: "My App"},
			want:     "browser:My-App",
		},
		{
			name:     "browser title truncated",
			endpoint: "http://localhost:9222",
			target:   Target{Type: "page", Title: "This Is A Very Long Title That Should Be Trimmed"},
			want:     "browser:This-Is-A-Very-Long",
		},
		{
			name:     "browser no URL no title",
			endpoint: "http://localhost:9222",
			target:   Target{Type: "page"},
			want:     "browser",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SourceName(tc.endpoint, tc.target)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// --- typeAllowed ---

func TestTypeAllowed(t *testing.T) {
	if !typeAllowed("page", nil) {
		t.Error("empty allowed list should allow everything")
	}
	if !typeAllowed("page", []string{"page", "node"}) {
		t.Error("page should be allowed")
	}
	if typeAllowed("iframe", []string{"page", "node"}) {
		t.Error("iframe should not be allowed")
	}
	if !typeAllowed("node", []string{"node"}) {
		t.Error("node should be allowed")
	}
}

// --- handleLogEntry ---

func TestHandleLogEntry(t *testing.T) {
	t.Run("basic error entry", func(t *testing.T) {
		raw := json.RawMessage(`{
			"entry": {
				"level": "error",
				"text": "something went wrong",
				"source": "javascript",
				"url": "http://app/index.js",
				"lineNumber": 42,
				"timestamp": 1713400000000
			}
		}`)
		target := Target{Type: "page", URL: "http://app/"}
		r, ok := handleLogEntry(raw, "browser:app", target)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if r.Level != "ERROR" {
			t.Errorf("level=%q want ERROR", r.Level)
		}
		if r.Message != "something went wrong" {
			t.Errorf("message=%q", r.Message)
		}
		if r.LogTime == nil {
			t.Error("expected non-nil log_time")
		} else {
			want := time.UnixMilli(1713400000000)
			if !r.LogTime.Equal(want) {
				t.Errorf("log_time=%v want %v", r.LogTime, want)
			}
		}
		if r.Data["url"] != "http://app/index.js" {
			t.Errorf("url=%v", r.Data["url"])
		}
	})

	t.Run("empty text returns false", func(t *testing.T) {
		raw := json.RawMessage(`{"entry": {"level": "info", "text": "", "timestamp": 0}}`)
		_, ok := handleLogEntry(raw, "src", Target{})
		if ok {
			t.Error("empty text should return false")
		}
	})

	t.Run("no url omits url/line fields", func(t *testing.T) {
		raw := json.RawMessage(`{"entry": {"level": "info", "text": "hello"}}`)
		r, ok := handleLogEntry(raw, "src", Target{})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if _, has := r.Data["url"]; has {
			t.Error("url should not be set when entry.url is empty")
		}
	})
}

// --- handleConsoleAPI ---

func TestHandleConsoleAPI(t *testing.T) {
	t.Run("string args joined", func(t *testing.T) {
		raw := json.RawMessage(`{
			"type": "log",
			"timestamp": 1713400001000,
			"args": [
				{"type": "string", "value": "hello"},
				{"type": "string", "value": "world"}
			]
		}`)
		r, ok := handleConsoleAPI(raw, "node:server.js", Target{})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if r.Message != "hello world" {
			t.Errorf("message=%q want 'hello world'", r.Message)
		}
		if r.Level != "INFO" {
			t.Errorf("level=%q want INFO", r.Level)
		}
	})

	t.Run("warn type maps to WARN", func(t *testing.T) {
		raw := json.RawMessage(`{
			"type": "warning",
			"timestamp": 0,
			"args": [{"type": "string", "value": "careful"}]
		}`)
		r, ok := handleConsoleAPI(raw, "src", Target{})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if r.Level != "WARN" {
			t.Errorf("level=%q want WARN", r.Level)
		}
	})

	t.Run("object arg uses description", func(t *testing.T) {
		raw := json.RawMessage(`{
			"type": "log",
			"timestamp": 0,
			"args": [{"type": "object", "description": "Error: boom"}]
		}`)
		r, ok := handleConsoleAPI(raw, "src", Target{})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if r.Message != "Error: boom" {
			t.Errorf("message=%q", r.Message)
		}
	})

	t.Run("stack trace first frame extracted", func(t *testing.T) {
		raw := json.RawMessage(`{
			"type": "error",
			"timestamp": 0,
			"args": [{"type": "string", "value": "oops"}],
			"stackTrace": {
				"callFrames": [
					{"functionName": "handleReq", "url": "http://app/server.js", "lineNumber": 10, "columnNumber": 5}
				]
			}
		}`)
		r, ok := handleConsoleAPI(raw, "src", Target{})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if r.Data["url"] != "http://app/server.js" {
			t.Errorf("url=%v", r.Data["url"])
		}
		if r.Data["function"] != "handleReq" {
			t.Errorf("function=%v", r.Data["function"])
		}
	})

	t.Run("empty args returns false", func(t *testing.T) {
		raw := json.RawMessage(`{"type": "log", "timestamp": 0, "args": []}`)
		_, ok := handleConsoleAPI(raw, "src", Target{})
		if ok {
			t.Error("no args should return false")
		}
	})
}

// --- handleException ---

func TestHandleException(t *testing.T) {
	t.Run("exception with description", func(t *testing.T) {
		raw := json.RawMessage(`{
			"timestamp": 1713400002000,
			"exceptionDetails": {
				"text": "Uncaught",
				"lineNumber": 5,
				"columnNumber": 1,
				"url": "http://app/main.js",
				"exception": {
					"description": "TypeError: Cannot read property 'foo' of undefined\n    at main.js:5:1"
				}
			}
		}`)
		r, ok := handleException(raw, "browser:app", Target{})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if r.Level != "ERROR" {
			t.Errorf("level=%q want ERROR", r.Level)
		}
		if r.Message != "TypeError: Cannot read property 'foo' of undefined\n    at main.js:5:1" {
			t.Errorf("message=%q", r.Message)
		}
		if r.Data["url"] != "http://app/main.js" {
			t.Errorf("url=%v", r.Data["url"])
		}
	})

	t.Run("fallback to text when no exception description", func(t *testing.T) {
		raw := json.RawMessage(`{
			"timestamp": 0,
			"exceptionDetails": {
				"text": "SyntaxError: unexpected token"
			}
		}`)
		r, ok := handleException(raw, "src", Target{})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if r.Message != "SyntaxError: unexpected token" {
			t.Errorf("message=%q", r.Message)
		}
	})

	t.Run("empty text uses default message", func(t *testing.T) {
		raw := json.RawMessage(`{"timestamp": 0, "exceptionDetails": {}}`)
		r, ok := handleException(raw, "src", Target{})
		if !ok {
			t.Fatal("expected ok=true")
		}
		if r.Message != "Uncaught exception" {
			t.Errorf("message=%q", r.Message)
		}
	})

	t.Run("timestamp set correctly", func(t *testing.T) {
		raw := json.RawMessage(`{
			"timestamp": 1713400003000,
			"exceptionDetails": {"text": "err"}
		}`)
		r, _ := handleException(raw, "src", Target{})
		if r.LogTime == nil {
			t.Fatal("expected non-nil log_time")
		}
		want := time.UnixMilli(1713400003000)
		if !r.LogTime.Equal(want) {
			t.Errorf("log_time=%v want %v", r.LogTime, want)
		}
	})
}

// --- handleEvent dispatch ---

func TestHandleEvent(t *testing.T) {
	target := Target{Type: "page", URL: "http://app/"}

	t.Run("Log.entryAdded dispatched", func(t *testing.T) {
		m := cdpMsg{
			Method: "Log.entryAdded",
			Params: json.RawMessage(`{"entry":{"level":"info","text":"hello"}}`),
		}
		_, ok := handleEvent(m, "src", target)
		if !ok {
			t.Error("expected ok=true")
		}
	})

	t.Run("Runtime.consoleAPICalled dispatched", func(t *testing.T) {
		m := cdpMsg{
			Method: "Runtime.consoleAPICalled",
			Params: json.RawMessage(`{"type":"log","timestamp":0,"args":[{"type":"string","value":"hi"}]}`),
		}
		_, ok := handleEvent(m, "src", target)
		if !ok {
			t.Error("expected ok=true")
		}
	})

	t.Run("Runtime.exceptionThrown dispatched", func(t *testing.T) {
		m := cdpMsg{
			Method: "Runtime.exceptionThrown",
			Params: json.RawMessage(`{"timestamp":0,"exceptionDetails":{"text":"boom"}}`),
		}
		_, ok := handleEvent(m, "src", target)
		if !ok {
			t.Error("expected ok=true")
		}
	})

	t.Run("unknown method returns false", func(t *testing.T) {
		m := cdpMsg{Method: "Debugger.paused", Params: json.RawMessage(`{}`)}
		_, ok := handleEvent(m, "src", target)
		if ok {
			t.Error("unknown method should return false")
		}
	})
}
