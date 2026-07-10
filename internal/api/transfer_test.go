package api

import "testing"

// The pendingReferStore state machine backs the app-driven inbound-REFER
// consult: exactly one of accept / decline(timeout) may win, and terminal
// operations require a prior accept.
func TestPendingReferStore_Transitions(t *testing.T) {
	s := newPendingReferStore()

	// Unknown leg: every lookup misses.
	if _, ok := s.markAccepted("nope"); ok {
		t.Error("markAccepted on unknown leg should miss")
	}
	if _, ok := s.takeIfPending("nope"); ok {
		t.Error("takeIfPending on unknown leg should miss")
	}
	if _, ok := s.takeAccepted("nope"); ok {
		t.Error("takeAccepted on unknown leg should miss")
	}

	// Accept path: park → accept → progress(peek) → complete(take).
	s.put(&pendingRefer{legID: "L1"})
	if _, ok := s.peekAccepted("L1"); ok {
		t.Error("peekAccepted before accept should miss")
	}
	if _, ok := s.takeAccepted("L1"); ok {
		t.Error("takeAccepted before accept should miss")
	}
	if _, ok := s.markAccepted("L1"); !ok {
		t.Fatal("first markAccepted should succeed")
	}
	if _, ok := s.markAccepted("L1"); ok {
		t.Error("second markAccepted should miss (already accepted)")
	}
	if _, ok := s.takeIfPending("L1"); ok {
		t.Error("takeIfPending after accept should miss (decline no longer valid)")
	}
	if _, ok := s.peekAccepted("L1"); !ok {
		t.Error("peekAccepted after accept should hit")
	}
	if _, ok := s.takeAccepted("L1"); !ok {
		t.Error("takeAccepted after accept should hit")
	}
	if _, ok := s.peekAccepted("L1"); ok {
		t.Error("entry should be gone after takeAccepted")
	}

	// Decline/timeout path wins the race against a late accept.
	s.put(&pendingRefer{legID: "L2"})
	if _, ok := s.takeIfPending("L2"); !ok {
		t.Fatal("takeIfPending on a pending entry should hit")
	}
	if _, ok := s.markAccepted("L2"); ok {
		t.Error("markAccepted after decline should miss (entry removed)")
	}
}

func TestSIPReasonPhrase(t *testing.T) {
	cases := map[int]string{
		100: "Trying",
		180: "Ringing",
		200: "OK",
		603: "Decline",
		499: "Transfer", // unknown → generic
	}
	for code, want := range cases {
		if got := sipReasonPhrase(code); got != want {
			t.Errorf("sipReasonPhrase(%d) = %q, want %q", code, got, want)
		}
	}
}
