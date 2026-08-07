package api

import (
	"io"
	"log/slog"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
)

// referIdentityTestServer builds a server whose engine carries one registered
// trunk "t1" with AOR sip:+15551234567@pbx.example.com.
//
// The bind port is deliberately not 5060: referTestEngine already claims that
// port for the dialog-watch tests in this package, and a second bind would
// fail.
func referIdentityTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	eng, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: 15062,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	s.SIPEngine = eng

	reg := sipmod.NewOutboundRegistration(nil, nil, nil, sipmod.OutboundRegistrationConfig{}, sipmod.OutboundRegistrationParams{
		ID:           "t1",
		RegistrarURI: sip.Uri{Scheme: "sip", Host: "sbc.carrier.net", Port: 5060},
		AOR:          sip.Uri{Scheme: "sip", User: "+15551234567", Host: "pbx.example.com"},
		Password:     "x",
	})
	eng.Trunks().Add(reg)
	return s
}

func newReferrerLeg(t *testing.T, s *Server, trunkID, user, host string) *leg.SIPLeg {
	t.Helper()
	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Log)
	l.SetTrunkID(trunkID)
	l.SetOriginatingIdentity(user, host)
	return l
}

// TestReferIdentity_InheritsTrunk pins the core of the REFER identity rule: a
// leg associated with a trunk transfers out under that trunk's AOR, NOT under
// the transferor's own caller ID.
//
// The leg's own originating identity is deliberately set to a different URI, so
// an implementation that reached for it instead of the trunk cannot pass.
func TestReferIdentity_InheritsTrunk(t *testing.T) {
	s := referIdentityTestServer(t)

	l := newReferrerLeg(t, s, "t1", "+15559999999", "pbx.example.com")
	const want = "sip:+15551234567@pbx.example.com"
	if got := s.referIdentity(l); got != want {
		t.Errorf("referIdentity() = %q, want the trunk AOR %q", got, want)
	}
}

// TestReferIdentity_UnknownTrunkFallsBack covers a trunk deleted between the
// call arriving and the REFER: the ID is stale, Get misses, and the leg falls
// back to its own identity rather than originating with no From at all.
func TestReferIdentity_UnknownTrunkFallsBack(t *testing.T) {
	s := referIdentityTestServer(t)

	l := newReferrerLeg(t, s, "gone", "alice", "vb.test")
	const want = "sip:alice@vb.test"
	if got := s.referIdentity(l); got != want {
		t.Errorf("referIdentity() = %q, want the leg identity %q", got, want)
	}
}

func TestReferIdentity_NoTrunk(t *testing.T) {
	s := referIdentityTestServer(t)

	cases := []struct {
		name string
		user string
		host string
		want string
	}{
		{"full identity", "alice", "vb.test", "sip:alice@vb.test"},
		// A leg dialled with `from: "alice"` and no trunk match carries no
		// host; the bare user re-enters applyFromIdentity as a user-part.
		{"user only", "alice", "", "alice"},
		// Nothing to claim — the From is left to sipgo, as before.
		{"no identity", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newReferrerLeg(t, s, "", tc.user, tc.host)
			if got := s.referIdentity(l); got != tc.want {
				t.Errorf("referIdentity() = %q, want %q", got, tc.want)
			}
		})
	}
}
