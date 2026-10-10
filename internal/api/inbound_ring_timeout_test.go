package api

import (
	"context"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func newUnansweredInboundCall() *sipmod.InboundCall {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "test", Host: "127.0.0.1"})
	ds := &sipgo.DialogServerSession{Dialog: sipgo.Dialog{InviteRequest: req}}
	ds.Init()
	return &sipmod.InboundCall{Dialog: ds, Request: req}
}

// awaitResult runs awaitInboundAnswer in the background so a test can bound
// how long it is allowed to block.
func awaitResult(s *Server, l leg.Leg, answerCh <-chan struct{}) <-chan bool {
	out := make(chan bool, 1)
	go func() { out <- s.awaitInboundAnswer(l, newUnansweredInboundCall(), answerCh) }()
	return out
}

func TestAwaitInboundAnswer_Answered(t *testing.T) {
	s := newTestServer(t)
	s.Config.SIPInboundRingTimeoutSeconds = 30
	reasons := disconnectReasons(s)
	l := addPendingLeg(t, s, "")

	answerCh := make(chan struct{})
	res := awaitResult(s, l, answerCh)
	close(answerCh)

	select {
	case answered := <-res:
		if !answered {
			t.Fatal("awaitInboundAnswer = false, want true once answered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitInboundAnswer did not return after answer")
	}
	if l.State() == leg.StateHungUp {
		t.Error("answered leg was torn down")
	}
	if got := reasons(l.ID()); len(got) != 0 {
		t.Errorf("leg.disconnected reasons = %v, want none", got)
	}
}

func TestAwaitInboundAnswer_RingTimeout(t *testing.T) {
	s := newTestServer(t)
	s.Config.SIPInboundRingTimeoutSeconds = 1
	reasons := disconnectReasons(s)
	l := addPendingLeg(t, s, "")

	select {
	case answered := <-awaitResult(s, l, make(chan struct{})):
		if answered {
			t.Fatal("awaitInboundAnswer = true, want false on ring timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitInboundAnswer did not return after the ring timeout")
	}
	if l.State() != leg.StateHungUp {
		t.Errorf("leg state = %s, want hung_up", l.State())
	}
	if _, ok := s.LegMgr.Get(l.ID()); ok {
		t.Error("timed-out leg still registered")
	}
	if got := reasons(l.ID()); len(got) != 1 || got[0] != "ring_timeout" {
		t.Errorf("leg.disconnected reasons = %v, want [ring_timeout]", got)
	}
}

func TestAwaitInboundAnswer_ZeroIsUnbounded(t *testing.T) {
	s := newTestServer(t)
	s.Config.SIPInboundRingTimeoutSeconds = 0
	l := addPendingLeg(t, s, "")

	answerCh := make(chan struct{})
	res := awaitResult(s, l, answerCh)
	select {
	case <-res:
		t.Fatal("awaitInboundAnswer returned with no timeout configured")
	case <-time.After(1500 * time.Millisecond):
	}
	if l.State() == leg.StateHungUp {
		t.Error("leg torn down with no timeout configured")
	}

	close(answerCh)
	<-res
}

// A hangup that sends no final response must still release the wait, and must
// not publish a second leg.disconnected on top of the API path's.
func TestAwaitInboundAnswer_LegHungUp(t *testing.T) {
	s := newTestServer(t)
	s.Config.SIPInboundRingTimeoutSeconds = 30
	reasons := disconnectReasons(s)
	l := addPendingLeg(t, s, "")

	res := awaitResult(s, l, make(chan struct{}))
	if err := l.Hangup(context.Background()); err != nil {
		t.Fatalf("hangup: %v", err)
	}

	select {
	case answered := <-res:
		if answered {
			t.Fatal("awaitInboundAnswer = true, want false after hangup")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitInboundAnswer did not return after hangup")
	}
	if got := reasons(l.ID()); len(got) != 0 {
		t.Errorf("leg.disconnected reasons = %v, want none from the wait", got)
	}
}
