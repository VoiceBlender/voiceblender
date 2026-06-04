package leg

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func lkpTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLiveKitParticipantLeg_Identity(t *testing.T) {
	l := NewLiveKitParticipantLeg("alice", "TR_audio_1", nil, 48000, lkpTestLog())
	if l.Type() != TypeLiveKitParticipant {
		t.Errorf("Type = %s, want %s", l.Type(), TypeLiveKitParticipant)
	}
	if l.SampleRate() != 48000 {
		t.Errorf("SampleRate = %d, want 48000", l.SampleRate())
	}
	if l.Identity() != "alice" {
		t.Errorf("Identity = %q, want alice", l.Identity())
	}
	if l.TrackSID() != "TR_audio_1" {
		t.Errorf("TrackSID = %q, want TR_audio_1", l.TrackSID())
	}
	if l.RTTNegotiated() {
		t.Error("RTTNegotiated should be false")
	}
	if l.IsHeld() {
		t.Error("IsHeld should be false")
	}
	if l.SIPHeaders() != nil {
		t.Error("SIPHeaders should be nil")
	}
}

func TestLiveKitParticipantLeg_Headers(t *testing.T) {
	l := NewLiveKitParticipantLeg("alice", "TR_audio_1", nil, 48000, lkpTestLog())
	h := l.Headers()
	if h["livekit_identity"] != "alice" {
		t.Errorf("livekit_identity = %q, want alice", h["livekit_identity"])
	}
	if h["livekit_track_sid"] != "TR_audio_1" {
		t.Errorf("livekit_track_sid = %q, want TR_audio_1", h["livekit_track_sid"])
	}
	// Defensive copy — mutating return must not affect leg state.
	h["livekit_identity"] = "MUTATED"
	if l.Headers()["livekit_identity"] != "alice" {
		t.Error("internal headers mutated through returned map")
	}
}

func TestLiveKitParticipantLeg_HeadersEmptyWhenNothingProvided(t *testing.T) {
	l := NewLiveKitParticipantLeg("", "", nil, 48000, lkpTestLog())
	if l.Headers() != nil {
		t.Errorf("Headers = %+v, want nil", l.Headers())
	}
}

func TestLiveKitParticipantLeg_MuteDeafState(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
	if l.IsMuted() {
		t.Error("default IsMuted should be false")
	}
	l.SetMuted(true)
	if !l.IsMuted() {
		t.Error("SetMuted(true) didn't take effect")
	}
	if l.IsDeaf() {
		t.Error("default IsDeaf should be false")
	}
	l.SetDeaf(true)
	if !l.IsDeaf() {
		t.Error("SetDeaf(true) didn't take effect")
	}
}

func TestLiveKitParticipantLeg_RoomAppRoleMetadata(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
	l.SetRoomID("vb-r-1")
	l.SetAppID("app-x")
	l.SetRole("livekit_listen")
	if l.RoomID() != "vb-r-1" || l.AppID() != "app-x" || l.Role() != "livekit_listen" {
		t.Errorf("metadata: room=%q app=%q role=%q", l.RoomID(), l.AppID(), l.Role())
	}
}

func TestLiveKitParticipantLeg_HangupTransitions(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
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
	// Context cancelled.
	select {
	case <-l.Context().Done():
	case <-time.After(100 * time.Millisecond):
		t.Error("Context should be done after Hangup")
	}
}

func TestLiveKitParticipantLeg_ClaimDisconnectSingleFlight(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
	if !l.ClaimDisconnect() {
		t.Fatal("first ClaimDisconnect should return true")
	}
	if l.ClaimDisconnect() {
		t.Fatal("second ClaimDisconnect should return false")
	}
}

func TestLiveKitParticipantLeg_DTMFAndTextUnsupported(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
	if err := l.SendDTMF(context.Background(), "1234"); err == nil {
		t.Error("SendDTMF should return an error")
	}
	if !strings.Contains(strErr(l.SendText(context.Background(), "hi")), "negotiated") {
		t.Errorf("SendText should return ErrRTTNotNegotiated, got %v",
			l.SendText(context.Background(), "hi"))
	}
}

func strErr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func TestLiveKitParticipantLeg_AudioReaderReturnsProvidedPCM(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	l := NewLiveKitParticipantLeg("a", "t", bytes.NewReader(pcm), 48000, lkpTestLog())
	r := l.AudioReader()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, pcm) {
		t.Errorf("got %v, want %v", got, pcm)
	}
}

func TestLiveKitParticipantLeg_AudioReaderNilSourceEmpty(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
	r := l.AudioReader()
	buf := make([]byte, 4)
	if _, err := r.Read(buf); err != io.EOF {
		t.Errorf("expected EOF from nil-source reader, got %v", err)
	}
}

func TestLiveKitParticipantLeg_AudioWriterIsDiscard(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
	w := l.AudioWriter()
	n, err := w.Write([]byte("anything"))
	if err != nil || n != len("anything") {
		t.Errorf("Write = (%d, %v), want (%d, nil)", n, err, len("anything"))
	}
}

func TestLiveKitParticipantLeg_AcceptDTMFAndText(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
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

func TestLiveKitParticipantLeg_RTPStatsEmpty(t *testing.T) {
	l := NewLiveKitParticipantLeg("a", "t", nil, 48000, lkpTestLog())
	if got := l.RTPStats(); (got != RTPStats{}) {
		t.Errorf("RTPStats = %+v, want zero", got)
	}
}

func TestLiveKitParticipantLeg_AtomicBoolDefaults(t *testing.T) {
	var b atomic.Bool
	if b.Load() {
		t.Fatal("atomic.Bool zero value should be false")
	}
}
