// Package wsutilx provides shared helpers for both halves of a WebSocket
// client: recv loops and the write path.
//
// gobwas/ws's NextFrame ignores ctx cancellation; read deadlines bound the
// wait so half-open sockets can't pin goroutines indefinitely. Writes have
// the mirror-image problem: a peer that stops draining its receive window
// wedges conn.Write forever, so every write is deadline-bounded too.
package wsutilx

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// DurationVar is an atomic time.Duration suitable for package-level
// configuration knobs that may be read concurrently by recv loops while
// tests mutate them.
type DurationVar struct{ ns atomic.Int64 }

// Load returns the current value.
func (v *DurationVar) Load() time.Duration { return time.Duration(v.ns.Load()) }

// Store atomically replaces the value.
func (v *DurationVar) Store(d time.Duration) { v.ns.Store(int64(d)) }

// DefaultReadTimeout caps inter-frame idle on application WebSockets.
// 60s = 30s ping interval + 1 missed ping + margin. Tests may override.
var DefaultReadTimeout DurationVar

// DefaultWriteTimeout caps how long a single WebSocket frame write may
// block on a peer that has stopped draining. 5s matches wsmedia's
// DefaultWriteTimeout — a legitimate 640-byte audio frame never takes
// that long. Tests may override.
var DefaultWriteTimeout DurationVar

func init() {
	DefaultReadTimeout.Store(60 * time.Second)
	DefaultWriteTimeout.Store(5 * time.Second)
}

// SetReadDeadline pushes the read deadline forward on conn. Call before
// each blocking read inside a recv loop; pass timeout <= 0 to skip (e.g.
// when the caller manages deadlines explicitly). Errors are intentionally
// ignored: if the conn is already broken, the next read will surface it.
func SetReadDeadline(conn net.Conn, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
}

// SetWriteDeadline pushes the write deadline forward on conn. Call before
// each blocking write; pass timeout <= 0 to skip (e.g. when the caller
// manages deadlines explicitly). Errors are intentionally ignored: if the
// conn is already broken, the write itself will surface it.
func SetWriteDeadline(conn net.Conn, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
}

// WatchCancel spawns a single goroutine that pushes conn's read deadline
// to the past when ctx is cancelled, breaking any in-flight blocking read
// so the caller's loop can observe the cancellation and return. The
// returned stop function MUST be called when the loop exits (typically
// via defer) to terminate the watcher; failing to call it leaks the
// watcher goroutine until ctx is cancelled.
//
// Pass a nil ctx (or one with no Done channel) for a no-op — returns a
// no-op stop fn. This keeps callers simple in places where ctx isn't
// readily available.
func WatchCancel(ctx context.Context, conn net.Conn) func() {
	if ctx == nil {
		return func() {}
	}
	done := ctx.Done()
	if done == nil {
		return func() {}
	}
	stopCh := make(chan struct{})
	go func() {
		select {
		case <-done:
			// Push deadline to a point in the past so any blocking read
			// returns os.ErrDeadlineExceeded immediately.
			_ = conn.SetReadDeadline(time.Unix(1, 0))
		case <-stopCh:
		}
	}()
	return func() { close(stopCh) }
}

// LockedWriter serializes client-side WebSocket frame writes to a net.Conn
// and bounds each one with a write deadline.
//
// Serialization is required for correctness, not merely for safety: a
// WebSocket frame is a header write followed by a payload write (gobwas
// ws.WriteFrame issues them as two separate conn.Write calls), so two
// unsynchronized writers interleave and corrupt the stream. Any client
// with both a send loop and a read path that answers pings therefore
// needs a single writer shared by both halves.
//
// The mutex is deliberately held across the gobwas write call. That is the
// only way to make the header+payload pair atomic, and it is safe precisely
// because the deadline set immediately beforehand bounds how long the write
// — and therefore the lock hold — can last. Do not "fix" this by dropping
// the lock before the write.
//
// A write that fails on the deadline leaves the stream mid-frame and
// unrecoverable. Callers must tear the connection down rather than retry.
type LockedWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

// NewLockedWriter returns a LockedWriter writing client-side (masked)
// frames to conn.
func NewLockedWriter(conn net.Conn) *LockedWriter { return &LockedWriter{conn: conn} }

// WriteText writes data as a single text frame.
func (lw *LockedWriter) WriteText(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	SetWriteDeadline(lw.conn, DefaultWriteTimeout.Load())
	return wsutil.WriteClientText(lw.conn, data)
}

// WriteBinary writes data as a single binary frame.
func (lw *LockedWriter) WriteBinary(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	SetWriteDeadline(lw.conn, DefaultWriteTimeout.Load())
	return wsutil.WriteClientBinary(lw.conn, data)
}

// WriteControl writes a control frame (typically ws.OpPong) with payload.
func (lw *LockedWriter) WriteControl(op ws.OpCode, payload []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	SetWriteDeadline(lw.conn, DefaultWriteTimeout.Load())
	return wsutil.WriteClientMessage(lw.conn, op, payload)
}
