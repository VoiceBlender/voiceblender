package api

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func newAuthTestServer(timeoutMs int) *Server {
	return &Server{
		Bus:         events.NewBus("test"),
		Config:      config.Config{SIPInboundAuthConsultTimeoutMs: timeoutMs},
		regAttempts: newRegisterAttemptStore(),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func sampleAttempt() *sipmod.RegisterAttempt {
	return &sipmod.RegisterAttempt{AOR: "sip:alice@vb.example", Contact: "sip:alice@1.2.3.4", Source: "1.2.3.4:5060", Transport: "udp", CallID: "call-1"}
}

// Every REGISTER is consulted (symmetric with the always-surfaced inbound
// INVITE); when no decision arrives within the consult window it auto-accepts.
func TestHandleRegisterAttempt_TimeoutAccepts(t *testing.T) {
	s := newAuthTestServer(50)
	start := time.Now()
	d := s.HandleRegisterAttempt(sampleAttempt())
	if d.Kind != sipmod.RegisterAccept {
		t.Fatalf("kind = %v, want RegisterAccept on timeout", d.Kind)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Error("returned before the consult timeout elapsed")
	}
}

// HandleRegisterAttempt always publishes the attempt event so a client can act,
// regardless of how it is subscribed.
func TestHandleRegisterAttempt_AlwaysPublishesEvent(t *testing.T) {
	s := newAuthTestServer(50)
	var published bool
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type == events.SIPRegistrationAttempt {
			published = true
		}
	})
	s.HandleRegisterAttempt(sampleAttempt())
	if !published {
		t.Error("sip.registration_attempt was not published")
	}
}

func TestHandleRegisterAttempt_ChallengeDecision(t *testing.T) {
	s := newAuthTestServer(5000)

	// Capture the attempt_id from the published event and challenge it.
	gotID := make(chan string, 1)
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type == events.SIPRegistrationAttempt {
			gotID <- e.Data.(*events.SIPRegistrationAttemptData).AttemptID
		}
	})

	type res struct{ d sipmod.RegisterDecision }
	done := make(chan res, 1)
	go func() { done <- res{s.HandleRegisterAttempt(sampleAttempt())} }()

	select {
	case id := <-gotID:
		if err := s.doChallengeRegistration(id, ChallengeRequest{Realm: "vb", Password: "pw", MaxExpires: 30}); err != nil {
			t.Fatalf("doChallengeRegistration: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("no registration_attempt event published")
	}

	select {
	case r := <-done:
		if r.d.Kind != sipmod.RegisterChallenge {
			t.Fatalf("kind = %v, want RegisterChallenge", r.d.Kind)
		}
		if r.d.Challenge.Realm != "vb" || r.d.Challenge.Password != "pw" {
			t.Errorf("challenge params not propagated: %+v", r.d.Challenge)
		}
		if r.d.MaxExpires != 30 {
			t.Errorf("MaxExpires = %d, want 30", r.d.MaxExpires)
		}
	case <-time.After(time.Second):
		t.Fatal("HandleRegisterAttempt did not return after decision")
	}
}

func TestHandleRegisterAttempt_AcceptCarriesMaxExpires(t *testing.T) {
	s := newAuthTestServer(5000)

	gotID := make(chan string, 1)
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type == events.SIPRegistrationAttempt {
			gotID <- e.Data.(*events.SIPRegistrationAttemptData).AttemptID
		}
	})

	done := make(chan sipmod.RegisterDecision, 1)
	go func() { done <- s.HandleRegisterAttempt(sampleAttempt()) }()

	select {
	case id := <-gotID:
		if err := s.doAcceptRegistration(id, RegistrationAcceptRequest{MaxExpires: 45}); err != nil {
			t.Fatalf("doAcceptRegistration: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("no registration_attempt event published")
	}

	select {
	case d := <-done:
		if d.Kind != sipmod.RegisterAccept {
			t.Fatalf("kind = %v, want RegisterAccept", d.Kind)
		}
		if d.MaxExpires != 45 {
			t.Errorf("MaxExpires = %d, want 45", d.MaxExpires)
		}
	case <-time.After(time.Second):
		t.Fatal("HandleRegisterAttempt did not return after decision")
	}
}

func TestDecideRegisterAttempt_NotFound(t *testing.T) {
	s := newAuthTestServer(100)
	if err := s.doAcceptRegistration("nope", RegistrationAcceptRequest{}); err == nil {
		t.Fatal("expected error for unknown attempt id")
	}
}

func TestChallengeRequest_NegativeMaxExpires(t *testing.T) {
	err := ChallengeRequest{Realm: "vb", Password: "pw", MaxExpires: -1}.validate()
	if err == nil {
		t.Fatal("expected error for negative max_expires")
	}
}

func TestChallengeRequest_Validate(t *testing.T) {
	cases := []struct {
		name string
		req  ChallengeRequest
		ok   bool
	}{
		{"missing realm", ChallengeRequest{Password: "pw"}, false},
		{"missing credential", ChallengeRequest{Realm: "vb"}, false},
		{"password ok", ChallengeRequest{Realm: "vb", Password: "pw"}, true},
		{"ha1 ok", ChallengeRequest{Realm: "vb", HA1: "deadbeef"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.req.validate()
			if c.ok && err != nil {
				t.Errorf("validate() = %v, want nil", err)
			}
			if !c.ok && err == nil {
				t.Error("validate() = nil, want error")
			}
		})
	}
}
