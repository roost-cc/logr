package sender

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roost-cc/logr/internal/wire"
)

// listen accepts connections on a unix socket and counts the lines received.
func listen(t *testing.T) (addr string, lines <-chan int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan int, 1)
	go func() {
		n := 0
		defer func() { out <- n }()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			n++
		}
	}()
	return "unix://" + path, out
}

// TestCloseDeliversQueued sends records and closes at once, as logr-in does
// when a short stdin hits EOF before the connection is up.
func TestCloseDeliversQueued(t *testing.T) {
	addr, lines := listen(t)
	s, err := New(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	for range n {
		s.Send(wire.Record{Source: "test", Message: "line", Level: "INFO"})
	}
	s.Close()
	select {
	case got := <-lines:
		if got != n {
			t.Fatalf("server received %d records, want %d", got, n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not see the connection close")
	}
}

// TestCloseUnreachable returns promptly when no server is listening.
func TestCloseUnreachable(t *testing.T) {
	s, err := New(context.Background(), "unix://"+filepath.Join(t.TempDir(), "none.sock"))
	if err != nil {
		t.Fatal(err)
	}
	s.Send(wire.Record{Source: "test", Message: "line"})
	start := time.Now()
	s.Close()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v with no server", d)
	}
}

// TestCloseTwice allows repeated and concurrent calls to Close.
func TestCloseTwice(t *testing.T) {
	addr, _ := listen(t)
	s, err := New(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	s.Send(wire.Record{Source: "test", Message: "line"})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(s.Close)
	}
	wg.Wait()
	s.Close()
}

// TestCloseStalledPeer returns after closeTimeout when the server accepts the
// connection but never reads, leaving a write blocked.
func TestCloseStalledPeer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := ln.Accept(); err == nil {
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	})

	defer func(d time.Duration) { closeTimeout = d }(closeTimeout)
	closeTimeout = 200 * time.Millisecond

	s, err := New(context.Background(), "unix://"+path)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 4096)
	for range maxPending {
		s.Send(wire.Record{Source: "test", Message: big})
	}
	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(closeTimeout + time.Second):
		t.Fatal("Close did not return with a stalled peer")
	}
}
