package leg

import (
	"regexp"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/pion/webrtc/v4"
)

// negotiatePCMediaPair runs offer/answer with trickle ICE between two PCMedia
// peers. mungeOffer, when set, rewrites the offer the callee sees.
func negotiatePCMediaPair(t *testing.T, caller, callee *PCMedia, mungeOffer func(string) string) {
	t.Helper()
	caller.PC().OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = callee.PC().AddICECandidate(c.ToJSON())
		}
	})
	callee.PC().OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = caller.PC().AddICECandidate(c.ToJSON())
		}
	})

	offer, err := caller.PC().CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := caller.PC().SetLocalDescription(offer); err != nil {
		t.Fatalf("caller SetLocalDescription: %v", err)
	}
	if mungeOffer != nil {
		offer.SDP = mungeOffer(offer.SDP)
	}
	if err := callee.PC().SetRemoteDescription(offer); err != nil {
		t.Fatalf("callee SetRemoteDescription: %v", err)
	}
	answer, err := callee.PC().CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := callee.PC().SetLocalDescription(answer); err != nil {
		t.Fatalf("callee SetLocalDescription: %v", err)
	}
	if err := caller.PC().SetRemoteDescription(answer); err != nil {
		t.Fatalf("caller SetRemoteDescription: %v", err)
	}
	caller.Start()
	callee.Start()
}

func waitPCConnected(t *testing.T, peers ...*PCMedia) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, m := range peers {
		for m.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
			if time.Now().After(deadline) {
				t.Fatalf("peer connection not connected: %s", m.PC().ConnectionState())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func newDisconnectPair(t *testing.T) (caller, callee *PCMedia, reasons chan string) {
	t.Helper()
	reasons = make(chan string, 4)
	caller, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecOpus, Log: testLogger(), LoopbackICE: true})
	if err != nil {
		t.Fatalf("caller NewPCMedia: %v", err)
	}
	t.Cleanup(func() { caller.Close() })
	callee, err = NewPCMedia(PCMediaConfig{
		Codec:        codec.CodecOpus,
		Log:          testLogger(),
		LoopbackICE:  true,
		OnDisconnect: func(reason string) { reasons <- reason },
	})
	if err != nil {
		t.Fatalf("callee NewPCMedia: %v", err)
	}
	t.Cleanup(func() { callee.Close() })
	return caller, callee, reasons
}

// A peer that closes its connection sends a DTLS close_notify; pion answers
// by closing ours, which never passes through ICE failed/disconnected.
func TestPCMedia_PeerCloseFiresOnDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("involves real ICE/DTLS; skipped in -short")
	}
	caller, callee, reasons := newDisconnectPair(t)
	negotiatePCMediaPair(t, caller, callee, nil)
	waitPCConnected(t, caller, callee)

	if err := caller.Close(); err != nil {
		t.Fatalf("caller Close: %v", err)
	}

	select {
	case got := <-reasons:
		if got != PCPeerClosed {
			t.Fatalf("OnDisconnect reason = %q, want %q", got, PCPeerClosed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnDisconnect did not fire after the peer closed its connection")
	}
}

func TestPCMedia_LocalCloseDoesNotFireOnDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("involves real ICE/DTLS; skipped in -short")
	}
	caller, callee, reasons := newDisconnectPair(t)
	negotiatePCMediaPair(t, caller, callee, nil)
	waitPCConnected(t, caller, callee)

	if err := callee.Close(); err != nil {
		t.Fatalf("callee Close: %v", err)
	}

	select {
	case got := <-reasons:
		t.Fatalf("OnDisconnect fired with %q for a locally closed connection", got)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestPCMedia_DTLSFailureFiresOnDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("involves real ICE/DTLS; skipped in -short")
	}
	caller, callee, reasons := newDisconnectPair(t)

	fingerprint := regexp.MustCompile(`a=fingerprint:sha-256 [0-9A-Fa-f]{2}`)
	negotiatePCMediaPair(t, caller, callee, func(sdp string) string {
		if !fingerprint.MatchString(sdp) {
			t.Fatal("offer carries no sha-256 fingerprint to corrupt")
		}
		return fingerprint.ReplaceAllStringFunc(sdp, func(m string) string {
			if m[len(m)-2:] == "00" {
				return m[:len(m)-2] + "FF"
			}
			return m[:len(m)-2] + "00"
		})
	})

	select {
	case got := <-reasons:
		if got != PCDTLSFailed {
			t.Fatalf("OnDisconnect reason = %q, want %q", got, PCDTLSFailed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnDisconnect did not fire after the DTLS handshake failed")
	}
}
