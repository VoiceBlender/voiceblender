package leg

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// One failed send must not end the stream's outgoing audio for the rest of
// the call.
func TestWriteLoop_SurvivesFailedSends(t *testing.T) {
	l, peer := newStreamingLeg(t, 0)
	sess := l.prim.rtpSess
	buf := make([]byte, 1500)

	readOne := func(within time.Duration) bool {
		peer.conn.SetReadDeadline(time.Now().Add(within))
		_, _, err := peer.conn.ReadFromUDP(buf)
		return err == nil
	}
	if !readOne(time.Second) {
		t.Fatal("no RTP from the leg before the failed sends")
	}

	// Port 0 makes the send fail on Linux and macOS. The peer sends nothing,
	// so symmetric RTP does not move the remote back.
	if err := sess.SetRemote("127.0.0.1", 0); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	probe := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 1}, Payload: make([]byte, 160)}
	if err := sess.WriteRTP(probe); err == nil {
		t.Skip("a send to port 0 does not fail here")
	}
	time.Sleep(100 * time.Millisecond) // several 20 ms ticks, each a failed send
	for readOne(5 * time.Millisecond) {
	}

	if err := sess.SetRemote("127.0.0.1", peer.port()); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	if !readOne(time.Second) {
		t.Fatal("the leg sent no more RTP after a failed send")
	}
}

// Closing a stream's socket is how a removed stream is stopped, so the loop
// must still end on a closed socket.
func TestWriteLoop_EndsWhenSocketCloses(t *testing.T) {
	l, _ := newStreamingLeg(t, 0)

	before := settledWriteLoops(t)
	if before == 0 {
		t.Fatal("no writeLoop running")
	}
	l.prim.rtpSess.Close()

	deadline := time.Now().Add(time.Second)
	for writeLoops() >= before {
		if time.Now().After(deadline) {
			t.Fatal("writeLoop still running after its socket closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeLoops() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "(*SIPLeg).writeLoop(")
}

// settledWriteLoops waits for loops left by earlier tests to finish exiting.
func settledWriteLoops(t *testing.T) int {
	t.Helper()
	n := writeLoops()
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		m := writeLoops()
		if m == n {
			return n
		}
		n = m
	}
	return n
}
