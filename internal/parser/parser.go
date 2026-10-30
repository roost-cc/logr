// Package parser converts raw log lines into structured Records.
// The chain is tried in order; the first match wins.
package parser

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/roost-cc/logr/internal/db"
)

// Parse attempts to extract structure from a raw log line using the full parser chain.
// source is the service/process name (e.g. "storage", "myapp").
func Parse(source, raw string) db.Record {
	// Strip Docker Compose prefix: "service-N  | actual line"
	line := stripDockerPrefix(raw)

	for _, p := range parsers {
		if r, ok := p(source, line, raw); ok {
			return r
		}
	}
	return generic(source, line, raw)
}

// ParseWith applies a named parser, falling back to generic if the line doesn't match.
// Names: "auto" (full chain), "json", "uvicorn"/"nginx", "synapse", "llama",
// "chromium". Anything else (including "line") wraps the raw line as a message.
func ParseWith(name, source, raw string) db.Record {
	line := stripDockerPrefix(raw)
	switch name {
	case "auto":
		return Parse(source, raw)
	case "json":
		if r, ok := parseJSON(source, line, raw); ok {
			return r
		}
	case "uvicorn", "nginx":
		if r, ok := parseUvicorn(source, line, raw); ok {
			return r
		}
	case "synapse":
		if r, ok := parseSynapse(source, line, raw); ok {
			return r
		}
	case "llama":
		if r, ok := parseLlama(source, line, raw); ok {
			return r
		}
	case "chromium":
		if r, ok := parseChromium(source, line, raw); ok {
			return r
		}
	}
	return generic(source, line, raw)
}

// ParseLine wraps a raw line as a plain message with no format detection.
func ParseLine(source, raw string) db.Record {
	line := stripDockerPrefix(raw)
	return generic(source, line, raw)
}

var parsers = []func(source, line, raw string) (db.Record, bool){
	parseJSON,
	parseSynapse,
	parseUvicorn,
	parseLlama,
	parseChromium,
	parseNodeRouter,
}

// stripDockerPrefix removes the "service-N  | " prefix added by docker compose logs.
var dockerPrefixRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+-\d+\s+\|\s?`)

func stripDockerPrefix(line string) string {
	return dockerPrefixRe.ReplaceAllString(line, "")
}

// --- JSON parser ---

var jsonFields = [][]string{
	{"time", "timestamp", "ts", "@timestamp"},
	{"level", "severity", "lvl"},
	{"message", "msg", "text"},
}

func parseJSON(source, line, raw string) (db.Record, bool) {
	line = strings.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return db.Record{}, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return db.Record{}, false
	}

	r := db.Record{
		Source: source,
		Data:   m,
	}
	r.Data["_raw"] = raw

	// Extract well-known fields
	for _, candidates := range jsonFields {
		for _, k := range candidates {
			v, ok := m[k]
			if !ok {
				continue
			}
			switch candidates[0] {
			case "time":
				if s, ok := v.(string); ok {
					if t := tryParseTime(s); t != nil {
						r.LogTime = t
					}
				}
			case "level":
				if s, ok := v.(string); ok {
					r.Level = normaliseLevel(s)
				}
			case "message":
				if s, ok := v.(string); ok {
					r.Message = s
				}
			}
			break
		}
	}
	if r.Message == "" {
		r.Message = line
	}
	return r, true
}

// --- Synapse / Python logging format ---
// 2026-04-17 23:20:58,792 - synapse.access.http.8008 - 643 - INFO - ...

var synapseRe = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2},\d+)\s+-\s+(\S+)\s+-\s+\d+\s+-\s+(\w+)\s+-\s+(.+)$`,
)

func parseSynapse(source, line, raw string) (db.Record, bool) {
	m := synapseRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return db.Record{}, false
	}
	t := tryParseTime(strings.Replace(m[1], ",", ".", 1))
	msg := strings.TrimSpace(m[4])
	return db.Record{
		LogTime: t,
		Source:  source,
		Level:   normaliseLevel(m[3]),
		Message: msg,
		Data: map[string]any{
			"logger": m[2],
			"_raw":   raw,
		},
	}, true
}

// --- uvicorn / FastAPI / Python logging ---
// INFO:     127.0.0.1:PORT - "METHOD path HTTP/1.1" STATUS REASON
// INFO:root:message

var uvicornAccessRe = regexp.MustCompile(
	`^(\w+):\s{1,5}(\S+)\s+-\s+"(\w+)\s+(\S+)\s+HTTP/[\d.]+"\s+(\d{3})\s+(.*)$`,
)
var uvicornLogRe = regexp.MustCompile(`^(\w+):[\w.]*:(.+)$`)

func parseUvicorn(source, line, raw string) (db.Record, bool) {
	line = strings.TrimSpace(line)

	if m := uvicornAccessRe.FindStringSubmatch(line); m != nil {
		return db.Record{
			Source:  source,
			Level:   normaliseLevel(m[1]),
			Message: m[3] + " " + m[4] + " " + m[5],
			Data: map[string]any{
				"method": m[3],
				"path":   m[4],
				"status": m[5],
				"client": m[2],
				"_raw":   raw,
			},
		}, true
	}

	if m := uvicornLogRe.FindStringSubmatch(line); m != nil {
		lvl := normaliseLevel(m[1])
		if lvl == "" {
			return db.Record{}, false
		}
		return db.Record{
			Source:  source,
			Level:   lvl,
			Message: strings.TrimSpace(m[2]),
			Data:    map[string]any{"_raw": raw},
		}, true
	}

	return db.Record{}, false
}

// --- Node.js router plain-text ---
// -> [GET] /health
// <- [200] /health
// routing GET '/health'
// checking GET '/health'
// found : GET '/health' -> Controller.method

var nodeArrowRe = regexp.MustCompile(`^([<>-]+)\s+\[(\w+)\]\s+(.+)$`)

func parseNodeRouter(source, line, raw string) (db.Record, bool) {
	line = strings.TrimSpace(line)
	m := nodeArrowRe.FindStringSubmatch(line)
	if m == nil {
		return db.Record{}, false
	}
	direction := "request"
	if strings.HasPrefix(m[1], "<") {
		direction = "response"
	}
	return db.Record{
		Source:  source,
		Level:   "INFO",
		Message: line,
		Data: map[string]any{
			"direction": direction,
			"method":    m[2],
			"path":      strings.TrimSpace(m[3]),
			"_raw":      raw,
		},
	}, true
}

// --- Generic fallback ---

func generic(source, line, raw string) db.Record {
	msg := strings.TrimSpace(line)
	if msg == "" {
		msg = strings.TrimSpace(raw)
	}
	return db.Record{
		Source:  source,
		Message: msg,
		Data:    map[string]any{"_raw": raw},
	}
}

// --- Helpers ---

var timeFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999",
	"2006-01-02 15:04:05",
}

func tryParseTime(s string) *time.Time {
	for _, f := range timeFormats {
		if t, err := time.Parse(f, s); err == nil {
			return &t
		}
	}
	return nil
}

var knownLevels = map[string]string{
	"trace":    "TRACE",
	"debug":    "DEBUG",
	"info":     "INFO",
	"notice":   "INFO",
	"warn":     "WARN",
	"warning":  "WARN",
	"error":    "ERROR",
	"err":      "ERROR",
	"critical": "ERROR",
	"fatal":    "FATAL",
	"panic":    "FATAL",
}

func normaliseLevel(s string) string {
	if v, ok := knownLevels[strings.ToLower(strings.TrimSpace(s))]; ok {
		return v
	}
	return ""
}
