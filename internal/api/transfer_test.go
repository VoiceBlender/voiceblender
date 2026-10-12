package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// Header validation runs before the leg lookup, so a request that passes it
// reaches the lookup and misses (404), while a bad one stops at 400.
func TestDoTransferLeg_HeaderValidation(t *testing.T) {
	s := &Server{LegMgr: leg.NewManager(), Log: slog.Default()}
	cases := []struct {
		name     string
		req      TransferRequest
		wantCode int
		wantMsg  string
	}{
		{"both maps valid", TransferRequest{
			Target:         "sip:bob@example.com",
			ReferToHeaders: map[string]string{"x-inin-cnv": "b2de3fe4", "User-to-User": "00FA;encoding=hex"},
			Headers:        map[string]string{"X-Correlation-ID": "abc", "Referred-By": "<sip:alice@example.com>"},
		}, http.StatusNotFound, "leg not found"},
		{"target already carries a different header", TransferRequest{
			Target:         "sip:bob@example.com?X-Old=1",
			ReferToHeaders: map[string]string{"X-New": "2"},
		}, http.StatusNotFound, "leg not found"},
		{"refer_to_headers: bad name", TransferRequest{
			Target: "sip:bob@example.com", ReferToHeaders: map[string]string{"X Foo": "1"},
		}, http.StatusBadRequest, "invalid refer_to_headers"},
		{"refer_to_headers: Replaces", TransferRequest{
			Target: "sip:bob@example.com", ReferToHeaders: map[string]string{"replaces": "abc"},
		}, http.StatusBadRequest, "invalid refer_to_headers"},
		{"refer_to_headers: dialog header", TransferRequest{
			Target: "sip:bob@example.com", ReferToHeaders: map[string]string{"From": "<sip:ceo@example.com>"},
		}, http.StatusBadRequest, "invalid refer_to_headers"},
		{"refer_to_headers: body", TransferRequest{
			Target: "sip:bob@example.com", ReferToHeaders: map[string]string{"body": "v=0"},
		}, http.StatusBadRequest, "invalid refer_to_headers"},
		{"refer_to_headers: CRLF in value", TransferRequest{
			Target: "sip:bob@example.com", ReferToHeaders: map[string]string{"X-A": "1\r\nVia: x"},
		}, http.StatusBadRequest, "invalid refer_to_headers"},
		{"refer_to_headers: case-insensitive duplicate", TransferRequest{
			Target: "sip:bob@example.com", ReferToHeaders: map[string]string{"X-A": "1", "x-a": "2"},
		}, http.StatusBadRequest, "invalid refer_to_headers"},
		{"refer_to_headers: already on target", TransferRequest{
			Target: "sip:bob@example.com?x-a=0", ReferToHeaders: map[string]string{"X-A": "1"},
		}, http.StatusBadRequest, "already set on target"},
		{"Replaces on target with replaces_leg_id", TransferRequest{
			Target: "sip:bob@example.com?Replaces=abc%3Bto-tag%3Dx%3Bfrom-tag%3Dy", ReplacesLegID: "other",
		}, http.StatusBadRequest, "already set on target"},
		{"headers: Refer-To", TransferRequest{
			Target: "sip:bob@example.com", Headers: map[string]string{"Refer-To": "<sip:evil@example.com>"},
		}, http.StatusBadRequest, "invalid headers"},
		{"headers: Contact", TransferRequest{
			Target: "sip:bob@example.com", Headers: map[string]string{"Contact": "<sip:evil@example.com>"},
		}, http.StatusBadRequest, "invalid headers"},
		{"headers: CRLF in value", TransferRequest{
			Target: "sip:bob@example.com", Headers: map[string]string{"X-A": "1\r\nRefer-To: <sip:evil@example.com>"},
		}, http.StatusBadRequest, "invalid headers"},
	}
	for _, c := range cases {
		_, err := s.doTransferLeg("no-such-leg", c.req)
		var ae *apiError
		if !errors.As(err, &ae) {
			t.Errorf("%s: err = %v, want *apiError", c.name, err)
			continue
		}
		if ae.Code != c.wantCode || !strings.Contains(ae.Message, c.wantMsg) {
			t.Errorf("%s: got %d %q, want %d containing %q", c.name, ae.Code, ae.Message, c.wantCode, c.wantMsg)
		}
	}
}

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
