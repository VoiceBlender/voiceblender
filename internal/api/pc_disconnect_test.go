package api

import (
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

func TestPCDisconnectReason(t *testing.T) {
	cases := []struct{ cause, iceReason, want string }{
		{"failed", "ice_failure", "ice_failure"},
		{"disconnected", "ice_disconnected", "ice_disconnected"},
		{leg.PCPeerClosed, "ice_failure", "peer_closed"},
		{leg.PCDTLSFailed, "ice_dtls_failed", "dtls_failed"},
	}
	for _, c := range cases {
		if got := pcDisconnectReason(c.cause, c.iceReason); got != c.want {
			t.Errorf("pcDisconnectReason(%q, %q) = %q, want %q", c.cause, c.iceReason, got, c.want)
		}
	}
}

func newWhatsAppTestLeg(t *testing.T, s *Server) (*leg.WhatsAppLeg, <-chan string) {
	t.Helper()
	media, err := leg.NewPCMedia(leg.PCMediaConfig{Codec: codec.CodecOpus, Log: s.Log})
	if err != nil {
		t.Fatalf("NewPCMedia: %v", err)
	}
	l := leg.NewWhatsAppOutboundPendingLeg(media, "+15550001", "+15550002", s.Log)
	t.Cleanup(func() { media.Close() })
	s.LegMgr.Add(l)

	reasons := make(chan string, 4)
	s.Bus.Subscribe(func(e events.Event) {
		if d, ok := e.Data.(*events.LegDisconnectedData); ok && e.Type == events.LegDisconnected && d.LegID == l.ID() {
			reasons <- d.CDR.Reason
		}
	})
	return l, reasons
}

func TestWhatsAppMediaLost_PeerClosedTearsDownAfterGrace(t *testing.T) {
	s := newTestServer(t)
	l, reasons := newWhatsAppTestLeg(t, s)

	start := time.Now()
	s.whatsAppMediaLost(l, leg.PCPeerClosed)
	if waited := time.Since(start); waited < whatsAppByeGrace {
		t.Errorf("tore down after %v, before the %v BYE grace elapsed", waited, whatsAppByeGrace)
	}

	select {
	case got := <-reasons:
		if got != "peer_closed" {
			t.Errorf("cdr.reason = %q, want peer_closed", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no leg.disconnected published")
	}
	if _, ok := s.LegMgr.Get(l.ID()); ok {
		t.Error("leg still registered after its media path was lost")
	}
	if l.State() != leg.StateHungUp {
		t.Errorf("leg state = %s, want hung_up", l.State())
	}
}

// A BYE landing inside the grace window owns the teardown and its reason.
func TestWhatsAppMediaLost_PeerClosedYieldsToHangup(t *testing.T) {
	s := newTestServer(t)
	l, reasons := newWhatsAppTestLeg(t, s)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.whatsAppMediaLost(l, leg.PCPeerClosed)
	}()
	time.Sleep(50 * time.Millisecond)
	s.cleanupLeg(l)
	s.publishDisconnect(l, "remote_bye")

	select {
	case <-done:
	case <-time.After(whatsAppByeGrace / 2):
		t.Fatal("whatsAppMediaLost kept waiting after the leg hung up")
	}
	if got := <-reasons; got != "remote_bye" {
		t.Errorf("cdr.reason = %q, want remote_bye", got)
	}
	select {
	case got := <-reasons:
		t.Errorf("second leg.disconnected published with reason %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWhatsAppMediaLost_ICEFailureIsImmediate(t *testing.T) {
	s := newTestServer(t)
	l, reasons := newWhatsAppTestLeg(t, s)

	start := time.Now()
	s.whatsAppMediaLost(l, "failed")
	if waited := time.Since(start); waited >= whatsAppByeGrace {
		t.Errorf("ICE failure waited %v before tearing down", waited)
	}
	select {
	case got := <-reasons:
		if got != "ice_failed" {
			t.Errorf("cdr.reason = %q, want ice_failed", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no leg.disconnected published")
	}
}

func TestWhatsAppMediaLost_NilLeg(t *testing.T) {
	newTestServer(t).whatsAppMediaLost(nil, leg.PCPeerClosed)
}
