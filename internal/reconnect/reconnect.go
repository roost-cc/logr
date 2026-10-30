// Package reconnect tracks the up/down state of one retried connection (a
// container log stream, a DevTools target or endpoint, the Docker events
// stream) so callers log state changes instead of every attempt, and back off
// exponentially while it is down.
//
// Lines logged by a State:
//
//	<name> lost: <err>                                   first failure
//	<name> still down (N attempts since HH:MM:SS): <err> at most once per 5 minutes
//	<name> reconnected after N attempts                  first success after a failure
package reconnect

import (
	"context"
	"log"
	"time"
)

const (
	// InitialDelay is the wait after the first failure.
	InitialDelay = 3 * time.Second
	// MaxDelay caps the exponential backoff.
	MaxDelay = 60 * time.Second
	// SummaryEvery is the minimum gap between "still down" lines.
	SummaryEvery = 5 * time.Minute
)

// Clock abstracts time so tests can drive backoff without sleeping.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// State is the reconnect state of one connection. It is not safe for
// concurrent use; each retry loop owns its State.
type State struct {
	name  string
	logf  func(format string, args ...any)
	clock Clock

	down        bool
	attempts    int
	downSince   time.Time
	lastSummary time.Time
	delay       time.Duration
}

// New returns a State that logs with log.Printf and uses the wall clock.
func New(name string) *State {
	return NewWith(name, nil, nil)
}

// NewWith returns a State with an injected logger and clock; nil means the
// default (log.Printf, RealClock).
func NewWith(name string, logf func(format string, args ...any), clock Clock) *State {
	if logf == nil {
		logf = log.Printf
	}
	if clock == nil {
		clock = RealClock
	}
	return &State{name: name, logf: logf, clock: clock}
}

// Failed records a failed attempt and advances the backoff. The first failure
// logs a "lost" line; later ones log a "still down" summary at most once per
// SummaryEvery.
func (s *State) Failed(err error) {
	now := s.clock.Now()
	s.attempts++
	if !s.down {
		s.down = true
		s.downSince = now
		s.lastSummary = now
		s.delay = InitialDelay
		s.logf("%s lost: %v", s.name, err)
		return
	}
	s.delay *= 2
	if s.delay > MaxDelay {
		s.delay = MaxDelay
	}
	if now.Sub(s.lastSummary) >= SummaryEvery {
		s.lastSummary = now
		s.logf("%s still down (%d attempts since %s): %v",
			s.name, s.attempts, s.downSince.Format("15:04:05"), err)
	}
}

// Succeeded records a successful attempt. If the connection was down it logs
// one "reconnected" line. The backoff resets either way.
func (s *State) Succeeded() {
	if s.down {
		s.logf("%s reconnected after %d attempts", s.name, s.attempts)
	}
	s.Reset()
}

// Reset clears the state without logging, e.g. when the connection ends for a
// reason that is not a failure (a container that stopped).
func (s *State) Reset() {
	s.down = false
	s.attempts = 0
	s.delay = 0
	s.downSince = time.Time{}
	s.lastSummary = time.Time{}
}

// Down reports whether the last attempt failed.
func (s *State) Down() bool { return s.down }

// Attempts is the number of failures since the last success.
func (s *State) Attempts() int { return s.attempts }

// Delay is the wait before the next retry.
func (s *State) Delay() time.Duration {
	if s.delay == 0 {
		return InitialDelay
	}
	return s.delay
}

// AtCap reports whether the backoff has reached MaxDelay.
func (s *State) AtCap() bool { return s.delay >= MaxDelay }

// Wait sleeps for the current delay. It returns false if ctx ended first.
func (s *State) Wait(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.clock.After(s.Delay()):
		return ctx.Err() == nil
	}
}
