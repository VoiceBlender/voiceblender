package leg

import (
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// rtpPeer is the far end of a streaming test leg: a loopback UDP socket that
// sends RTP to the leg. Nothing reads what the leg sends back.
type rtpPeer struct {
	conn *net.UDPConn
	to   *net.UDPAddr
}

// newStreamingLeg is a leg whose primary stream runs its real media loops
// against a UDP peer, which is what a re-INVITE lands on mid-call.
func newStreamingLeg(t *testing.T, jitterMs int) (*SIPLeg, *rtpPeer) {
	t.Helper()
	sess, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatalf("NewRTPSession: %v", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		sess.Close()
		t.Fatalf("listen peer: %v", err)
	}
	peer := &rtpPeer{conn: conn, to: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sess.LocalPort()}}

	l := newTestSIPLeg(codec.CodecPCMU)
	l.log = slog.New(slog.DiscardHandler)
	l.localIP = "127.0.0.1"
	l.supportedCodecs = []codec.CodecType{codec.CodecPCMU, codec.CodecG722}
	l.prim.rtpSess = sess
	l.prim.rtpPT = codec.CodecPCMU.PayloadType()
	l.prim.negotiatedDir = sipmod.DirSendRecv
	l.SetJitterBuffer(jitterMs, 0)
	if err := sess.SetRemote("127.0.0.1", peer.port()); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}
	t.Cleanup(func() {
		l.cancel()
		sess.Close()
		conn.Close()
	})

	l.setupStreamMedia(l.prim)
	if l.prim.live() == nil {
		t.Fatal("setupStreamMedia did not publish a live codec")
	}
	return l, peer
}

func (p *rtpPeer) port() int { return p.conn.LocalAddr().(*net.UDPAddr).Port }

func (p *rtpPeer) send(t *testing.T, pkt *rtp.Packet) {
	raw, err := pkt.Marshal()
	if err != nil {
		t.Errorf("marshal RTP: %v", err)
		return
	}
	p.conn.WriteToUDP(raw, p.to)
}

// offer builds a re-offer from the peer. Each rtpmap is "<pt> <name>/<rate>".
func (p *rtpPeer) offer(direction string, rtpmaps ...string) []byte {
	var pts, attrs []string
	for _, m := range rtpmaps {
		pt, _, _ := strings.Cut(m, " ")
		pts = append(pts, pt)
		attrs = append(attrs, "a=rtpmap:"+m)
	}
	lines := []string{
		"v=0",
		"o=- 1 1 IN IP4 127.0.0.1",
		"s=-",
		"c=IN IP4 127.0.0.1",
		"t=0 0",
		"m=audio " + strconv.Itoa(p.port()) + " RTP/AVP " + strings.Join(pts, " "),
	}
	lines = append(lines, attrs...)
	lines = append(lines, "a="+direction, "")
	return []byte(strings.Join(lines, "\r\n"))
}

// streamAudio sends a PCMU packet every millisecond until the test ends.
func (p *rtpPeer) streamAudio(t *testing.T) {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		pkt := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 0, SSRC: 0xfeed},
			Payload: make([]byte, 160),
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			pkt.SequenceNumber++
			pkt.Timestamp += 160
			p.send(t, pkt)
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

type frameCounter struct{ n atomic.Int64 }

func (c *frameCounter) Write(p []byte) (int, error) {
	c.n.Add(1)
	return len(p), nil
}

