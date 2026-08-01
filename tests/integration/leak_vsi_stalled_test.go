//go:build integration

package integration

import (
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
)

// TestLeak_VSIStalledReader is the write-side sibling of
// TestLeak_VSIZombieConnection. There the client goes silent and the read
// deadline is what frees the connection; here the client stays connected and
// simply stops draining, which the read deadline cannot see — the socket is
// healthy, there is just no room left in it. The server's write blocks, and
// with it the send loop, every command response, and the recv loop that would
// otherwise return and run the deferred bus unsubscribe.
//
// The read timeout is deliberately left at its 60s default: if this test only
// passed because the read deadline eventually fired, it would be measuring the
// wrong mechanism. Everything here has to happen well inside that window.
func TestLeak_VSIStalledReader(t *testing.T) {
	prevWrite := wsutilx.DefaultWriteTimeout.Load()
	wsutilx.DefaultWriteTimeout.Store(300 * time.Millisecond)
	defer wsutilx.DefaultWriteTimeout.Store(prevWrite)

	inst := newTestInstance(t, "leak-vsi-stalled")

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 2*time.Second) // "connected", then never read again

	// Enough payload to overrun the socket send buffer and the peer's receive
	// window, so the server's write genuinely blocks rather than being
	// absorbed by the kernel. One shared 64 KiB string keeps the cost to a
	// transient marshal buffer per frame.
	big := strings.Repeat("x", 64*1024)
	for i := 0; i < 2000; i++ {
		inst.bus.Publish(events.LegRinging, &events.LegRingingData{
			LegScope: events.LegScope{LegID: "leg-stalled"},
			LegType:  "sip_inbound",
			From:     big,
		})
	}

	// Stay stalled comfortably past the write deadline before draining.
	// Without this the drain below unblocks the server's write and the
	// deadline never gets the chance to fire.
	time.Sleep(time.Second)

	// The server must hang up on its own. Drain whatever is already queued —
	// the read ends in EOF/reset if it did, or in our own deadline if it did
	// not.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 64*1024)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("server never disconnected the stalled client: the VSI write is unbounded")
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !isConnReset(err) {
			t.Logf("read ended with %v (treated as server hangup)", err)
		}
		break
	}

	// The disconnect is only half the fix. What made this a leak rather than a
	// stuck connection is the bus subscriber the handler never unregistered,
	// so require the goroutines to actually drain.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine()-before <= 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	stackBuf := make([]byte, 1<<20)
	n := runtime.Stack(stackBuf, true)
	stacks := string(stackBuf[:n])
	for _, marker := range []string{"vsiRecvLoop", "ws_events.go"} {
		if strings.Contains(stacks, marker) {
			t.Errorf("goroutine still references %s after the stalled client was dropped\n%s",
				marker, stacks)
			return
		}
	}
	t.Errorf("goroutine count did not return to baseline: before=%d after=%d (delta=%d)",
		before, runtime.NumGoroutine(), runtime.NumGoroutine()-before)
}

func isConnReset(err error) bool {
	return strings.Contains(err.Error(), "connection reset by peer")
}
