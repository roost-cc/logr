package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	dockerclient "github.com/moby/moby/client"

	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/reconnect"
)

// ---- fake Docker API ----

type fakeContainer struct {
	id      string
	labels  map[string]string
	running bool
	exists  bool
	lines   []string // written at the start of every log stream
	errs    []error  // returned by the next ContainerLogs calls, in order
	end     chan struct{}
}

type logCall struct {
	id    string
	since string
}

type fakeDocker struct {
	mu          sync.Mutex
	cs          map[string]*fakeContainer
	logCalls    []logCall
	inspects    int
	eventsCalls int
	evtCh       chan events.Message
	errCh       chan error
	// afterInspect, if set, runs after ContainerInspect has computed its
	// result and before it returns, outside f.mu.
	afterInspect func(id string)
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		cs:    map[string]*fakeContainer{},
		evtCh: make(chan events.Message),
		errCh: make(chan error, 1),
	}
}

func notFound(id string) error {
	return fmt.Errorf("Error response from daemon: No such container: %s: %w", id, cerrdefs.ErrNotFound)
}

func (f *fakeDocker) add(id, svc string, lines ...string) *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeContainer{
		id:      id,
		labels:  map[string]string{"com.docker.compose.service": svc},
		running: true,
		exists:  true,
		lines:   lines,
		end:     make(chan struct{}),
	}
	f.cs[id] = c
	return c
}

// stop ends the container's current log stream and marks it stopped.
func (f *fakeDocker) stop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cs[id]
	c.running = false
	close(c.end)
	c.end = make(chan struct{})
}

// remove ends the container's log stream and deletes it.
func (f *fakeDocker) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cs[id]
	c.running = false
	c.exists = false
	close(c.end)
	c.end = make(chan struct{})
}

func (f *fakeDocker) start(id string) {
	f.mu.Lock()
	f.cs[id].running = true
	f.mu.Unlock()
}

func (f *fakeDocker) calls() []logCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]logCall(nil), f.logCalls...)
}

func (f *fakeDocker) ContainerList(_ context.Context, _ dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []container.Summary
	for _, c := range f.cs {
		if c.exists && c.running {
			out = append(out, container.Summary{ID: c.id, Labels: c.labels})
		}
	}
	return dockerclient.ContainerListResult{Items: out}, nil
}

// writeStdFrame writes p as one frame of Docker's multiplexed log stream
// (see stdcopy.StdCopy): an 8-byte header {stream, 0, 0, 0, size (uint32 BE)}.
func writeStdFrame(buf *bytes.Buffer, stream stdcopy.StdType, p []byte) {
	n := len(p)
	buf.Write([]byte{byte(stream), 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	buf.Write(p)
}

func (f *fakeDocker) ContainerLogs(ctx context.Context, id string, opts dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logCalls = append(f.logCalls, logCall{id: id, since: opts.Since})
	c := f.cs[id]
	if c == nil || !c.exists {
		return nil, notFound(id)
	}
	if len(c.errs) > 0 {
		err := c.errs[0]
		c.errs = c.errs[1:]
		return nil, err
	}
	var buf bytes.Buffer
	for _, l := range c.lines {
		writeStdFrame(&buf, stdcopy.Stdout, []byte(l+"\n"))
	}
	if !c.running {
		// Docker returns the log and closes for a stopped container.
		return io.NopCloser(&buf), nil
	}
	pr, pw := io.Pipe()
	end := c.end
	go func() {
		pw.Write(buf.Bytes()) //nolint:errcheck
		select {
		case <-end:
		case <-ctx.Done():
		}
		pw.Close()
	}()
	return pr, nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string, _ dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error) {
	resp, err, hook := f.inspect(id)
	if hook != nil {
		hook(id)
	}
	return dockerclient.ContainerInspectResult{Container: resp}, err
}

func (f *fakeDocker) inspect(id string) (container.InspectResponse, error, func(string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspects++
	c := f.cs[id]
	if c == nil || !c.exists {
		return container.InspectResponse{}, notFound(id), f.afterInspect
	}
	return container.InspectResponse{
		ID:    id,
		State: &container.State{Running: c.running},
	}, nil, f.afterInspect
}

func (f *fakeDocker) Events(_ context.Context, _ dockerclient.EventsListOptions) dockerclient.EventsResult {
	f.mu.Lock()
	f.eventsCalls++
	f.mu.Unlock()
	return dockerclient.EventsResult{Messages: f.evtCh, Err: f.errCh}
}

func (f *fakeDocker) sendEvent(action events.Action, id, svc string) {
	f.evtCh <- events.Message{
		Type:   events.ContainerEventType,
		Action: action,
		Actor: events.Actor{ID: id, Attributes: map[string]string{
			"com.docker.compose.service": svc,
		}},
	}
}

// ---- harness ----

type harness struct {
	t      *testing.T
	fake   *fakeDocker
	d      *dockerConnector
	clock  *reconnect.FakeClock
	cancel context.CancelFunc
	done   chan error

	mu      sync.Mutex
	lines   []string
	records []db.Record
}

func newHarness(t *testing.T, fake *fakeDocker, services ...string) *harness {
	h := &harness{t: t, fake: fake, clock: reconnect.NewFakeClock(time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC))}
	h.d = newDockerConnector(fake, services, func(string) string { return "line" }, func(r db.Record) {
		h.mu.Lock()
		h.records = append(h.records, r)
		h.mu.Unlock()
	})
	h.d.clock = h.clock
	h.d.logf = func(format string, args ...any) {
		h.mu.Lock()
		h.lines = append(h.lines, fmt.Sprintf(format, args...))
		h.mu.Unlock()
	}
	return h
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.d.run(ctx) }()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		h.t.Error("connector did not stop after cancel")
	}
}

