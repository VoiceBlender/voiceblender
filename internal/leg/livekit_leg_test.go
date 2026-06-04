package leg

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func lkTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newBareLiveKitLeg builds a LiveKitLeg with no transport, suitable for
// exercising state/metadata behavior without spinning up signaling.
func newBareLiveKitLeg(t *testing.T) *LiveKitLeg {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	return &LiveKitLeg{
		id:         "lk-test",
		legType:    TypeLiveKitRoom,
		state:      StateConnected,
		sampleRate: 48000,
		createdAt:  time.Now(),
		answeredAt: time.Now(),
		ctx:        ctx,
		cancel:     cancel,
		log:        lkTestLog(),
	}
}

func TestLiveKitLeg_Identity(t *testing.T) {
	l := newBareLiveKitLeg(t)
	if l.Type() != TypeLiveKitRoom {
		t.Errorf("Type = %s, want %s", l.Type(), TypeLiveKitRoom)
	}
	if l.SampleRate() != 48000 {
		t.Errorf("SampleRate = %d, want 48000", l.SampleRate())
	}
	if l.RTTNegotiated() {
		t.Error("RTTNegotiated should be false for LiveKit leg")
	}
	if l.IsHeld() {
		t.Error("IsHeld should be false")
	}
	if l.SIPHeaders() != nil {
		t.Error("SIPHeaders should be nil for LiveKit leg")
	}
}

func TestLiveKitLeg_MuteDeafState(t *testing.T) {
	l := newBareLiveKitLeg(t)
	if l.IsMuted() {
		t.Error("default IsMuted should be false")
	}
	l.SetMuted(true)
	if !l.IsMuted() {
		t.Error("SetMuted(true) didn't take effect")
	}
	l.SetMuted(false)
	if l.IsMuted() {
		t.Error("SetMuted(false) didn't take effect")
	}

	if l.IsDeaf() {
		t.Error("default IsDeaf should be false")
	}
	l.SetDeaf(true)
	if !l.IsDeaf() {
		t.Error("SetDeaf(true) didn't take effect")
	}
}

func TestLiveKitLeg_RoomAppRoleMetadata(t *testing.T) {
	l := newBareLiveKitLeg(t)
	l.SetRoomID("room-7")
	l.SetAppID("app-9")
	l.SetRole("agent")
	if l.RoomID() != "room-7" || l.AppID() != "app-9" || l.Role() != "agent" {
		t.Errorf("metadata round-trip: room=%q app=%q role=%q",
			l.RoomID(), l.AppID(), l.Role())
	}
}

func TestLiveKitLeg_HangupTransitions(t *testing.T) {
	l := newBareLiveKitLeg(t)
	if l.State() != StateConnected {
		t.Fatalf("expected StateConnected, got %s", l.State())
	}
	if err := l.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	if l.State() != StateHungUp {
		t.Errorf("after Hangup State = %s, want %s", l.State(), StateHungUp)
	}
	// Idempotent.
	if err := l.Hangup(context.Background()); err != nil {
		t.Fatalf("second Hangup: %v", err)
	}
	// Context is cancelled.
	select {
	case <-l.Context().Done():
	case <-time.After(100 * time.Millisecond):
		t.Error("Context should be done after Hangup")
	}
}

func TestLiveKitLeg_ClaimDisconnectSingleFlight(t *testing.T) {
	l := newBareLiveKitLeg(t)
	if !l.ClaimDisconnect() {
		t.Fatal("first ClaimDisconnect should return true")
	}
	if l.ClaimDisconnect() {
		t.Fatal("second ClaimDisconnect should return false")
	}
}

func TestLiveKitLeg_DTMFAndTextUnsupported(t *testing.T) {
	l := newBareLiveKitLeg(t)
	if err := l.SendDTMF(context.Background(), "1234"); err == nil {
		t.Error("SendDTMF should return an error")
	}
	if err := l.SendText(context.Background(), "hello"); err != ErrRTTNotNegotiated {
		t.Errorf("SendText err = %v, want ErrRTTNotNegotiated", err)
	}
}

func TestLiveKitLeg_AudioReaderNilTransport(t *testing.T) {
	l := newBareLiveKitLeg(t)
	r := l.AudioReader()
	if r == nil {
		t.Fatal("AudioReader returned nil")
	}
	buf := make([]byte, 4)
	if _, err := r.Read(buf); err != io.EOF {
		t.Errorf("expected EOF from nil-transport reader, got %v", err)
	}
}

func TestLiveKitLeg_AudioWriterNilTransportDiscards(t *testing.T) {
	l := newBareLiveKitLeg(t)
	w := l.AudioWriter()
	if w == nil {
		t.Fatal("AudioWriter returned nil")
	}
	n, err := w.Write([]byte{1, 2, 3})
	if err != nil || n != 3 {
		t.Errorf("Write to nil-transport writer = (%d, %v), want (3, nil)", n, err)
	}
}

func TestLiveKitLeg_Headers_MergesAndDefensiveCopy(t *testing.T) {
	in := map[string]string{
		"livekit_name": "Alice",
		"x_custom":     "v",
	}
	// Construct via NewLiveKitLeg with nil transport (skips transport-derived
	// header merge but still honors caller-supplied headers).
	l := NewLiveKitLeg(nil, in, 48000, lkTestLog())
	out := l.Headers()
	if got := out["livekit_name"]; got != "Alice" {
		t.Errorf("livekit_name = %q, want Alice", got)
	}
	if got := out["x_custom"]; got != "v" {
		t.Errorf("x_custom = %q, want v", got)
	}
	// Mutating the returned map must not affect the leg.
	out["x_custom"] = "MUTATED"
	if got := l.Headers()["x_custom"]; got != "v" {
		t.Errorf("internal headers mutated through returned map: %q", got)
	}
}

func TestLiveKitLeg_AcceptDTMFAndText(t *testing.T) {
	l := newBareLiveKitLeg(t)
	if l.AcceptDTMF() {
		t.Error("default AcceptDTMF should be false")
	}
	l.SetAcceptDTMF(true)
	if !l.AcceptDTMF() {
		t.Error("SetAcceptDTMF(true) didn't take effect")
	}
	l.SetAcceptText(true)
	if !l.AcceptText() {
		t.Error("SetAcceptText(true) didn't take effect")
	}
}

func TestLiveKitLeg_RTPStatsEmpty(t *testing.T) {
	l := newBareLiveKitLeg(t)
	if got := l.RTPStats(); (got != RTPStats{}) {
		t.Errorf("RTPStats = %+v, want zero", got)
	}
}

// Sanity check that the atomic.Bool zero value behaves as expected.
func TestLiveKitLeg_AtomicBoolDefaults(t *testing.T) {
	var b atomic.Bool
	if b.Load() {
		t.Fatal("atomic.Bool zero value should be false")
	}
}
