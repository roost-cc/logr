package reconnect

import (
	"sync"
	"time"
)

// FakeClock is a Clock for tests. After(d) advances the fake time by d and
// fires at once, so retry loops run without sleeping; Waits records every
// requested duration.
type FakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

// NewFakeClock returns a FakeClock starting at now.
func NewFakeClock(now time.Time) *FakeClock { return &FakeClock{now: now} }

// Now returns the fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After advances the fake time by d and returns a channel that has fired.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.waits = append(c.waits, d)
	now := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

// Advance moves the fake time forward by d without recording a wait.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Waits returns a copy of every duration passed to After.
func (c *FakeClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}
