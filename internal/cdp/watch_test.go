package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/reconnect"
)

// testClock fires backoff waits at once (via FakeClock) but holds discovery
// ticks until the test sends one, so the watcher never spins while up.
type testClock struct {
	*reconnect.FakeClock
	ticks chan time.Time
}

func (c *testClock) After(d time.Duration) <-chan time.Time {
	if d == discoverEvery {
		return c.ticks
	}
	return c.FakeClock.After(d)
}

// fakeEndpoint is a DevTools endpoint serving /json and a websocket per target.
// onDial runs for each websocket connection before the server closes it.
type fakeEndpoint struct {
	srv *httptest.Server

	mu      sync.Mutex
	targets []Target
	down    bool
	dials   int
	onDial  func(n int)
}

func newFakeEndpoint(t *testing.T) *fakeEndpoint {
	e := &fakeEndpoint{}
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(e.targets) //nolint:errcheck
	})
	mux.HandleFunc("/devtools/page/", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		down := e.down
		e.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Read Log.enable and Runtime.enable so the client counts as connected.
		for i := 0; i < 2; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		e.mu.Lock()
		e.dials++
		n := e.dials
		hook := e.onDial
		e.mu.Unlock()
		if hook != nil {
			hook(n)
		}
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *fakeEndpoint) target(id, url string) Target {
	return Target{
		ID:                   id,
		Type:                 "page",
		URL:                  url,
		WebSocketDebuggerURL: "ws://" + strings.TrimPrefix(e.srv.URL, "http://") + "/devtools/page/" + id,
	}
}

func (e *fakeEndpoint) set(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn()
}

func (e *fakeEndpoint) dialCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dials
}

type watchHarness struct {
	t      *testing.T
	w      *watcher
	clock  *testClock
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	lines []string
}

func newWatchHarness(t *testing.T, endpoint string) *watchHarness {
	h := &watchHarness{
		t: t,
		clock: &testClock{
			FakeClock: reconnect.NewFakeClock(time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)),
			ticks:     make(chan time.Time),
		},
	}
	h.w = newWatcher(endpoint, func(db.Record) {}, nil)
	h.w.clock = h.clock
	h.w.logf = func(format string, args ...any) {
		h.mu.Lock()
		h.lines = append(h.lines, fmt.Sprintf(format, args...))
		h.mu.Unlock()
	}
	return h
}

func (h *watchHarness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		h.w.run(ctx)
		close(h.done)
	}()
	h.t.Cleanup(h.stop)
}

func (h *watchHarness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		h.t.Error("watcher did not stop after cancel")
	}
}

// tick lets the discovery loop run once more (blocks until it is waiting).
func (h *watchHarness) tick() {
	select {
	case h.clock.ticks <- time.Time{}:
	case <-time.After(3 * time.Second):
		h.t.Fatal("discovery loop not waiting for a tick")
	}
}

func (h *watchHarness) logLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.lines...)
}

func (h *watchHarness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; lines: %q", what, h.logLines())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *watchHarness) hasLine(want string) func() bool {
	return func() bool {
		for _, l := range h.logLines() {
			if l == want {
				return true
			}
		}
		return false
	}
}

