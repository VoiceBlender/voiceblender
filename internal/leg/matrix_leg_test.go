package leg

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/matrix"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// fakeSender is a no-op EventSender that records every call for assertions.
type fakeSender struct {
	mu         sync.Mutex
	answers    []*mevent.CallAnswerEventContent
	candidates []*mevent.CallCandidatesEventContent
	hangups    []*mevent.CallHangupEventContent
	subs       map[string]chan matrix.CallEvent
}

func newFakeSender() *fakeSender {
	return &fakeSender{subs: make(map[string]chan matrix.CallEvent)}
}

func (f *fakeSender) SendAnswer(_ context.Context, _ id.RoomID, c *mevent.CallAnswerEventContent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, c)
	return nil
}

func (f *fakeSender) SendCandidates(_ context.Context, _ id.RoomID, c *mevent.CallCandidatesEventContent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candidates = append(f.candidates, c)
	return nil
}

func (f *fakeSender) SendHangup(_ context.Context, _ id.RoomID, c *mevent.CallHangupEventContent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hangups = append(f.hangups, c)
	return nil
}

func (f *fakeSender) Subscribe(_ id.RoomID, callID string) <-chan matrix.CallEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.subs[callID]; ok {
		return ch
	}
	ch := make(chan matrix.CallEvent, 16)
	f.subs[callID] = ch
	return ch
}

func (f *fakeSender) Unsubscribe(_ id.RoomID, callID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.subs[callID]; ok {
		delete(f.subs, callID)
		close(ch)
	}
}

func TestMatrixLeg_IsLeg(t *testing.T) {
	var _ Leg = (*MatrixLeg)(nil)
}

func TestMatrixLeg_OutboundStartsRinging(t *testing.T) {
	m := newTestMedia(t)
	sender := newFakeSender()
	l := NewMatrixOutboundPendingLeg(MatrixLegConfig{
		Media:        m,
		Sender:       sender,
		MatrixRoomID: "!room:test",
		CallID:       "c1",
		PartyID:      "p1",
		Log:          slog.Default(),
	})
	defer l.Hangup(t.Context())

	if l.Type() != TypeMatrixOutbound {
		t.Errorf("Type = %v, want matrix_out", l.Type())
	}
	if l.State() != StateRinging {
		t.Errorf("State = %v, want ringing", l.State())
	}
	if l.SampleRate() != 48000 {
		t.Errorf("SampleRate = %d, want 48000", l.SampleRate())
	}
	if l.IsHeld() {
		t.Error("Matrix legs must not report IsHeld=true")
	}
	if l.CallID() != "c1" || l.PartyID() != "p1" {
		t.Errorf("CallID/PartyID mismatch: %q / %q", l.CallID(), l.PartyID())
	}
}

func TestMatrixLeg_InboundStartsRinging(t *testing.T) {
	m := newTestMedia(t)
	sender := newFakeSender()
	l := NewMatrixInboundLeg(MatrixLegConfig{
		Media:         m,
		Sender:        sender,
		MatrixRoomID:  "!room:test",
		CallID:        "c2",
		PartyID:       "p-us",
		RemotePartyID: "p-them",
		AnswerSDP:     "v=0\r\n",
		Log:           slog.Default(),
	})
	defer l.Hangup(t.Context())

	if l.Type() != TypeMatrixInbound {
		t.Errorf("Type = %v, want matrix_in", l.Type())
	}
	if l.State() != StateRinging {
		t.Errorf("State = %v, want ringing", l.State())
	}
	select {
	case <-l.AnswerCh():
		t.Fatal("AnswerCh should not be closed yet")
	default:
	}
}

func TestMatrixLeg_RequestAnswerRejectsOutbound(t *testing.T) {
	m := newTestMedia(t)
	l := NewMatrixOutboundPendingLeg(MatrixLegConfig{Media: m, Sender: newFakeSender(), MatrixRoomID: "!r:t", CallID: "c", Log: slog.Default()})
	defer l.Hangup(t.Context())

	if err := l.RequestAnswer(); err == nil {
		t.Fatal("expected RequestAnswer to fail for outbound leg")
	}
}

func TestMatrixLeg_RequestAnswerIdempotency(t *testing.T) {
	m := newTestMedia(t)
	l := NewMatrixInboundLeg(MatrixLegConfig{Media: m, Sender: newFakeSender(), MatrixRoomID: "!r:t", CallID: "c", AnswerSDP: "v=0\r\n", Log: slog.Default()})
	defer l.Hangup(t.Context())

	if err := l.RequestAnswer(); err != nil {
		t.Fatalf("first RequestAnswer: %v", err)
	}
	if err := l.RequestAnswer(); err == nil {
		t.Fatal("second RequestAnswer should return an error")
	}
	select {
	case <-l.AnswerCh():
	default:
		t.Fatal("answerCh should be closed after RequestAnswer")
	}
}

func TestMatrixLeg_HangupIsIdempotent(t *testing.T) {
	m := newTestMedia(t)
	sender := newFakeSender()
	l := NewMatrixOutboundPendingLeg(MatrixLegConfig{Media: m, Sender: sender, MatrixRoomID: "!r:t", CallID: "c1", PartyID: "p1", Log: slog.Default()})

	_ = l.Hangup(t.Context())
	_ = l.Hangup(t.Context())

	if l.State() != StateHungUp {
		t.Errorf("State = %v after Hangup, want hung_up", l.State())
	}
	sender.mu.Lock()
	hangups := len(sender.hangups)
	sender.mu.Unlock()
	if hangups != 1 {
		t.Errorf("SendHangup called %d times, want exactly 1 (idempotent)", hangups)
	}
}

func TestMatrixLeg_ClaimDisconnectOnce(t *testing.T) {
	m := newTestMedia(t)
	l := NewMatrixOutboundPendingLeg(MatrixLegConfig{Media: m, Sender: newFakeSender(), MatrixRoomID: "!r:t", CallID: "c", Log: slog.Default()})
	defer l.Hangup(t.Context())

	if !l.ClaimDisconnect() {
		t.Fatal("first ClaimDisconnect should return true")
	}
	if l.ClaimDisconnect() {
		t.Fatal("second ClaimDisconnect should return false")
	}
}

func TestMatrixLeg_RemoteHangupTriggersCallback(t *testing.T) {
	m := newTestMedia(t)
	sender := newFakeSender()
	l := NewMatrixInboundLeg(MatrixLegConfig{Media: m, Sender: sender, MatrixRoomID: "!r:t", CallID: "c", AnswerSDP: "v=0\r\n", Log: slog.Default()})
	defer l.Hangup(t.Context())

	fired := make(chan string, 1)
	l.SetOnRemoteHangup(func(reason string) { fired <- reason })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l.StartCandidatePump(ctx)

	// Inject a hangup event via the sender's subscribe channel.
	sub := sender.Subscribe("!r:t", "c") // same chan StartCandidatePump received
	_ = sub                              // ensure created
	sender.mu.Lock()
	ch := sender.subs["c"]
	sender.mu.Unlock()
	ch <- matrix.CallEvent{
		Kind:   matrix.KindHangup,
		RoomID: "!r:t",
		CallID: "c",
		Hangup: &mevent.CallHangupEventContent{Reason: mevent.CallHangupUserHangup},
	}

	select {
	case reason := <-fired:
		if reason != "user_hangup" {
			t.Errorf("reason = %q, want user_hangup", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRemoteHangup callback did not fire")
	}
}
