package leg

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// The negotiated refresher names a role in the initial INVITE, so which side
// refreshes depends on the leg's direction (RFC 4028 §7.2 and §9).
func TestRefreshesSession_FollowsLegRole(t *testing.T) {
	cases := []struct {
		name      string
		inbound   bool
		refresher string
		want      bool
	}{
		{"inbound leg, refresher=uas", true, "uas", true},
		{"inbound leg, refresher=uac", true, "uac", false},
		{"outbound leg, refresher=uac", false, "uac", true},
		{"outbound leg, refresher=uas", false, "uas", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestSIPLeg(codec.CodecPCMU)
			if tc.inbound {
				l.inbound = &sipmod.InboundCall{}
			} else {
				l.outbound = &sipmod.OutboundCall{}
			}
			l.sessionRefresher = tc.refresher

			if got := l.refreshesSessionLocked(); got != tc.want {
				t.Errorf("refreshesSessionLocked() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A re-INVITE or UPDATE from the far end can name a new refresher or interval.
// We are the UAS of that request, whatever the leg's direction.
func TestResetSessionTimer_FollowsFarEndRequest(t *testing.T) {
	cases := []struct {
		name          string
		inbound       bool
		state         LegState
		interval      uint32
		refresher     string // from the initial INVITE
		request       *sipmod.SessionTimerParams
		wantInterval  uint32
		wantRefreshes bool
		wantTimer     bool
	}{
		{"outbound leg takes the role", false, StateConnected, 1800, "uas",
			&sipmod.SessionTimerParams{Interval: 1800, Refresher: "uas"}, 1800, true, true},
		{"outbound leg gives up the role", false, StateConnected, 1800, "uac",
			&sipmod.SessionTimerParams{Interval: 1800, Refresher: "uac"}, 1800, false, true},
		{"inbound leg takes the role", true, StateConnected, 1800, "uac",
			&sipmod.SessionTimerParams{Interval: 1800, Refresher: "uas"}, 1800, true, true},
		{"inbound leg gives up the role", true, StateConnected, 1800, "uas",
			&sipmod.SessionTimerParams{Interval: 1800, Refresher: "uac"}, 1800, false, true},
		{"new interval, same role", false, StateConnected, 1800, "uac",
			&sipmod.SessionTimerParams{Interval: 600, Refresher: "uas"}, 600, true, true},
		{"held leg takes the role", false, StateHeld, 1800, "uas",
			&sipmod.SessionTimerParams{Interval: 1800, Refresher: "uas"}, 1800, true, true},
		{"request names no timer", false, StateConnected, 1800, "uac",
			nil, 1800, true, true},
		{"timer first agreed in mid call", false, StateConnected, 0, "",
			&sipmod.SessionTimerParams{Interval: 1800, Refresher: "uas"}, 1800, true, true},
		{"no timer and none asked for", false, StateConnected, 0, "",
			nil, 0, false, false},
		{"not answered yet", true, StateRinging, 1800, "uac",
			&sipmod.SessionTimerParams{Interval: 600, Refresher: "uas"}, 1800, false, false},
		{"hung up", false, StateHungUp, 1800, "uas",
			&sipmod.SessionTimerParams{Interval: 600, Refresher: "uas"}, 1800, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestSIPLeg(codec.CodecPCMU)
			l.log = slog.New(slog.DiscardHandler)
			if tc.inbound {
				l.inbound = &sipmod.InboundCall{}
			} else {
				l.outbound = &sipmod.OutboundCall{}
			}
			l.state = tc.state
			l.sessionInterval = tc.interval
			l.sessionRefresher = tc.refresher
			if tc.state == StateConnected || tc.state == StateHeld {
				l.startSessionTimer()
			}
			t.Cleanup(l.stopSessionTimer)

			l.ResetSessionTimer(tc.request)

			l.mu.RLock()
			defer l.mu.RUnlock()
			if l.sessionInterval != tc.wantInterval {
				t.Errorf("interval = %d, want %d", l.sessionInterval, tc.wantInterval)
			}
			if got := l.refreshesSessionLocked(); got != tc.wantRefreshes {
				t.Errorf("refreshesSessionLocked() = %v, want %v", got, tc.wantRefreshes)
			}
			if got := l.sessionTimer != nil; got != tc.wantTimer {
				t.Errorf("timer running = %v, want %v", got, tc.wantTimer)
			}
		})
	}
}

func sdpDirection(t *testing.T, body []byte) string {
	t.Helper()
	if len(body) == 0 {
		t.Fatal("no refresh SDP")
	}
	parsed, err := sipmod.ParseSDP(body)
	if err != nil {
		t.Fatalf("parse refresh SDP: %v", err)
	}
	if parsed.Direction == "" {
		return sipmod.DirSendRecv
	}
	return parsed.Direction
}

func TestSessionRefreshSDP_NotHeldIsSendRecv(t *testing.T) {
	l := newReofferLeg(t)

	if got := sdpDirection(t, l.sessionRefreshSDP()); got != sipmod.DirSendRecv {
		t.Errorf("refresh direction = %q, want sendrecv", got)
	}
}

// A refresh that offered sendrecv would take the call off hold at the peer.
func TestSessionRefreshSDP_KeepsLocalHold(t *testing.T) {
	l := newReofferLeg(t)
	t.Cleanup(func() { l.SetHeld(false) })

	// What Hold does once the peer has answered our sendonly offer.
	l.applyAnswer(reofferSDP(40000, sipmod.DirRecvOnly))
	l.SetHeld(true)

	if got := sdpDirection(t, l.sessionRefreshSDP()); got != sipmod.DirSendOnly {
		t.Errorf("refresh direction = %q, want sendonly", got)
	}
}

func TestSessionRefreshSDP_KeepsLocalHoldWithoutAnswerSDP(t *testing.T) {
	l := newReofferLeg(t)
	t.Cleanup(func() { l.SetHeld(false) })

	l.applyAnswer(nil)
	l.SetHeld(true)

	if got := sdpDirection(t, l.sessionRefreshSDP()); got != sipmod.DirSendOnly {
		t.Errorf("refresh direction = %q, want sendonly", got)
	}
}

// The peer holds us: we answered recvonly, and a refresh must say the same.
func TestSessionRefreshSDP_KeepsRemoteHold(t *testing.T) {
	l := newReofferLeg(t)
	t.Cleanup(func() { l.SetHeld(false) })

	answer, direction, err := l.ApplyRemoteOffer(reofferSDP(40000, sipmod.DirSendOnly))
	if err != nil {
		t.Fatalf("hold offer refused: %v", err)
	}
	l.HandleRemoteHold(direction)
	if !strings.Contains(string(answer), "a=recvonly") {
		t.Fatalf("answer to a hold offer is not recvonly:\n%s", answer)
	}

	if got := sdpDirection(t, l.sessionRefreshSDP()); got != sipmod.DirRecvOnly {
		t.Errorf("refresh direction = %q, want recvonly", got)
	}
}
