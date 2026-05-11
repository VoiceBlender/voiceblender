// Package wsutilx provides shared helpers for WebSocket recv loops.
// gobwas/ws's NextFrame ignores ctx cancellation; read deadlines bound
// the wait so half-open sockets can't pin goroutines indefinitely.
package wsutilx

import (
	"context"
	"net"
	"sync/atomic"
	"time"
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

func init() {
	DefaultReadTimeout.Store(60 * time.Second)
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
