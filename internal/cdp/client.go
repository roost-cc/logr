// Package cdp connects to Chrome DevTools Protocol debug endpoints and streams
// log events as db.Records. It works with both Chrome (port 9222) and Node.js
// V8 Inspector (ports 9229/9230/9231) — they speak the same protocol.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/reconnect"
)

// Target is a debuggable entity returned by /json.
type Target struct {
	ID                   string `json:"id"`
	Title                string `json:"title"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	ParentID             string `json:"parentId"`
}

// ListTargets fetches the target list from a CDP debug endpoint.
// endpoint is e.g. "http://localhost:9222" or "http://localhost:9229".
func ListTargets(ctx context.Context, endpoint string) ([]Target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var targets []Target
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("parse /json: %w", err)
	}
	return targets, nil
}

// cdpMsg is a generic CDP message (both commands and events share this shape).
type cdpMsg struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ConnectAndStream connects to a single CDP target, enables log/runtime domains,
// and calls emit for every log event until ctx is cancelled or the connection drops.
func ConnectAndStream(ctx context.Context, target Target, source string, emit func(db.Record)) error {
	return connectAndStream(ctx, target, source, emit, nil)
}

// connectAndStream is ConnectAndStream with onOpen, if non-nil, called once
// the domains are enabled.
func connectAndStream(ctx context.Context, target Target, source string, emit func(db.Record), onOpen func()) error {
	wsURL := target.WebSocketDebuggerURL
	if wsURL == "" {
		return fmt.Errorf("target %s has no webSocketDebuggerUrl", target.ID)
	}

	// Chrome validates the Host header — set it to the actual host.
	u, err := url.Parse(wsURL)
	if err != nil {
		return fmt.Errorf("parse ws url: %w", err)
	}
	hdr := http.Header{"Host": {u.Host}}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer conn.Close()

	var seq atomic.Int64
	send := func(method string, params any) error {
		id := seq.Add(1)
		body := map[string]any{"id": id, "method": method}
		if params != nil {
			body["params"] = params
		}
		b, _ := json.Marshal(body)
		return conn.WriteMessage(websocket.TextMessage, b)
	}

	// Enable domains
	for _, method := range []string{"Log.enable", "Runtime.enable"} {
		if err := send(method, nil); err != nil {
			return fmt.Errorf("enable %s: %w", method, err)
		}
	}
	if onOpen != nil {
		onOpen()
	}

	// Read loop
	done := make(chan struct{})
	var readErr error
	go func() {
		defer close(done)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				readErr = err
				return
			}
			var m cdpMsg
			if err := json.Unmarshal(msg, &m); err != nil {
				continue
			}
			if m.Method == "" {
				continue // command response, not an event
			}
			if r, ok := handleEvent(m, source, target); ok {
				emit(r)
			}
		}
	}()

	select {
	case <-ctx.Done():
		conn.WriteMessage(websocket.CloseMessage, //nolint:errcheck
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		return nil
	case <-done:
		return readErr
	}
}

// handleEvent converts a CDP event into a db.Record.
func handleEvent(m cdpMsg, source string, target Target) (db.Record, bool) {
	switch m.Method {
	case "Log.entryAdded":
		return handleLogEntry(m.Params, source, target)
	case "Runtime.consoleAPICalled":
		return handleConsoleAPI(m.Params, source, target)
	case "Runtime.exceptionThrown":
		return handleException(m.Params, source, target)
	}
	return db.Record{}, false
}

// --- Log.entryAdded ---

type logEntry struct {
	Entry struct {
		Level      string  `json:"level"`
		Text       string  `json:"text"`
		Source     string  `json:"source"`
		URL        string  `json:"url"`
		LineNumber int     `json:"lineNumber"`
		Timestamp  float64 `json:"timestamp"` // ms since epoch
	} `json:"entry"`
}

func handleLogEntry(raw json.RawMessage, source string, target Target) (db.Record, bool) {
	var p logEntry
	if err := json.Unmarshal(raw, &p); err != nil {
		return db.Record{}, false
	}
	e := p.Entry
	if e.Text == "" {
		return db.Record{}, false
	}

	data := map[string]any{
		"cdp_event":  "Log.entryAdded",
		"log_source": e.Source,
		"target_url": target.URL,
		"_raw":       e.Text,
	}
	if e.URL != "" {
		data["url"] = e.URL
		data["line"] = e.LineNumber
	}

	var logTime *time.Time
	if e.Timestamp > 0 {
		t := time.UnixMilli(int64(e.Timestamp))
		logTime = &t
	}

	return db.Record{
		LogTime: logTime,
		Source:  source,
		Level:   cdpLevelToLevel(e.Level),
		Message: e.Text,
		Data:    data,
	}, true
}

// --- Runtime.consoleAPICalled ---

type consoleAPI struct {
	Type      string  `json:"type"` // log, warn, error, info, debug, etc.
	Timestamp float64 `json:"timestamp"`
	Args      []struct {
		Type  string `json:"type"`
		Value any    `json:"value"`
		// Objects have Description instead of Value
		Description string `json:"description"`
	} `json:"args"`
	StackTrace *struct {
		CallFrames []struct {
			FunctionName string `json:"functionName"`
			URL          string `json:"url"`
			LineNumber   int    `json:"lineNumber"`
			ColumnNumber int    `json:"columnNumber"`
		} `json:"callFrames"`
	} `json:"stackTrace"`
}

func handleConsoleAPI(raw json.RawMessage, source string, target Target) (db.Record, bool) {
	var p consoleAPI
	if err := json.Unmarshal(raw, &p); err != nil {
		return db.Record{}, false
	}

	// Render args to a message string
	var parts []string
	for _, arg := range p.Args {
		switch arg.Type {
		case "string":
			if s, ok := arg.Value.(string); ok {
				parts = append(parts, s)
			}
		case "number":
			parts = append(parts, fmt.Sprint(arg.Value))
		case "boolean":
			parts = append(parts, fmt.Sprint(arg.Value))
		case "object", "function":
			if arg.Description != "" {
				parts = append(parts, arg.Description)
			}
		case "undefined":
			parts = append(parts, "undefined")
		default:
			if arg.Value != nil {
				parts = append(parts, fmt.Sprint(arg.Value))
			}
		}
	}
	msg := strings.Join(parts, " ")
	if msg == "" {
		return db.Record{}, false
	}

	data := map[string]any{
		"cdp_event":  "Runtime.consoleAPICalled",
		"console_fn": p.Type,
		"target_url": target.URL,
		"_raw":       msg,
	}

	if p.StackTrace != nil && len(p.StackTrace.CallFrames) > 0 {
		f := p.StackTrace.CallFrames[0]
		data["url"] = f.URL
		data["line"] = f.LineNumber
		data["column"] = f.ColumnNumber
		if f.FunctionName != "" {
			data["function"] = f.FunctionName
		}
	}

	var logTime *time.Time
	if p.Timestamp > 0 {
		t := time.UnixMilli(int64(p.Timestamp))
		logTime = &t
	}

	return db.Record{
		LogTime: logTime,
		Source:  source,
		Level:   cdpLevelToLevel(p.Type),
		Message: msg,
		Data:    data,
	}, true
}

// --- Runtime.exceptionThrown ---

type exceptionEvent struct {
	Timestamp        float64 `json:"timestamp"`
	ExceptionDetails struct {
		Text         string `json:"text"`
		LineNumber   int    `json:"lineNumber"`
		ColumnNumber int    `json:"columnNumber"`
		URL          string `json:"url"`
		Exception    *struct {
			Description string `json:"description"`
		} `json:"exception"`
		StackTrace *struct {
			Description string `json:"description"`
			CallFrames  []struct {
				FunctionName string `json:"functionName"`
				URL          string `json:"url"`
				LineNumber   int    `json:"lineNumber"`
			} `json:"callFrames"`
		} `json:"stackTrace"`
	} `json:"exceptionDetails"`
}

func handleException(raw json.RawMessage, source string, target Target) (db.Record, bool) {
	var p exceptionEvent
	if err := json.Unmarshal(raw, &p); err != nil {
		return db.Record{}, false
	}

	e := p.ExceptionDetails
	msg := e.Text
	if e.Exception != nil && e.Exception.Description != "" {
		msg = e.Exception.Description
	}
	if msg == "" {
		msg = "Uncaught exception"
	}

	data := map[string]any{
		"cdp_event":  "Runtime.exceptionThrown",
		"target_url": target.URL,
		"_raw":       msg,
	}
	if e.URL != "" {
		data["url"] = e.URL
		data["line"] = e.LineNumber
		data["column"] = e.ColumnNumber
	}
	if e.StackTrace != nil && e.StackTrace.Description != "" {
		data["stack"] = e.StackTrace.Description
	}

	var logTime *time.Time
	if p.Timestamp > 0 {
		t := time.UnixMilli(int64(p.Timestamp))
		logTime = &t
	}

	return db.Record{
		LogTime: logTime,
		Source:  source,
		Level:   "ERROR",
		Message: msg,
		Data:    data,
	}, true
}

// cdpLevelToLevel normalises CDP log levels to logr levels.
func cdpLevelToLevel(level string) string {
	switch strings.ToLower(level) {
	case "verbose", "debug":
		return "DEBUG"
	case "info", "log", "dir", "dirxml", "table", "group", "groupcollapsed", "count", "timeend":
		return "INFO"
	case "warning", "warn":
		return "WARN"
	case "error", "assert":
		return "ERROR"
	default:
		return "INFO"
	}
}

// SourceName derives a short source label from a CDP target.
func SourceName(endpoint string, t Target) string {
	// Node.js targets: use the script filename
	if t.Type == "node" {
		parts := strings.Split(t.URL, "/")
		if len(parts) > 0 {
			name := parts[len(parts)-1]
			if name != "" {
				return "node:" + name
			}
		}
		return "node"
	}
	// Browser page targets: use URL hostname + path prefix
	if u, err := url.Parse(t.URL); err == nil && u.Host != "" {
		host := strings.TrimPrefix(u.Hostname(), "www.")
		host = strings.Split(host, ".")[0] // "app" from "app.localhost.example.test"
		return "browser:" + host
	}
	if t.Title != "" {
		title := strings.TrimSpace(t.Title)
		if len(title) > 20 {
			title = strings.TrimSpace(title[:20])
		}
		return "browser:" + strings.ReplaceAll(title, " ", "-")
	}
	return "browser"
}

// discoverEvery is the target discovery interval while an endpoint is up.
const discoverEvery = 5 * time.Second

// watcher follows one CDP endpoint. Its clock, logger and target lister are
// fields so tests can replace them.
type watcher struct {
	endpoint    string
	targetTypes []string
	emit        func(db.Record)
	clock       reconnect.Clock
	logf        func(format string, args ...any)
	listTargets func(ctx context.Context, endpoint string) ([]Target, error)

	seen sync.Map // target ID → true while a followTarget goroutine owns it
	wg   sync.WaitGroup
}

func newWatcher(endpoint string, emit func(db.Record), targetTypes []string) *watcher {
	return &watcher{
		endpoint:    endpoint,
		targetTypes: targetTypes,
		emit:        emit,
		clock:       reconnect.RealClock,
		logf:        log.Printf,
		listTargets: ListTargets,
	}
}

// WatchEndpoint discovers targets at endpoint, connects to each eligible one,
// and re-discovers every 5s to pick up new tabs/workers. It blocks until ctx
// is cancelled. emit is called for every log record.
//
// Only state changes are logged: an endpoint or target that goes down logs one
// "lost" line, one "reconnected" line on recovery, and at most one "still
// down" line per 5 minutes in between, while retries back off from 3s to 60s.
// A target that disappears from /json is dropped with one "closed" line.
func WatchEndpoint(ctx context.Context, endpoint string, emit func(db.Record), targetTypes []string) {
	newWatcher(endpoint, emit, targetTypes).run(ctx)
}

func (w *watcher) run(ctx context.Context) {
	defer w.wg.Wait()
	st := reconnect.NewWith("cdp: endpoint "+w.endpoint, w.logf, w.clock)
	for {
		if err := w.discover(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			st.Failed(err)
			if !st.Wait(ctx) {
				return
			}
			continue
		}
		st.Succeeded()
		select {
		case <-ctx.Done():
			return
		case <-w.clock.After(discoverEvery):
		}
	}
}

// discover lists the endpoint's targets and follows any eligible new ones.
func (w *watcher) discover(ctx context.Context) error {
	targets, err := w.listTargets(ctx, w.endpoint)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if !typeAllowed(t.Type, w.targetTypes) {
			continue
		}
		// Skip internal browser chrome and iframes
		if t.ParentID != "" && t.Type == "iframe" {
			continue
		}
		if strings.HasPrefix(t.URL, "chrome://") || strings.HasPrefix(t.URL, "chrome-untrusted://") || strings.HasPrefix(t.URL, "devtools://") {
			continue
		}
		if _, loaded := w.seen.LoadOrStore(t.ID, true); loaded {
			continue
		}
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			defer w.seen.Delete(t.ID) // lets discover reconnect it, or its replacement
			w.followTarget(ctx, t)
		}()
	}
	return nil
}

// followTarget streams one target, redialling after drops while the endpoint
// still lists it. It returns when ctx ends, the target is no longer listed,
// or the endpoint stays unreachable past the backoff cap.
func (w *watcher) followTarget(ctx context.Context, t Target) {
	src := SourceName(w.endpoint, t)
	w.logf("cdp: connecting to %s (%s)", src, t.URL)
	st := reconnect.NewWith("cdp: "+src, w.logf, w.clock)
	for {
		err := connectAndStream(ctx, t, src, w.emit, st.Succeeded)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("connection closed")
		}

		targets, listErr := w.listTargets(ctx, w.endpoint)
		if ctx.Err() != nil {
			return
		}
		if listErr != nil {
			if st.AtCap() {
				w.logf("cdp: %s dropped: endpoint not answering: %v", src, listErr)
				return
			}
			st.Failed(listErr)
		} else {
			fresh, ok := findTarget(targets, t.ID)
			if !ok {
				w.logf("cdp: %s closed", src)
				return
			}
			t = fresh // the WebSocket URL may have changed
			st.Failed(err)
		}
		if !st.Wait(ctx) {
			return
		}
	}
}

func findTarget(targets []Target, id string) (Target, bool) {
	for _, t := range targets {
		if t.ID == id {
			return t, true
		}
	}
	return Target{}, false
}

func typeAllowed(typ string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == typ {
			return true
		}
	}
	return false
}
