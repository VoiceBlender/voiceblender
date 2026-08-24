//go:build integration

package integration

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// Every port this harness picks comes from a band below the OS ephemeral range
// (Linux 32768-60999, macOS 49152-65535), and no two uses share a band.
//
// The harness used to take its SIP port by binding :0, reading the port back
// and closing the socket, while RTP sessions bound :0 too because
// EngineConfig.PortAllocator was never set. Both drew from the ephemeral pool,
// so an RTP session could be handed a port an instance had already picked for
// SIP but not yet bound. The call's audio then arrived at a SIP transport,
// which logged a parse error per packet and dropped it, and whichever test was
// asserting on that audio failed on silence.
const (
	sipPortBandMin = 8192
	sipPortBandMax = 16383

	rtpPoolMin = 16384
	rtpPoolMax = 28671

	// cfg.RTPPortMin/Max is pion's ephemeral range (WebRTC, LiveKit, WhatsApp
	// and MoQ legs). Pion allocates without consulting the SIP pool, so it gets
	// a band of its own.
	pionPortMin = 28672
	pionPortMax = 32767

	// Lowest ephemeral port on either platform we build for.
	lowestEphemeralPort = 32768
)

var rtpPool struct {
	once  sync.Once
	alloc *sipmod.PortAllocator
	err   error
}

// testRTPAllocator returns the pool every instance in this process allocates
// RTP ports from. One shared pool rather than a band per instance: ports come
// back on RTPSession.Close, and an instance's sockets outlive the t.Cleanup
// that would hand its band to the next test.
func testRTPAllocator(t *testing.T) *sipmod.PortAllocator {
	t.Helper()
	rtpPool.once.Do(func() {
		rtpPool.alloc, rtpPool.err = sipmod.NewPortAllocator(rtpPoolMin, rtpPoolMax)
	})
	if rtpPool.err != nil {
		t.Fatalf("RTP port pool %d-%d: %v", rtpPoolMin, rtpPoolMax, rtpPool.err)
	}
	return rtpPool.alloc
}

var sipPorts struct {
	mu   sync.Mutex
	next int
}

// reserveSIPPort returns a port free on every network the caller names that no
// live instance in this process holds. The cursor only comes back round after
// the whole band, and a port is handed out again only once nothing is bound to
// it -- an engine's sockets outlive the test that created it, so releasing a
// port on cleanup and trusting it to be free is what would hand the next
// instance a socket still in use.
func reserveSIPPort(t *testing.T, networks ...string) int {
	t.Helper()
	if len(networks) == 0 {
		networks = []string{"udp4", "tcp4"}
	}

	sipPorts.mu.Lock()
	defer sipPorts.mu.Unlock()
	for tried := 0; tried <= sipPortBandMax-sipPortBandMin; tried++ {
		if sipPorts.next < sipPortBandMin || sipPorts.next > sipPortBandMax {
			sipPorts.next = sipPortBandMin
		}
		port := sipPorts.next
		sipPorts.next++
		if portIsFree(port, networks) {
			return port
		}
	}
	t.Fatalf("no free port in the SIP band %d-%d", sipPortBandMin, sipPortBandMax)
	return 0
}

// portIsFree reports whether port can be bound on every named network. Another
// process on the machine may hold it, and once the cursor has come round the
// band a port may still belong to a live instance, so a candidate is checked
// rather than assumed.
func portIsFree(port int, networks []string) bool {
	for _, network := range networks {
		host := "127.0.0.1"
		if strings.HasSuffix(network, "6") {
			host = "::1"
		}
		addr := net.JoinHostPort(host, strconv.Itoa(port))

		if strings.HasPrefix(network, "udp") {
			c, err := net.ListenPacket(network, addr)
			if err != nil {
				return false
			}
			c.Close()
			continue
		}
		l, err := net.Listen(network, addr)
		if err != nil {
			return false
		}
		l.Close()
	}
	return true
}

// requireIPv6Loopback skips the test when the host has no ::1 to bind.
func requireIPv6Loopback(t *testing.T, name string) {
	t.Helper()
	c, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("[%s] cannot bind UDP on [::1]: %v", name, err)
	}
	c.Close()
}

