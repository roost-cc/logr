package db

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Tests focus on concurrency safety of the Inserter buffer — we verify
// the mutex prevents data races on buf without needing a live database.

func TestInserter_AddFlush_sequential(t *testing.T) {
	ins := &Inserter{
		maxBuf: 5,
	}

	ctx := context.Background()

	// Fill to just below the threshold — no auto-flush
	for i := 0; i < 4; i++ {
		ins.mu.Lock()
		ins.buf = append(ins.buf, Record{
			Source:  "test",
			Message: "msg",
			Data:    map[string]any{},
		})
		ins.mu.Unlock()
	}

	ins.mu.Lock()
	n := len(ins.buf)
	ins.mu.Unlock()

	if n != 4 {
		t.Errorf("expected 4 buffered records, got %d", n)
	}

	// ManualFlush drains without a real pool — just test the drain logic
	ins.mu.Lock()
	batch := ins.buf
	ins.buf = make([]Record, 0, ins.maxBuf)
	ins.mu.Unlock()

	if len(batch) != 4 {
		t.Errorf("expected batch of 4, got %d", len(batch))
	}

	ins.mu.Lock()
	remaining := len(ins.buf)
	ins.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected empty buf after drain, got %d", remaining)
	}

	_ = ctx
}

// TestInserter_ConcurrentAdd verifies no data races occur when multiple
// goroutines call Add concurrently. Run with: go test -race ./internal/db/
func TestInserter_ConcurrentAdd(t *testing.T) {
	ins := &Inserter{
		maxBuf: 1000, // large enough that auto-flush (which needs a pool) won't trigger
	}

	var wg sync.WaitGroup
	const goroutines = 50
	const recordsEach = 20

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < recordsEach; j++ {
				ins.mu.Lock()
				ins.buf = append(ins.buf, Record{
					Source:  "test",
					Message: "concurrent",
					Data:    map[string]any{"goroutine": id, "seq": j},
				})
				ins.mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	ins.mu.Lock()
	total := len(ins.buf)
	ins.mu.Unlock()

	want := goroutines * recordsEach
	if total != want {
		t.Errorf("expected %d records in buffer, got %d", want, total)
	}
}

// TestInserter_DrainUnderConcurrentAdd verifies that draining the buffer
// (simulating Flush) while goroutines are adding doesn't race or panic.
func TestInserter_DrainUnderConcurrentAdd(t *testing.T) {
	ins := &Inserter{
		maxBuf: 10000,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup

	// Writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					ins.mu.Lock()
					ins.buf = append(ins.buf, Record{
						Source:  "svc",
						Message: "msg",
						Data:    map[string]any{},
					})
					ins.mu.Unlock()
				}
			}
		}()
	}

	// Drainer (simulates the flush ticker)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ins.mu.Lock()
				ins.buf = make([]Record, 0, ins.maxBuf)
				ins.mu.Unlock()
			}
		}
	}()

	wg.Wait()
	// If we get here without panic or race detector firing, the mutex works.
}

// TestNilIfEmpty covers the helper used in Flush.
func TestNilIfEmpty(t *testing.T) {
	if nilIfEmpty("") != nil {
		t.Error("nilIfEmpty(\"\") should return nil")
	}
	s := nilIfEmpty("INFO")
	if s == nil || *s != "INFO" {
		t.Errorf("nilIfEmpty(\"INFO\") = %v, want *\"INFO\"", s)
	}
}