func (h *watchHarness) wantLines(want ...string) {
	h.t.Helper()
	if got := h.logLines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		h.t.Fatalf("log lines:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestTargetAbsentFromJSONIsDroppedNotRedialled(t *testing.T) {
	e := newFakeEndpoint(t)
	e.set(func() {
		e.targets = []Target{e.target("T1", "http://app.localhost/")}
		// The tab closes: it leaves /json and its socket drops.
		e.onDial = func(int) { e.set(func() { e.targets = nil }) }
	})
	h := newWatchHarness(t, e.srv.URL)
	h.start()

	h.eventually("closed line", h.hasLine("cdp: browser:app closed"))
	for i := 0; i < 5; i++ {
		h.tick()
	}
	if n := e.dialCount(); n != 1 {
		t.Fatalf("closed target dialled %d times, want 1", n)
	}
	h.wantLines(
		"cdp: connecting to browser:app (http://app.localhost/)",
		"cdp: browser:app closed",
	)
}

func TestTargetStillListedIsRedialledThenClosed(t *testing.T) {
	e := newFakeEndpoint(t)
	e.set(func() {
		e.targets = []Target{e.target("T1", "http://app.localhost/")}
		e.onDial = func(n int) {
			if n == 2 {
				e.set(func() { e.targets = nil })
			}
		}
	})
	h := newWatchHarness(t, e.srv.URL)
	h.start()

	h.eventually("closed line", h.hasLine("cdp: browser:app closed"))
	if n := e.dialCount(); n != 2 {
		t.Fatalf("dials %d want 2", n)
	}
	lines := h.logLines()
	if len(lines) != 4 ||
		lines[0] != "cdp: connecting to browser:app (http://app.localhost/)" ||
		!strings.HasPrefix(lines[1], "cdp: browser:app lost: ") ||
		lines[2] != "cdp: browser:app reconnected after 1 attempts" ||
		lines[3] != "cdp: browser:app closed" {
		t.Fatalf("lines %q", lines)
	}
}

func TestReplacementTargetPickedUpAfterClose(t *testing.T) {
	e := newFakeEndpoint(t)
	e.set(func() {
		e.targets = []Target{e.target("T1", "http://app.localhost/")}
		e.onDial = func(n int) {
			if n == 1 { // node restart: new target ID
				e.set(func() { e.targets = []Target{e.target("T2", "http://app.localhost/")} })
			}
		}
	})
	h := newWatchHarness(t, e.srv.URL)
	h.start()
	h.eventually("closed line", h.hasLine("cdp: browser:app closed"))
	h.tick()
	h.eventually("second dial", func() bool { return e.dialCount() >= 2 })
}

func TestTargetDroppedWhenEndpointStopsAnswering(t *testing.T) {
	e := newFakeEndpoint(t)
	e.set(func() {
		e.targets = []Target{e.target("T1", "http://app.localhost/")}
		e.onDial = func(int) { e.set(func() { e.down = true }) }
	})
	h := newWatchHarness(t, e.srv.URL)
	h.start()

	h.eventually("dropped line", func() bool {
		for _, l := range h.logLines() {
			if strings.HasPrefix(l, "cdp: browser:app dropped: endpoint not answering") {
				return true
			}
		}
		return false
	})
	if n := e.dialCount(); n != 1 {
		t.Fatalf("dials %d want 1 (no redial while /json fails)", n)
	}
	var target []string
	for _, l := range h.logLines() {
		if strings.HasPrefix(l, "cdp: browser:app") {
			target = append(target, l)
		}
	}
	// lost once, then dropped once the backoff hit the cap.
	if len(target) != 2 || !strings.HasPrefix(target[0], "cdp: browser:app lost: ") {
		t.Fatalf("target lines %q", target)
	}
	// seen is cleared so the next successful discover reconnects it.
	e.set(func() { e.down = false })
	h.tick()
	h.eventually("reconnect after drop", func() bool { return e.dialCount() >= 2 })
}

func TestEndpointDownIsQuiet(t *testing.T) {
	h := newWatchHarness(t, "http://localhost:9230")
	var mu sync.Mutex
	calls := 0
	up := make(chan struct{})
	h.w.listTargets = func(ctx context.Context, _ string) ([]Target, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 12 {
			return nil, errors.New("connection refused")
		}
		if n == 13 {
			close(up)
		}
		return nil, nil
	}
	h.start()
	select {
	case <-up:
	case <-time.After(3 * time.Second):
		t.Fatal("endpoint never retried 13 times")
	}
	h.eventually("reconnected line", h.hasLine("cdp: endpoint http://localhost:9230 reconnected after 12 attempts"))

	// Failures at t=0,3,9,21,45,93,153,213,273,333,393,453s: one summary at 333s.
	h.wantLines(
		"cdp: endpoint http://localhost:9230 lost: connection refused",
		"cdp: endpoint http://localhost:9230 still down (10 attempts since 10:00:00): connection refused",
		"cdp: endpoint http://localhost:9230 reconnected after 12 attempts",
	)
	want := "[3s 6s 12s 24s 48s 1m0s 1m0s 1m0s 1m0s 1m0s 1m0s 1m0s]"
	if got := fmt.Sprint(h.clock.Waits()); got != want {
		t.Fatalf("waits %s want %s", got, want)
	}
	h.tick() // back to the 5s discovery tick while up
}