func (h *harness) logLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.lines...)
}

func (h *harness) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		out = append(out, r.Source+": "+r.Message)
	}
	return out
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s\nlog lines: %q\nrecords: %q\nlogs calls: %v",
				what, h.logLines(), h.messages(), h.fake.calls())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) hasLine(want string) func() bool {
	return func() bool {
		for _, l := range h.logLines() {
			if l == want {
				return true
			}
		}
		return false
	}
}

func (h *harness) wantLines(want ...string) {
	h.t.Helper()
	if got := h.logLines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		h.t.Fatalf("log lines:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// settle gives background goroutines a moment to do anything they shouldn't.
func settle() { time.Sleep(50 * time.Millisecond) }

const (
	idA = "aaaaaaaaaaaa0000"
	idB = "bbbbbbbbbbbb0000"
)

// ---- tests ----

func TestContainerGone(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{cerrdefs.ErrNotFound, true},
		{notFound("abc"), true},
		{fmt.Errorf("wrapped: %w", notFound("abc")), true},
		{errors.New("Error response from daemon: No such container: abc"), false}, // untyped
		{errors.New("dial unix /var/run/docker.sock: connect: connection refused"), false},
		{context.Canceled, false},
		{io.EOF, false},
	}
	for _, c := range cases {
		if got := containerGone(c.err); got != c.want {
			t.Errorf("containerGone(%v) = %v want %v", c.err, got, c.want)
		}
	}
}

func TestRemovedContainerDropped(t *testing.T) {
	fake := newFakeDocker()
	fake.add(idA, "synapse", "hello")
	h := newHarness(t, fake)
	h.start()
	h.eventually("first line", func() bool { return len(h.messages()) == 1 })

	fake.remove(idA)
	h.eventually("removed line", h.hasLine("docker: synapse aaaaaaaaaaaa removed"))
	settle()

	if n := len(fake.calls()); n != 1 {
		t.Fatalf("ContainerLogs called %d times, want 1 (no retries after removal)", n)
	}
	h.wantLines(
		"docker: synapse aaaaaaaaaaaa attached",
		"docker: synapse aaaaaaaaaaaa removed",
	)
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	if len(h.d.atts) != 0 {
		t.Fatalf("attachment not deleted: %v", h.d.atts)
	}
}

func TestLogsNotFoundDropsWithoutInspect(t *testing.T) {
	fake := newFakeDocker()
	h := newHarness(t, fake)
	h.start()

	fake.sendEvent(events.ActionStart, idA, "web") // never existed in the fake
	h.eventually("removed line", h.hasLine("docker: web aaaaaaaaaaaa removed"))
	settle()

	fake.mu.Lock()
	inspects := fake.inspects
	fake.mu.Unlock()
	if inspects != 0 {
		t.Fatalf("inspected %d times, want 0", inspects)
	}
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("ContainerLogs called %d times, want 1", n)
	}
}

