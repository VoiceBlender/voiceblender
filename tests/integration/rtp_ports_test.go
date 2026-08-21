//go:build integration

package integration

import (
	"sync"
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// Each test instance gets a private RTP port range, disjoint from every other
// instance's and from the OS ephemeral pool.
//
// Without one, EngineConfig.PortAllocator is nil and every RTP session binds :0,
// drawing from the same OS ephemeral pool the harness uses to pick SIP ports --
// and the harness picks a SIP port by binding :0, reading the port back and
// closing the socket, so that port sits in the pool again until the engine binds
// it for real. An RTP session landing on it sends a call's audio into a SIP
// transport, which logs a parse error per packet and drops it. The call it
// belonged to then records silence, and whichever test was asserting on that
// audio fails somewhere unrelated to what it was testing.
//
// The band sits below the ephemeral range on both platforms we build for: Linux
// defaults to 32768-60999, macOS to 49152-65535.
const (
	rtpBandMin  = 16384
	rtpBandMax  = 32767
	rtpBandSize = 128
)

var rtpBands struct {
	mu   sync.Mutex
	free []int
	next int
}

// acquireRTPBand reserves a range for one instance and releases it when the test
// ends. Bands are recycled, so a suite with more instances than bands still runs
// as long as they are not all live at once.
func acquireRTPBand(t *testing.T) (min, max int) {
	t.Helper()
	rtpBands.mu.Lock()
	var base int
	switch {
	case len(rtpBands.free) > 0:
		base = rtpBands.free[len(rtpBands.free)-1]
		rtpBands.free = rtpBands.free[:len(rtpBands.free)-1]
	default:
		base = rtpBandMin + rtpBands.next*rtpBandSize
		if base+rtpBandSize-1 > rtpBandMax {
			rtpBands.mu.Unlock()
			t.Fatalf("RTP port bands exhausted: %d live instances at %d ports each does not "+
				"fit in %d-%d", rtpBands.next, rtpBandSize, rtpBandMin, rtpBandMax)
		}
		rtpBands.next++
	}
	rtpBands.mu.Unlock()

	t.Cleanup(func() {
		rtpBands.mu.Lock()
		rtpBands.free = append(rtpBands.free, base)
		rtpBands.mu.Unlock()
	})
	return base, base + rtpBandSize - 1
}

// newTestPortAllocator returns the allocator an instance's engine should use.
func newTestPortAllocator(t *testing.T) *sipmod.PortAllocator {
	t.Helper()
	min, max := acquireRTPBand(t)
	alloc, err := sipmod.NewPortAllocator(min, max)
	if err != nil {
		t.Fatalf("new port allocator for %d-%d: %v", min, max, err)
	}
	return alloc
}

// Every instance must get a band of its own, below the ephemeral pool. A shared
// or unset range is what let a call's RTP arrive at another instance's SIP port.
func TestInstancesGetDisjointRTPPortRanges(t *testing.T) {
	type span struct{ min, max int }
	var spans []span

	for _, name := range []string{"a", "b", "c"} {
		inst := newTestInstance(t, name)
		min, max := inst.engine.PortAllocator().Range()
		if min == 0 || max == 0 {
			t.Fatalf("[%s] no RTP port range: sessions would bind :0 from the ephemeral pool", name)
		}
		// 32768 is the lowest ephemeral port on either platform we build for.
		if max >= 32768 {
			t.Errorf("[%s] range %d-%d reaches into the OS ephemeral pool", name, min, max)
		}
		for _, s := range spans {
			if min <= s.max && s.min <= max {
				t.Errorf("[%s] range %d-%d overlaps another instance's %d-%d", name, min, max, s.min, s.max)
			}
		}
		spans = append(spans, span{min, max})
	}
}

// And the range has to actually be used: a nil allocator reaches NewRTPSession,
// which binds :0 and puts the port back in the pool the SIP ports come from.
func TestRTPSessionsBindInsideTheInstanceBand(t *testing.T) {
	inst := newTestInstance(t, "band")
	alloc := inst.engine.PortAllocator()
	min, max := alloc.Range()

	for i := 0; i < 8; i++ {
		sess, err := sipmod.NewRTPSessionFromAllocator(alloc)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		if p := sess.LocalPort(); p < min || p > max {
			sess.Close()
			t.Fatalf("session %d bound port %d, outside the instance band %d-%d", i, p, min, max)
		}
		sess.Close()
	}
}
