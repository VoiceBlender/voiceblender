// Package wsutilx provides small helpers shared by the codebase's many
// WebSocket recv-loop implementations. The central problem it addresses is
// that gobwas/ws's wsutil.Reader.NextFrame ignores context cancellation —
// when the underlying TCP connection enters a zombie state (no FIN, no RST
// observable to userspace, e.g. carrier NAT timeout), the read blocks
// indefinitely, the surrounding loop never wakes to check ctx.Done, and the
// goroutine plus everything it pins (event-bus subscriptions, channels,
// peer goroutines) leaks. Read deadlines bound the wait so loops always
// wake within a known interval.
package wsutilx

import (
	"context"
	"net"
	"time"
)

// DefaultReadTimeout is a sensible upper bound for inter-frame idle on
// long-lived application WebSockets. Server-driven pings run every 30s in
// our codebase, so 60s gives one missed ping plus margin before the recv
// loop unblocks and cleanup runs.
//
// Declared as a var (rather than const) so integration tests can shrink it
// to verify zombie-connection cleanup without waiting a full minute.
var DefaultReadTimeout = 60 * time.Second

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