func TestStoppedDetachesThenStartReattachesWithSince(t *testing.T) {
	fake := newFakeDocker()
	fake.add(idA, "synapse", "hello")
	h := newHarness(t, fake)
	h.start()
	h.eventually("first line", func() bool { return len(h.messages()) == 1 })

	fake.stop(idA)
	h.eventually("stopped line", h.hasLine("docker: synapse aaaaaaaaaaaa stopped"))
	settle()
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("stopped container polled: %d ContainerLogs calls", n)
	}

	fake.start(idA)
	fake.sendEvent(events.ActionStart, idA, "synapse")
	h.eventually("reattach", func() bool { return len(fake.calls()) == 2 })

	calls := fake.calls()
	if calls[0].since != "" {
		t.Errorf("first attach passed Since=%q, want full log", calls[0].since)
	}
	since, err := time.Parse(time.RFC3339Nano, calls[1].since)
	if err != nil {
		t.Fatalf("reattach Since=%q: %v", calls[1].since, err)
	}
	if !since.Equal(h.clock.Now()) {
		t.Errorf("Since=%v want stream end time %v", since, h.clock.Now())
	}
	h.eventually("second attached line", func() bool { return len(h.logLines()) == 3 })
	h.wantLines(
		"docker: synapse aaaaaaaaaaaa attached",
		"docker: synapse aaaaaaaaaaaa stopped",
		"docker: synapse aaaaaaaaaaaa attached",
	)
}

// A start event that lands after follow's inspect reported the container
// stopped, but before detach, must not leave the restarted container
// unfollowed.
func TestStartBetweenInspectAndDetachKeepsFollowing(t *testing.T) {
	fake := newFakeDocker()
	fake.add(idA, "synapse", "hello")
	h := newHarness(t, fake)
	var once sync.Once
	fake.afterInspect = func(id string) {
		once.Do(func() {
			fake.start(id)
			h.d.handleEvent(context.Background(), events.Message{
				Type:   events.ContainerEventType,
				Action: events.ActionStart,
				Actor: events.Actor{ID: id, Attributes: map[string]string{
					"com.docker.compose.service": "synapse",
				}},
			})
		})
	}
	h.start()
	h.eventually("first line", func() bool { return len(h.messages()) == 1 })

	fake.stop(idA)
	h.eventually("reattach", func() bool { return len(fake.calls()) == 2 })
	h.eventually("second stream", func() bool { return len(h.messages()) == 2 })
	settle()

	if since := fake.calls()[1].since; since == "" {
		t.Errorf("reattach passed no Since; history would be re-ingested")
	}
	h.wantLines(
		"docker: synapse aaaaaaaaaaaa attached",
		"docker: synapse aaaaaaaaaaaa stopped",
		"docker: synapse aaaaaaaaaaaa attached",
	)
	h.d.mu.Lock()
	streaming := h.d.atts[idA] != nil && h.d.atts[idA].streaming
	h.d.mu.Unlock()
	if !streaming {
		t.Fatal("restarted container left detached")
	}
}

func TestRecreatedServiceFollowed(t *testing.T) {
	fake := newFakeDocker()
	fake.add(idA, "synapse", "old")
	h := newHarness(t, fake, "synapse")
	h.start()
	h.eventually("old line", func() bool { return len(h.messages()) == 1 })

	// docker compose up -d --force-recreate synapse: stop, destroy, create, start.
	fake.stop(idA)
	h.eventually("stopped line", h.hasLine("docker: synapse aaaaaaaaaaaa stopped"))
	fake.remove(idA)
	fake.sendEvent(events.ActionDestroy, idA, "synapse")
	fake.add(idB, "synapse", "new")
	fake.sendEvent(events.ActionStart, idB, "synapse")

	h.eventually("new line", func() bool { return len(h.messages()) == 2 })
	settle()
	if got := h.messages(); got[1] != "synapse: new" {
		t.Fatalf("records %q, want the new container under source synapse", got)
	}
	for _, c := range fake.calls()[1:] {
		if c.id == idA {
			t.Fatalf("old container retried: %v", fake.calls())
		}
	}
	h.wantLines(
		"docker: synapse aaaaaaaaaaaa attached",
		"docker: synapse aaaaaaaaaaaa stopped",
		"docker: synapse aaaaaaaaaaaa removed",
		"docker: synapse bbbbbbbbbbbb attached",
	)
}

func TestRecreateBeforeEventsDropsOldID(t *testing.T) {
	fake := newFakeDocker()
	fake.add(idA, "synapse", "old")
	h := newHarness(t, fake)
	h.start()
	h.eventually("old line", func() bool { return len(h.messages()) == 1 })

	// The new container starts before the old one's destroy event arrives.
	fake.add(idB, "synapse", "new")
	fake.sendEvent(events.ActionStart, idB, "synapse")
	fake.remove(idA)
	h.eventually("old removed", h.hasLine("docker: synapse aaaaaaaaaaaa removed"))
	fake.sendEvent(events.ActionDestroy, idA, "synapse") // late; must stay quiet
	h.eventually("new line", func() bool { return len(h.messages()) == 2 })
	settle()

	removed := 0
	for _, l := range h.logLines() {
		if strings.HasSuffix(l, "removed") {
			removed++
		}
	}
	if removed != 1 {
		t.Fatalf("want one removed line, got %q", h.logLines())
	}
}

