package parser

import (
	"testing"
)

func TestParseChromium(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantLvl  string
		wantMsg  string
		wantPID  int64
		wantTID  int64
		wantFile string
	}{
		{
			name:     "error with message",
			line:     "[3009052:3009058:0418/054625.874179:ERROR:net/socket/ssl_client_socket_impl.cc(924)] handshake failed; returned -1, SSL error code 1, net_error -101",
			wantOK:   true,
			wantLvl:  "ERROR",
			wantMsg:  "handshake failed; returned -1, SSL error code 1, net_error -101",
			wantPID:  3009052,
			wantTID:  3009058,
			wantFile: "net/socket/ssl_client_socket_impl.cc",
		},
		{
			name:     "warning",
			line:     "[1234:5678:0101/120000.000000:WARNING:foo/bar.cc(42)] something odd",
			wantOK:   true,
			wantLvl:  "WARN",
			wantMsg:  "something odd",
			wantPID:  1234,
			wantTID:  5678,
			wantFile: "foo/bar.cc",
		},
		{
			name:     "info",
			line:     "[100:200:0418/090000.000000:INFO:chrome/browser.cc(1)] started",
			wantOK:   true,
			wantLvl:  "INFO",
			wantMsg:  "started",
			wantPID:  100,
			wantTID:  200,
			wantFile: "chrome/browser.cc",
		},
		{
			name:     "verbose maps to debug",
			line:     "[1:2:0418/090000.000000:VERBOSE1:renderer.cc(10)] verbose message",
			wantOK:   true,
			wantLvl:  "DEBUG",
			wantMsg:  "verbose message",
			wantPID:  1,
			wantTID:  2,
			wantFile: "renderer.cc",
		},
		{
			name:     "fatal",
			line:     "[999:888:1231/235959.999999:FATAL:base/logging.cc(500)] critical failure",
			wantOK:   true,
			wantLvl:  "FATAL",
			wantMsg:  "critical failure",
			wantPID:  999,
			wantTID:  888,
			wantFile: "base/logging.cc",
		},
		{
			name:    "empty message uses fallback",
			line:    "[1:2:0418/090000.000000:ERROR:some/file.cc(7)] ",
			wantOK:  true,
			wantLvl: "ERROR",
			wantMsg: "ERROR in some/file.cc(7)",
		},
		{
			name:   "not a chromium line",
			line:   "some random log line",
			wantOK: false,
		},
		{
			name:   "uvicorn line does not match",
			line:   "INFO:     127.0.0.1:52000 - \"GET / HTTP/1.1\" 200 OK",
			wantOK: false,
		},
		{
			name:   "synapse line does not match",
			line:   "2026-04-17 23:20:58,792 - synapse.access - 643 - INFO - message",
			wantOK: false,
		},
		{
			name:     "leading whitespace stripped",
			line:     "  [1:2:0418/090000.000000:INFO:x.cc(1)] msg  ",
			wantOK:   true,
			wantLvl:  "INFO",
			wantMsg:  "msg",
			wantPID:  1,
			wantTID:  2,
			wantFile: "x.cc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := parseChromium("chrome", tc.line, tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if r.Level != tc.wantLvl {
				t.Errorf("level=%q want %q", r.Level, tc.wantLvl)
			}
			if r.Message != tc.wantMsg {
				t.Errorf("message=%q want %q", r.Message, tc.wantMsg)
			}
			if tc.wantPID != 0 {
				if pid, _ := r.Data["pid"].(int64); pid != tc.wantPID {
					t.Errorf("pid=%d want %d", pid, tc.wantPID)
				}
			}
			if tc.wantTID != 0 {
				if tid, _ := r.Data["tid"].(int64); tid != tc.wantTID {
					t.Errorf("tid=%d want %d", tid, tc.wantTID)
				}
			}
			if tc.wantFile != "" {
				if f, _ := r.Data["file"].(string); f != tc.wantFile {
					t.Errorf("file=%q want %q", f, tc.wantFile)
				}
				if _, hasLine := r.Data["line"]; !hasLine {
					t.Errorf("expected data[line] to be set")
				}
			}
		})
	}
}

func TestChromiumLevel(t *testing.T) {
	cases := [][2]string{
		{"VERBOSE", "DEBUG"},
		{"VERBOSE1", "DEBUG"},
		{"VERBOSE2", "DEBUG"},
		{"INFO", "INFO"},
		{"WARNING", "WARN"},
		{"ERROR", "ERROR"},
		{"FATAL", "FATAL"},
		{"unknown", "INFO"},
	}
	for _, c := range cases {
		got := chromiumLevel(c[0])
		if got != c[1] {
			t.Errorf("chromiumLevel(%q)=%q want %q", c[0], got, c[1])
		}
	}
}

func TestTryParseChromiumTime(t *testing.T) {
	t.Run("valid time", func(t *testing.T) {
		got := tryParseChromiumTime("0418", "054625.874179")
		if got == nil {
			t.Fatal("expected non-nil time")
		}
		if got.Month() != 4 || got.Day() != 18 {
			t.Errorf("unexpected date: %v", got)
		}
		if got.Hour() != 5 || got.Minute() != 46 || got.Second() != 25 {
			t.Errorf("unexpected time: %v", got)
		}
	})

	t.Run("short mmdd returns nil", func(t *testing.T) {
		if tryParseChromiumTime("041", "054625.874179") != nil {
			t.Error("expected nil for short mmdd")
		}
	})

	t.Run("hhmmssus without fractional still parses", func(t *testing.T) {
		// Go treats .999999 as optional in format strings, so bare HHMMSS succeeds
		got := tryParseChromiumTime("0418", "054625")
		if got == nil {
			t.Error("expected non-nil time for bare HHMMSS")
		}
	})

	t.Run("empty inputs return nil", func(t *testing.T) {
		if tryParseChromiumTime("", "") != nil {
			t.Error("expected nil for empty inputs")
		}
	})
}
