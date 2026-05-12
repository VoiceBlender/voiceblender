package wsmedia

import (
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// stuckConn is an in-process net.Conn whose Write blocks until the write
// deadline trips (returning os.ErrDeadlineExceeded) or Close is called.
// Read blocks until Close, then returns io.EOF. Used to deterministically
// exercise the write-deadline path that real TCP only triggers when send
// buffers fill.
type stuckConn struct {
	mu       sync.Mutex
	deadline atomic.Value // time.Time
	closed   chan struct{}
	once     sync.Once
}

func newStuckConn() *stuckConn { return &stuckConn{closed: make(chan struct{})} }

func (s *stuckConn) Read(p []byte) (int, error) {
	<-s.closed
	return 0, io.EOF
}

func (s *stuckConn) Write(p []byte) (int, error) {
	dl, _ := s.deadline.Load().(time.Time)
	if dl.IsZero() {
		<-s.closed
		return 0, io.ErrClosedPipe
	}
	wait := time.Until(dl)
	if wait <= 0 {
		return 0, os.ErrDeadlineExceeded
	}
	select {
	case <-s.closed:
		return 0, io.ErrClosedPipe
	case <-time.After(wait):
		return 0, os.ErrDeadlineExceeded
	}
}

func (s *stuckConn) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *stuckConn) LocalAddr() net.Addr                { return nil }
func (s *stuckConn) RemoteAddr() net.Addr               { return nil }
func (s *stuckConn) SetDeadline(time.Time) error        { return nil }
func (s *stuckConn) SetReadDeadline(time.Time) error    { return nil }
func (s *stuckConn) SetWriteDeadline(t time.Time) error { s.deadline.Store(t); return nil }
