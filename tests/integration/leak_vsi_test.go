//go:build integration

package integration

import (
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
)

// TestLeak_VSIZombieConnection simulates a client whose TCP connection has
// gone half-open (no FIN, no RST, no traffic). With the read deadline in
// place, the server's vsiRecvLoop must wake within DefaultReadTimeout, run
// its defers (unsubscribe from bus, close ping/send goroutines), and free
// every goroutine it pinned. Without the fix, those goroutines persist
// indefinitely.
func TestLeak_VSIZombieConnection(t *testing.T) {
	// Shrink timeout so the test runs in seconds rather than a minute.
	prevTimeout := wsutilx.DefaultReadTimeout
	wsutilx.DefaultReadTimeout = 200 * time.Millisecond
	defer func() { wsutilx.DefaultReadTimeout = prevTimeout }()

	inst := newTestInstance(t, "leak-vsi")

	// Snapshot goroutine count before opening any VSI connection.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	// Open several VSI connections, read the initial "connected" frame so
	// the server's recv loop is parked inside NextFrame, then leave them
	// dangling — never write, never read, never close. This is the true
	// half-open shape: the client is unresponsive but the kernel TCP timer
	// hasn't yet declared the socket dead. The server cannot distinguish
	// this from a network partition; only its read deadline can.
	const N = 5
	conns := make([]net.Conn, 0, N)
	for i := 0; i < N; i++ {
		conn := dialVSI(t, inst)
		readWSFrame(t, conn, 2*time.Second)
		conns = append(conns, conn)
	}
	// Hold a reference to keep the conns alive for the test duration so the
	// GC can't reclaim them and confound the goroutine accounting.
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	// Wait long enough for the server-side read deadline to expire and all
	// per-client goroutines to drain.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		after := runtime.NumGoroutine()
		// Allow a small fudge for transient noise (e.g. metric flushers).
		if after-before <= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Diagnostic dump of any vsi-related frames still on the stack.
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	stacks := string(buf[:n])
	for _, marker := range []string{"vsiRecvLoop", "ws_events.go"} {
		if strings.Contains(stacks, marker) {
			t.Errorf("goroutine still references %s after %d zombie clients dropped\n%s",
				marker, N, stacks)
			return
		}
	}
	t.Errorf("goroutine count did not return to baseline: before=%d after=%d (delta=%d)",
		before, runtime.NumGoroutine(), runtime.NumGoroutine()-before)
}