func TestDestroyWhileStreamingLogsOnce(t *testing.T) {
	fake := newFakeDocker()
	fake.add(idA, "synapse", "hello")
	h := newHarness(t, fake)
	h.start()
	h.eventually("first line", func() bool { return len(h.messages()) == 1 })

	fake.sendEvent(events.ActionDestroy, idA, "synapse")
	h.eventually("removed line", h.hasLine("docker: synapse aaaaaaaaaaaa removed"))
	fake.remove(idA)
	settle()
	h.wantLines(
		"docker: synapse aaaaaaaaaaaa attached",
		"docker: synapse aaaaaaaaaaaa removed",
	)
}

func TestDestroyAfterStopLogsRemoved(t *testing.T) {
	fake := newFakeDocker()
	fake.add(idA, "synapse", "hello")
	h := newHarness(t, fake)
	h.start()
	h.eventually("first line", func() bool { return len(h.messages()) == 1 })

	fake.stop(idA)
	h.eventually("stopped line", h.hasLine("docker: synapse aaaaaaaaaaaa stopped"))
	fake.remove(idA)
	fake.sendEvent(events.ActionDestroy, idA, "synapse")
	h.eventually("removed line", h.hasLine("docker: synapse aaaaaaaaaaaa removed"))
	fake.sendEvent(events.ActionDestroy, idA, "synapse") // duplicate; must stay quiet
	settle()

	h.wantLines(
		"docker: synapse aaaaaaaaaaaa attached",
		"docker: synapse aaaaaaaaaaaa stopped",
		"docker: synapse aaaaaaaaaaaa removed",
	)
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	if len(h.d.atts) != 0 {
		t.Fatalf("attachment not deleted: %v", h.d.atts)
	}
}

func TestDisallowedServiceIgnored(t *testing.T) {
	fake := newFakeDocker()
	fake.add(idA, "web", "hello")
	h := newHarness(t, fake, "synapse")
	h.start()

	fake.sendEvent(events.ActionStart, idA, "web")
	settle()
	if n := len(fake.calls()); n != 0 {
		t.Fatalf("disallowed service attached: %v", fake.calls())
	}
	h.wantLines()
}

func TestTransientErrorBacksOffAndLogsOnce(t *testing.T) {
	fake := newFakeDocker()
	c := fake.add(idA, "synapse", "hello")
	boom := errors.New("connection reset by peer")
	c.errs = []error{boom, boom, boom}
	h := newHarness(t, fake)
	h.start()

	h.eventually("stream", func() bool { return len(h.messages()) == 1 })
	h.wantLines(
		"docker: synapse aaaaaaaaaaaa attached",
		"docker: synapse aaaaaaaaaaaa lost: connection reset by peer",
		"docker: synapse aaaaaaaaaaaa reconnected after 3 attempts",
	)
	want := []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second}
	if got := h.clock.Waits(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("waits %v want %v", got, want)
	}
	// ContainerLogs never succeeded before, so there is nothing to skip.
	for _, c := range fake.calls() {
		if c.since != "" {
			t.Fatalf("Since set before any stream was read: %v", fake.calls())
		}
	}
}

func TestEventsStreamReconnects(t *testing.T) {
	fake := newFakeDocker()
	h := newHarness(t, fake)
	h.start()
	// Let the initial (empty) container list happen first.
	h.eventually("subscribe", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.eventsCalls == 1
	})
	settle()

	// A container starts while the events stream is down: the re-list after
	// resubscribing picks it up.
	fake.add(idA, "synapse", "hello")
	fake.errCh <- errors.New("unexpected EOF")
	h.eventually("reattach via list", func() bool { return len(h.messages()) == 1 })
	h.eventually("reconnected", h.hasLine("docker: events reconnected after 1 attempts"))
	h.wantLines(
		"docker: events lost: unexpected EOF",
		"docker: synapse aaaaaaaaaaaa attached",
		"docker: events reconnected after 1 attempts",
	)
	fake.mu.Lock()
	calls := fake.eventsCalls
	fake.mu.Unlock()
	if calls != 2 {
		t.Fatalf("Events called %d times, want 2", calls)
	}
}
