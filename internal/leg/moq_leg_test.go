package leg

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func moqTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newBareMoQLeg builds an MoQLeg with no transport attached, suitable for
// exercising state/metadata behavior without spinning up WebTransport.
func newBareMoQLeg(t *testing.T) *MoQLeg {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	return &MoQLeg{
		id:         "moq-test",
		legType:    TypeMoQInbound,
		state:      StateConnected,
		sampleRate: 48000,
		egressPR:   pr,
		egressPW:   pw,
		createdAt:  time.Now(),
		answeredAt: time.Now(),
		ctx:        ctx,
		cancel:     cancel,
		log:        moqTestLog(),
	}
}

func TestMoQLeg_Identity(t *testing.T) {
	l := newBareMoQLeg(t)
	if l.Type() != TypeMoQInbound {
		t.Errorf("Type = %s, want %s", l.Type(), TypeMoQInbound)
	}
	if l.SampleRate() != 48000 {
		t.Errorf("SampleRate = %d, want 48000", l.SampleRate())
	}
	if l.RTTNegotiated() {
		t.Error("RTTNegotiated should be false for MoQ leg")
	}
	if l.IsHeld() {
		t.Error("IsHeld should be false")
	}
	if l.SIPHeaders() != nil {
		t.Error("SIPHeaders should be nil")
	}
}

func TestMoQLeg_MuteDeafState(t *testing.T) {
	l := newBareMoQLeg(t)
	if l.IsMuted() {
		t.Error("default IsMuted should be false")
	}
	l.SetMuted(true)
	if !l.IsMuted() {
		t.Error("SetMuted(true) didn't take effect")
	}
	l.SetDeaf(true)
	if !l.IsDeaf() {
		t.Error("SetDeaf(true) didn't take effect")
	}
}

func TestMoQLeg_HangupTransitions(t *testing.T) {
	l := newBareMoQLeg(t)
	if l.State() != StateConnected {
		t.Fatalf("expected StateConnected, got %s", l.State())
	}
	if err := l.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	if l.State() != StateHungUp {
		t.Errorf("after Hangup State = %s, want %s", l.State(), StateHungUp)
	}
	// Second hangup is a no-op.
	if err := l.Hangup(context.Background()); err != nil {
		t.Fatalf("second Hangup: %v", err)
	}
}

func TestMoQLeg_ClaimDisconnectSingleFlight(t *testing.T) {
	l := newBareMoQLeg(t)
	if !l.ClaimDisconnect() {
		t.Fatal("first ClaimDisconnect should return true")
	}
	if l.ClaimDisconnect() {
		t.Fatal("second ClaimDisconnect should return false")
	}
}

func TestMoQLeg_DTMFAndTextUnsupported(t *testing.T) {
	l := newBareMoQLeg(t)
	if err := l.SendDTMF(context.Background(), "1234"); err == nil {
		t.Error("SendDTMF should return an error")
	}
	if err := l.SendText(context.Background(), "hello"); err != ErrRTTNotNegotiated {
		t.Errorf("SendText err = %v, want ErrRTTNotNegotiated", err)
	}
}

func TestMoQLeg_AudioReaderNilTransport(t *testing.T) {
	l := newBareMoQLeg(t)
	r := l.AudioReader()
	if r == nil {
		t.Fatal("AudioReader returned nil")
	}
	buf := make([]byte, 4)
	if _, err := r.Read(buf); err != io.EOF {
		t.Errorf("expected EOF from nil-transport reader, got %v", err)
	}
}

func TestMoQLeg_RoomAndAppID(t *testing.T) {
	l := newBareMoQLeg(t)
	l.SetRoomID("room-1")
	l.SetAppID("app-1")
	if l.RoomID() != "room-1" {
		t.Errorf("RoomID = %q, want room-1", l.RoomID())
	}
	if l.AppID() != "app-1" {
		t.Errorf("AppID = %q, want app-1", l.AppID())
	}
}

// Sanity check that the atomic.Bool zero value behaves as expected
// (used by mute/deaf/acceptDTMF/acceptText).
func TestMoQLeg_AtomicBoolDefaults(t *testing.T) {
	var b atomic.Bool
	if b.Load() {
		t.Fatal("atomic.Bool zero value should be false")
	}
}