// TestReInviteWhileMediaFlows applies re-offers to a leg whose loops are
// decoding audio, while another goroutine polls the leg the way the API and a
// room do. It asserts little by itself: its job is to fail under -race if
// applying an offer rewrites state the loops or the pollers read unguarded.
func TestReInviteWhileMediaFlows(t *testing.T) {
	l, peer := newStreamingLeg(t, 0)
	decoded := &frameCounter{}
	l.SetInTap(decoded)
	peer.streamAudio(t)

	stop := make(chan struct{})
	var pollers sync.WaitGroup
	pollers.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			l.SampleRate()
			l.RTPStats()
			l.StreamMedia(primaryStreamID)
			time.Sleep(100 * time.Microsecond)
		}
	})

	for i := 0; i < 24; i++ {
		// Hold, resume, and a move of telephone-event to another payload type.
		dtmf, direction := "101 telephone-event/8000", sipmod.DirSendRecv
		if i%2 == 1 {
			direction = sipmod.DirSendOnly
		}
		if i%4 >= 2 {
			dtmf = "96 telephone-event/8000"
		}
		if answer, _, _ := l.ApplyRemoteOffer(peer.offer(direction, "0 PCMU/8000", dtmf)); len(answer) == 0 {
			t.Fatalf("offer %d: no answer", i)
		}

		// Let the read loop run on whatever the offer left behind.
		target := decoded.n.Load() + 3
		deadline := time.Now().Add(2 * time.Second)
		for decoded.n.Load() < target {
			if time.Now().After(deadline) {
				t.Fatalf("offer %d: audio stopped being decoded", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
	close(stop)
	pollers.Wait()

	if got := l.SampleRate(); got != 8000 {
		t.Errorf("SampleRate() = %d, want 8000", got)
	}
	if got := l.RTPStats().PacketsReceived; got == 0 {
		t.Error("RTPStats() counted no packets on a leg that decoded audio")
	}
}

// TestReInviteMovesDTMFPayloadType: a re-offer that moves telephone-event to
// another payload type must reach the running read loop, which works from the
// published codec state and not from the stream's negotiated fields.
func TestReInviteMovesDTMFPayloadType(t *testing.T) {
	l, peer := newStreamingLeg(t, 0)
	digits := make(chan rune, 16)
	l.OnDTMF(func(d rune) { digits <- d })

	var seq uint16
	ts := uint32(1000)
	press := func(digit rune, pt uint8) {
		pkts := sipmod.GenerateDTMFPackets(digit, pt, 0xfeed, seq, ts, 160)
		for _, pkt := range pkts {
			peer.send(t, pkt)
		}
		seq += uint16(len(pkts))
		ts += 1600
	}
	expect := func(want rune) {
		t.Helper()
		select {
		case got := <-digits:
			if got != want {
				t.Fatalf("digit = %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("digit %q was not reported", want)
		}
	}

	l.ApplyRemoteOffer(peer.offer(sipmod.DirSendRecv, "0 PCMU/8000", "96 telephone-event/8000"))
	press('1', 96)
	expect('1')

	// A second telephone-event line leaves the payload type we send on alone,
	// so the set we accept is the only thing that changes.
	l.ApplyRemoteOffer(peer.offer(sipmod.DirSendRecv,
		"0 PCMU/8000", "96 telephone-event/8000", "110 telephone-event/48000"))
	press('2', 110)
	expect('2')

	// 101 was the default and is no longer telephone-event: '3' must be
	// dropped, so the next digit reported is the '4' sent after it.
	press('3', 101)
	press('4', 96)
	expect('4')
}

// TestReInviteRateChangeWithJitterBuffer: with a jitter buffer and no audio
// arriving, the pop loop fills every tick with silence sized for the running
// codec. A re-offer that changes the rate must change that size, and must not
// rewrite anything the pop loop reads unguarded.
func TestReInviteRateChangeWithJitterBuffer(t *testing.T) {
	l, peer := newStreamingLeg(t, 60)

	nextFrame := func() int {
		t.Helper()
		select {
		case f := <-l.prim.inFrames:
			return len(f)
		case <-time.After(time.Second):
			t.Fatal("the pop loop stopped producing frames")
			return 0
		}
	}
	// A frame sized before the switch may still be queued behind it.
	expectFrames := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for nextFrame() != want {
			if time.Now().After(deadline) {
				t.Fatalf("no %d byte silence frame after the re-offer", want)
			}
		}
		for i := 0; i < 3; i++ {
			if got := nextFrame(); got != want {
				t.Fatalf("silence frame is %d bytes, want %d", got, want)
			}
		}
	}

	expectFrames(320)
	for i := 0; i < 3; i++ {
		// Leave the pop loop to run a few ticks on its own between offers.
		time.Sleep(50 * time.Millisecond)
		l.ApplyRemoteOffer(peer.offer(sipmod.DirSendRecv, "9 G722/8000", "101 telephone-event/8000"))
		time.Sleep(50 * time.Millisecond)
		expectFrames(640)

		time.Sleep(50 * time.Millisecond)
		l.ApplyRemoteOffer(peer.offer(sipmod.DirSendRecv, "0 PCMU/8000", "96 telephone-event/8000"))
		time.Sleep(50 * time.Millisecond)
		expectFrames(320)
	}
}
