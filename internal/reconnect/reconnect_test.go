package reconnect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type capture struct{ lines []string }

func (c *capture) logf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func newTestState() (*State, *capture, *FakeClock) {
	c := &capture{}
	clk := NewFakeClock(time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC))
	return NewWith("docker: synapse abc", c.logf, clk), c, clk
}

func TestBackoffSequence(t *testing.T) {
	s, _, _ := newTestState()
	want := []time.Duration{3, 6, 12, 24, 48, 60, 60, 60}
	for i, w := range want {
		s.Failed(errors.New("boom"))
		if got := s.Delay(); got != w*time.Second {
			t.Fatalf("after failure %d: delay %v want %v", i+1, got, w*time.Second)
		}
	}
	if !s.AtCap() {
		t.Fatal("AtCap false at 60s")
	}
}

func TestAtCapOnlyAtMax(t *testing.T) {
	s, _, _ := newTestState()
	for i := 0; i < 5; i++ {
		s.Failed(errors.New("boom"))
		if s.AtCap() {
			t.Fatalf("AtCap true after %d failures (delay %v)", i+1, s.Delay())
		}
	}
	s.Failed(errors.New("boom"))
	if !s.AtCap() {
		t.Fatal("AtCap false after 6 failures")
	}
}

func TestResetOnSuccess(t *testing.T) {
	s, c, _ := newTestState()
	for i := 0; i < 4; i++ {
		s.Failed(errors.New("boom"))
	}
	s.Succeeded()
	if s.Down() || s.Attempts() != 0 || s.Delay() != InitialDelay {
		t.Fatalf("after success: down=%v attempts=%d delay=%v", s.Down(), s.Attempts(), s.Delay())
	}
	want := []string{
		"docker: synapse abc lost: boom",
		"docker: synapse abc reconnected after 4 attempts",
	}
	if strings.Join(c.lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(c.lines, "\n"), strings.Join(want, "\n"))
	}
	// A new outage starts over at 3s with a fresh "lost" line.
	s.Failed(errors.New("again"))
	if s.Delay() != 3*time.Second || len(c.lines) != 3 || c.lines[2] != "docker: synapse abc lost: again" {
		t.Fatalf("second outage: delay=%v lines=%q", s.Delay(), c.lines)
	}
}

func TestSucceededWhileUpIsQuiet(t *testing.T) {
	s, c, _ := newTestState()
	s.Succeeded()
	s.Succeeded()
	if len(c.lines) != 0 {
		t.Fatalf("unexpected lines %q", c.lines)
	}
}

func TestSummaryEveryFiveMinutes(t *testing.T) {
	s, c, clk := newTestState()
	s.Failed(errors.New("boom")) // lost at 10:00:00
	for i := 0; i < 10; i++ {
		clk.Advance(20 * time.Second) // up to 10:03:20
		s.Failed(errors.New("boom"))
	}
	if len(c.lines) != 1 {
		t.Fatalf("summary before 5 minutes: %q", c.lines)
	}
	clk.Advance(100 * time.Second) // 10:05:00
	s.Failed(errors.New("boom"))
	if len(c.lines) != 2 {
		t.Fatalf("want summary at 5 minutes, got %q", c.lines)
	}
	if want := "docker: synapse abc still down (12 attempts since 10:00:00): boom"; c.lines[1] != want {
		t.Fatalf("summary %q want %q", c.lines[1], want)
	}
	clk.Advance(time.Minute)
	s.Failed(errors.New("boom"))
	if len(c.lines) != 2 {
		t.Fatalf("second summary too soon: %q", c.lines)
	}
	clk.Advance(4 * time.Minute)
	s.Failed(errors.New("boom"))
	if len(c.lines) != 3 {
		t.Fatalf("want second summary, got %q", c.lines)
	}
}

func TestWaitUsesDelay(t *testing.T) {
	s, _, clk := newTestState()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.Failed(errors.New("boom"))
		if !s.Wait(ctx) {
			t.Fatal("Wait returned false")
		}
	}
	got := clk.Waits()
	want := []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("waits %v want %v", got, want)
	}
}

type neverClock struct{}

func (neverClock) Now() time.Time                       { return time.Time{} }
func (neverClock) After(time.Duration) <-chan time.Time { return nil }

func TestWaitReturnsOnCancel(t *testing.T) {
	s := NewWith("x", func(string, ...any) {}, neverClock{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- s.Wait(ctx) }()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("Wait returned true after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}
