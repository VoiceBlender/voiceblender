package leg

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestStream_DirectionGatesLoops(t *testing.T) {
	cases := []struct {
		dir             string
		sends, receives bool
	}{
		{"", true, true}, // unnegotiated behaves like sendrecv, as before multi-stream
		{sipmod.DirSendRecv, true, true},
		{sipmod.DirSendOnly, true, false},
		{sipmod.DirRecvOnly, false, true},
		{sipmod.DirInactive, false, false},
	}
	for _, c := range cases {
		s := &mediaStream{negotiatedDir: c.dir}
		if got := s.sends(); got != c.sends {
			t.Errorf("sends(%q) = %v, want %v", c.dir, got, c.sends)
		}
		if got := s.receives(); got != c.receives {
			t.Errorf("receives(%q) = %v, want %v", c.dir, got, c.receives)
		}
	}
}

func TestStream_DTMFDefaultPTsUnchanged(t *testing.T) {
	// A peer that advertised no telephone-event must still have the historical
	// 100/101 pair accepted, or this refactor would silently break DTMF.
	s := &mediaStream{}
	for _, pt := range []uint8{100, 101} {
		if !s.acceptsDTMFPT(pt) {
			t.Errorf("PT %d must be accepted when the peer advertised none", pt)
		}
	}
	if s.acceptsDTMFPT(96) {
		t.Error("PT 96 must not be accepted without negotiation")
	}
}

func TestStream_DTMFUsesNegotiatedPT(t *testing.T) {
	l := newTestSIPLeg(codec.CodecPCMU)
	remote := &sipmod.SDPMedia{DTMFEventPTs: map[uint8]int{96: 8000}}
	l.configureDTMF(l.prim, remote)

	if !l.prim.acceptsDTMFPT(96) {
		t.Error("a peer advertising telephone-event on PT 96 must have its DTMF accepted")
	}
	// Once the peer has named its PTs, the conventional defaults no longer apply.
	if l.prim.acceptsDTMFPT(101) {
		t.Error("PT 101 must not be accepted when the peer advertised only 96")
	}
	if l.prim.dtmfSendPT != 96 || l.prim.dtmfClockRate != 8000 {
		t.Errorf("send params = PT %d rate %d, want 96/8000", l.prim.dtmfSendPT, l.prim.dtmfClockRate)
	}
}

func TestStream_ConfigureDTMFResetsRecvPTs(t *testing.T) {
	l := newTestSIPLeg(codec.CodecPCMU)
	l.configureDTMF(l.prim, &sipmod.SDPMedia{DTMFEventPTs: map[uint8]int{96: 8000}})
	// A renegotiation to a peer with no telephone-event must not leave the
	// previous PT set behind.
	l.configureDTMF(l.prim, &sipmod.SDPMedia{})
	if l.prim.acceptsDTMFPT(96) {
		t.Error("stale negotiated PT survived renegotiation")
	}
	if !l.prim.acceptsDTMFPT(101) {
		t.Error("defaults must be restored when the peer advertises none")
	}
}

func TestLeg_PrimaryStreamAlwaysInitialized(t *testing.T) {
	l := newTestSIPLeg(codec.CodecPCMU)
	if l.prim == nil {
		t.Fatal("prim must be non-nil after construction")
	}
	if !l.prim.primary || l.prim.id != primaryStreamID || l.prim.index != 0 {
		t.Errorf("primary stream = %+v, want id %q at index 0", l.prim, primaryStreamID)
	}
	if len(l.streams) != 1 || l.streams[0] != l.prim {
		t.Error("the primary stream must be registered in streams")
	}
	if got, ok := l.streamByID(primaryStreamID); !ok || got != l.prim {
		t.Error("streamByID must find the primary stream")
	}
}

func TestCloseStream_RemovesSecondaryAndTombstonesSlot(t *testing.T) {
	l := newTestSIPLeg(codec.CodecPCMU)

	sec := &mediaStream{id: "1", mid: "1", index: 1}
	l.streams = append(l.streams, sec)
	l.mlines.Append(sipmod.MLineSlot{Media: "audio", MID: "0", State: sipmod.SlotActive, StreamID: "0"})
	l.mlines.Append(sipmod.MLineSlot{Media: "audio", MID: "1", State: sipmod.SlotActive, StreamID: "1"})

	l.closeStream(sec)

	if len(l.streams) != 1 || l.streams[0] != l.prim {
		t.Errorf("streams = %d entries, want only the primary", len(l.streams))
	}
	// The slot must survive as a tombstone: RFC 3264 §8 forbids shrinking the
	// m-line vector, so subsequent offers still carry it with port 0.
	if l.mlines.Len() != 2 {
		t.Fatalf("m-line count = %d, want 2", l.mlines.Len())
	}
	if got := l.mlines.Slot(1).State; got != sipmod.SlotTombstone {
		t.Errorf("slot state = %v, want tombstone", got)
	}
}

func TestCloseStream_IgnoresPrimary(t *testing.T) {
	l := newTestSIPLeg(codec.CodecPCMU)
	l.closeStream(l.prim)
	if len(l.streams) != 1 {
		t.Error("closing the primary stream must be a no-op — it carries the call")
	}
}

func TestCloseStream_ReleasesPort(t *testing.T) {
	alloc, err := sipmod.NewPortAllocator(41000, 41200)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	sess, err := sipmod.NewRTPSessionFromAllocator(alloc)
	if err != nil {
		t.Fatalf("NewRTPSessionFromAllocator: %v", err)
	}
	port := sess.LocalPort()

	l := newTestSIPLeg(codec.CodecPCMU)
	sec := &mediaStream{id: "1", index: 1, rtpSess: sess}
	l.streams = append(l.streams, sec)

	l.closeStream(sec)

	// The port must be reusable; allocating the whole range again would fail if
	// the closed stream leaked it.
	seen := false
	for i := 0; i < 201; i++ {
		p, err := alloc.Allocate()
		if err != nil {
			break
		}
		if p == port {
			seen = true
			break
		}
	}
	if !seen {
		t.Errorf("port %d was not returned to the allocator", port)
	}
}
