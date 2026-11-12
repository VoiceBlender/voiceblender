package leg

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

type holdRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *holdRecorder) add(e string) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *holdRecorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func newHoldTestLeg(t *testing.T) (*SIPLeg, *holdRecorder) {
	t.Helper()
	l := newTestSIPLeg(codec.CodecPCMU)
	l.state = StateConnected
	rec := &holdRecorder{}
	l.OnHold(func() { rec.add("hold") })
	l.OnUnhold(func() { rec.add("unhold") })
	t.Cleanup(func() { l.SetHeld(false) })
	return l, rec
}

// An offer with no direction attribute is sendrecv (RFC 4566 §6), so it
// takes a held call off hold.
func TestHandleRemoteHold_NoDirectionIsSendRecv(t *testing.T) {
	l, rec := newHoldTestLeg(t)
	l.HandleRemoteHold(sipmod.DirSendOnly)
	l.HandleRemoteHold("")
	if l.IsHeld() {
		t.Fatal("leg still held after an offer without a direction attribute")
	}
	if got := rec.get(); len(got) != 2 || got[0] != "hold" || got[1] != "unhold" {
		t.Errorf("callbacks = %v, want [hold unhold]", got)
	}
}

// Re-offers that do not change the hold state (a codec change, a refresh)
// fire no callback.
func TestHandleRemoteHold_CallbacksOnlyOnChange(t *testing.T) {
	l, rec := newHoldTestLeg(t)
	l.HandleRemoteHold(sipmod.DirSendRecv)
	l.HandleRemoteHold("")
	l.HandleRemoteHold(sipmod.DirSendOnly)
	l.HandleRemoteHold(sipmod.DirInactive)
	if got := rec.get(); len(got) != 1 || got[0] != "hold" {
		t.Errorf("callbacks = %v, want [hold]", got)
	}
}

// An unhold requested while the hold re-INVITE is still waiting for its
// answer is applied after it, not dropped.
func TestSetHoldAsync_UnholdDuringPendingHold(t *testing.T) {
	l, rec := newHoldTestLeg(t)
	release := make(chan struct{})
	var mu sync.Mutex
	var sent []bool
	l.holdReInvite = func(ctx context.Context, hold bool) error {
		mu.Lock()
		sent = append(sent, hold)
		first := len(sent) == 1
		mu.Unlock()
		if first {
			<-release
		}
		return nil
	}

	errs := make(chan error, 2)
	l.SetHoldAsync(true, func(err error) { errs <- err })
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	l.SetHoldAsync(false, func(err error) { errs <- err })
	close(release)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := rec.get(); len(got) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-errs:
		t.Fatalf("hold request failed: %v", err)
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || !sent[0] || sent[1] {
		t.Errorf("re-INVITEs sent = %v, want [hold unhold]", sent)
	}
	if got := rec.get(); len(got) != 2 || got[0] != "hold" || got[1] != "unhold" {
		t.Errorf("callbacks = %v, want [hold unhold]", got)
	}
	if l.IsHeld() {
		t.Error("leg held although the last request was unhold")
	}
}

// Requests issued in order end in the last one's state, whatever order the
// background goroutines run in.
func TestSetHoldAsync_LastRequestWins(t *testing.T) {
	l, _ := newHoldTestLeg(t)
	l.holdReInvite = func(context.Context, bool) error { return nil }
	for i := 0; i < 20; i++ {
		l.SetHoldAsync(i%2 == 0, nil)
	}
	l.SetHoldAsync(true, nil)
	deadline := time.Now().Add(2 * time.Second)
	for !l.IsHeld() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if !l.IsHeld() {
		t.Error("leg not held after the final hold request")
	}
}

func TestHold_NotConnected(t *testing.T) {
	l, _ := newHoldTestLeg(t)
	l.state = StateRinging
	l.holdReInvite = func(context.Context, bool) error { t.Fatal("re-INVITE sent for a ringing leg"); return nil }
	if err := l.Hold(context.Background()); err == nil {
		t.Error("hold of a ringing leg should fail")
	}
}