// Every harness constructor must keep its ports out of the OS ephemeral pool.
// A new one that forgets to wire the allocator brings back the flake this
// guards: RTP sessions bind :0 and can land on a SIP port that has been probed
// but not yet bound.
func TestHarnessPortsStayBelowTheEphemeralPool(t *testing.T) {
	constructors := []struct {
		name string
		new  func(*testing.T, string) *testInstance
	}{
		{"default", newTestInstance},
		{"metrics", newTestInstanceWithMetrics},
		{"ipv6", newTestInstanceIPv6},
		{"dualstack", newTestInstanceDualStack},
	}

	for _, c := range constructors {
		t.Run(c.name, func(t *testing.T) {
			inst := c.new(t, "ports-"+c.name)

			if inst.sipPort < sipPortBandMin || inst.sipPort > sipPortBandMax {
				t.Errorf("SIP port %d outside the reserved band %d-%d", inst.sipPort, sipPortBandMin, sipPortBandMax)
			}

			alloc := inst.engine.PortAllocator()
			if alloc == nil {
				t.Fatal("no RTP port allocator: sessions would bind :0 from the ephemeral pool")
			}
			min, max := alloc.Range()
			if max >= lowestEphemeralPort {
				t.Errorf("RTP pool %d-%d reaches into the ephemeral pool", min, max)
			}

			// Pion allocates from cfg.RTPPortMin/Max without consulting the SIP
			// pool, so the two must not overlap.
			pionMin, pionMax := inst.cfg.RTPPortMin, inst.cfg.RTPPortMax
			if pionMin == 0 || pionMax == 0 {
				t.Fatal("no pion port range: WebRTC/LiveKit/MoQ legs would bind :0")
			}
			if pionMax >= lowestEphemeralPort {
				t.Errorf("pion range %d-%d reaches into the ephemeral pool", pionMin, pionMax)
			}
			if pionMin <= max && min <= pionMax {
				t.Errorf("pion range %d-%d overlaps the SIP RTP pool %d-%d", pionMin, pionMax, min, max)
			}
		})
	}
}

// And the pool has to actually be used: a nil allocator reaches NewRTPSession,
// which binds :0 and puts the port back in the pool SIP ports come from.
func TestRTPSessionsBindInsideThePool(t *testing.T) {
	inst := newTestInstance(t, "rtp-pool")
	alloc := inst.engine.PortAllocator()
	if alloc == nil {
		t.Fatal("no RTP port allocator: sessions would bind :0 from the ephemeral pool")
	}
	min, max := alloc.Range()

	for i := 0; i < 8; i++ {
		sess, err := sipmod.NewRTPSessionFromAllocator(alloc)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		port := sess.LocalPort()
		sess.Close()
		if port < min || port > max {
			t.Fatalf("session %d bound port %d, outside the pool %d-%d", i, port, min, max)
		}
	}
}

// Two live instances may never share a SIP port: an engine's sockets outlive
// the test that created it, so a port handed out twice is a port already bound.
func TestSIPPortsDifferPerLiveInstance(t *testing.T) {
	seen := map[int]string{}
	for _, name := range []string{"sip-port-a", "sip-port-b", "sip-port-c"} {
		inst := newTestInstance(t, name)
		if other, dup := seen[inst.sipPort]; dup {
			t.Fatalf("[%s] got SIP port %d, already held by %s", name, inst.sipPort, other)
		}
		seen[inst.sipPort] = name
	}
}

// What makes reuse safe: a port anything is bound to is skipped rather than
// handed out. Without the check, wrapping the band would return live ports.
func TestReservedSIPPortsSkipBoundPorts(t *testing.T) {
	port := reserveSIPPort(t)

	conn, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("hold port %d: %v", port, err)
	}
	if portIsFree(port, []string{"udp4", "tcp4"}) {
		t.Errorf("port %d reported free while bound", port)
	}
	conn.Close()

	if !portIsFree(port, []string{"udp4", "tcp4"}) {
		t.Errorf("port %d reported in use after release", port)
	}
}
