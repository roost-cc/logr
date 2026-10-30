// Package sender delivers wire.Records to logr-server over a unix or TCP socket.
// It reconnects automatically and buffers up to maxPending records while disconnected.
package sender

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/roost-cc/logr/internal/wire"
)

const maxPending = 2000

// Sender is safe for concurrent use.
type Sender struct {
	ch      chan wire.Record
	ctx     context.Context
	cancel  context.CancelFunc
	closing chan struct{} // closed by Close: deliver what is queued, then stop
	done    chan struct{}

	closeOnce sync.Once

	mu   sync.Mutex
	conn net.Conn // the live connection, expired by abort
}

// closeTimeout bounds how long Close waits to deliver queued records.
var closeTimeout = 5 * time.Second

// New creates a Sender that connects to addr in the background.
// addr formats: "unix:///tmp/logr.sock" or "tcp://host:5170"
func New(ctx context.Context, addr string) (*Sender, error) {
	network, hostaddr, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &Sender{
		ch:      make(chan wire.Record, maxPending),
		ctx:     sctx,
		cancel:  cancel,
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go s.loop(network, hostaddr)
	return s, nil
}

// Send enqueues a record. If the buffer is full the oldest record is dropped.
func (s *Sender) Send(r wire.Record) {
	select {
	case s.ch <- r:
	default:
		<-s.ch // drop oldest
		s.ch <- r
	}
}

// Close delivers queued records and shuts down the sender. It gives up after
// closeTimeout, or at once if the server cannot be reached. It may be called
// more than once and from several goroutines; every call returns once the
// sender has stopped.
func (s *Sender) Close() {
	s.closeOnce.Do(s.shutdown)
	<-s.done
}

func (s *Sender) shutdown() {
	close(s.closing)
	select {
	case <-s.done:
	case <-time.After(closeTimeout):
		log.Printf("sender: gave up delivering %d queued records", len(s.ch))
	}
	s.abort()
	<-s.done
}

// abort cancels the sender and expires the live connection, so a write blocked
// on an unresponsive peer returns at once.
func (s *Sender) abort() {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.SetDeadline(time.Now())
	}
}

// setConn records the live connection, expiring it at once if abort has
// already run.
func (s *Sender) setConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	if conn != nil && s.ctx.Err() != nil {
		conn.SetDeadline(time.Now())
	}
}

func (s *Sender) isClosing() bool {
	select {
	case <-s.closing:
		return true
	default:
		return false
	}
}

func (s *Sender) loop(network, addr string) {
	defer close(s.done)

	var pending []wire.Record

	for s.ctx.Err() == nil {
		conn := s.dialLoop(network, addr)
		if conn == nil {
			return
		}
		s.setConn(conn)
		pending = s.stream(conn, pending)
		s.setConn(nil)
		conn.Close()
		if s.isClosing() {
			if n := len(pending) + len(s.ch); n > 0 {
				log.Printf("sender: dropped %d records on close", n)
			}
			return
		}
		if s.ctx.Err() == nil && len(pending) > 0 {
			select {
			case <-s.ctx.Done():
				return
			case <-s.closing:
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (s *Sender) dialLoop(network, addr string) net.Conn {
	var d net.Dialer
	for {
		conn, err := d.DialContext(s.ctx, network, addr)
		if err == nil {
			return conn
		}
		if s.isClosing() {
			log.Printf("sender: dropped %d records on close: %v", len(s.ch), err)
			return nil
		}
		select {
		case <-s.ctx.Done():
			return nil
		case <-s.closing:
		case <-time.After(2 * time.Second):
			log.Printf("sender: connecting to %s://%s … (%v)", network, addr, err)
		}
	}
}

// stream flushes pending then drains ch into conn until error or ctx cancel.
// Returns any records that still need to be retried.
func (s *Sender) stream(conn net.Conn, pending []wire.Record) []wire.Record {
	w := bufio.NewWriterSize(conn, 64*1024)
	enc := json.NewEncoder(w)

	flush := func() error {
		return w.Flush()
	}

	// Flush pending first
	for i, r := range pending {
		if err := enc.Encode(r); err != nil || flush() != nil {
			return pending[i:]
		}
	}
	pending = pending[:0]

	// Stream new records
	for {
		select {
		case <-s.closing:
			// Deliver everything already queued on this connection.
			for {
				select {
				case r := <-s.ch:
					if err := enc.Encode(r); err != nil {
						pending = append(pending, r)
						return s.drainToPending(pending)
					}
				default:
					if err := flush(); err != nil {
						return s.drainToPending(pending)
					}
					return pending
				}
			}

		case <-s.ctx.Done():
			// Flush remaining channel records as pending for next connection
			for {
				select {
				case r := <-s.ch:
					pending = append(pending, r)
					if len(pending) >= maxPending {
						return pending
					}
				default:
					return pending
				}
			}

		case r := <-s.ch:
			if err := enc.Encode(r); err != nil {
				pending = append(pending, r)
				return s.drainToPending(pending)
			}
			// Batch-flush: drain what's already in the channel before flushing
			batchDone := false
			for !batchDone {
				select {
				case r2 := <-s.ch:
					if err := enc.Encode(r2); err != nil {
						pending = append(pending, r2)
						return s.drainToPending(pending)
					}
				default:
					batchDone = true
				}
			}
			if err := flush(); err != nil {
				return s.drainToPending(pending)
			}
		}
	}
}

func (s *Sender) drainToPending(pending []wire.Record) []wire.Record {
	for len(pending) < maxPending {
		select {
		case r := <-s.ch:
			pending = append(pending, r)
		default:
			return pending
		}
	}
	return pending
}

func parseAddr(addr string) (network, hostaddr string, err error) {
	switch {
	case strings.HasPrefix(addr, "unix://"):
		return "unix", strings.TrimPrefix(addr, "unix://"), nil
	case strings.HasPrefix(addr, "tcp://"):
		return "tcp", strings.TrimPrefix(addr, "tcp://"), nil
	}
	return "", "", fmt.Errorf("sender: unknown scheme in %q (want unix:// or tcp://)", addr)
}
