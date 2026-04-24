package leg

import (
	"log/slog"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func newTestMedia(t *testing.T) *PCMedia {
	t.Helper()
	m, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecOpus, Log: slog.Default()})
	if err != nil {
		t.Fatalf("NewPCMedia: %v", err)
	}
	return m
}

func TestWhatsAppLeg_OutboundStartsConnected(t *testing.T) {
	m := newTestMedia(t)
	l := NewWhatsAppOutboundLeg(nil, m, "15551234567", "15557654321", slog.Default())
	defer l.Hangup(t.Context())

	if l.Type() != TypeWhatsApp {
		t.Errorf("Type = %v, want whatsapp", l.Type())
	}
	if l.State() != StateConnected {
		t.Errorf("State = %v, want connected", l.State())
	}
	if l.SampleRate() != 48000 {
		t.Errorf("SampleRate = %d, want 48000", l.SampleRate())
	}
	if l.From() != "15551234567" || l.To() != "15557654321" {
		t.Errorf("From/To mismatch: %q / %q", l.From(), l.To())
	}
	if l.IsHeld() {
		t.Error("WhatsApp legs must not report IsHeld=true")
	}
}

func TestWhatsAppLeg_InboundStartsRinging(t *testing.T) {
	m := newTestMedia(t)
	l := NewWhatsAppInboundLeg(nil, m, "15557654321", "15551234567", map[string]string{"X-App-ID": "myapp"}, []byte("v=0\r\n"), slog.Default())
	defer l.Hangup(t.Context())

	if l.State() != StateRinging {
		t.Errorf("State = %v, want ringing", l.State())
	}
	if got := l.SIPHeaders()["X-App-ID"]; got != "myapp" {
		t.Errorf("X-App-ID header = %q, want myapp", got)
	}
}

func TestWhatsAppLeg_RequestAnswerRejectsOutbound(t *testing.T) {
	m := newTestMedia(t)
	l := NewWhatsAppOutboundLeg(nil, m, "from", "to", slog.Default())
	defer l.Hangup(t.Context())

	if err := l.RequestAnswer(); err == nil {
		t.Fatal("expected RequestAnswer to fail for outbound leg")
	}
}

func TestWhatsAppLeg_RequestAnswerIdempotency(t *testing.T) {
	m := newTestMedia(t)
	l := NewWhatsAppInboundLeg(nil, m, "from", "to", nil, nil, slog.Default())
	defer l.Hangup(t.Context())

	if err := l.RequestAnswer(); err != nil {
		t.Fatalf("first RequestAnswer: %v", err)
	}
	if err := l.RequestAnswer(); err == nil {
		t.Fatal("second RequestAnswer should return an error")
	}
	// answerCh must be closed so HandleInboundCall unblocks.
	select {
	case <-l.AnswerCh():
	default:
		t.Fatal("answerCh should be closed after RequestAnswer")
	}
}

func TestWhatsAppLeg_HangupIsIdempotent(t *testing.T) {
	m := newTestMedia(t)
	l := NewWhatsAppOutboundLeg(nil, m, "from", "to", slog.Default())

	if err := l.Hangup(t.Context()); err != nil {
		// media.Close can return the pion PeerConnection close error on
		// double-close; ignore.
		_ = err
	}
	if err := l.Hangup(t.Context()); err != nil {
		_ = err
	}
	if l.State() != StateHungUp {
		t.Errorf("State = %v after Hangup, want hung_up", l.State())
	}
}

func TestWhatsAppLeg_IsLeg(t *testing.T) {
	// Compile-time check that WhatsAppLeg satisfies the Leg interface.
	var _ Leg = (*WhatsAppLeg)(nil)
}
