package sip

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestParseGrantedExpires_FromContactParam(t *testing.T) {
	res := sip.NewResponse(sip.StatusOK, "OK")
	res.AppendHeader(sip.NewHeader("Contact", "<sip:alice@10.0.0.5:5060>;expires=120"))
	if got := parseGrantedExpires(res, 3600); got != 120 {
		t.Errorf("Contact expires=120 → %d, want 120", got)
	}
}

func TestParseGrantedExpires_FromTopLevelHeader(t *testing.T) {
	res := sip.NewResponse(sip.StatusOK, "OK")
	res.AppendHeader(sip.NewHeader("Expires", "300"))
	if got := parseGrantedExpires(res, 3600); got != 300 {
		t.Errorf("Expires:300 → %d, want 300", got)
	}
}

func TestParseGrantedExpires_FallsBackToRequested(t *testing.T) {
	res := sip.NewResponse(sip.StatusOK, "OK")
	if got := parseGrantedExpires(res, 3600); got != 3600 {
		t.Errorf("no header → %d, want 3600 (requested)", got)
	}
}

// TestOutboundRegistration_FromHost pins FromHost to the AOR realm.
//
// The registrar host and the AOR host are deliberately DIFFERENT strings. The
// struct holds both registrarURI and aor, so a getter that reached for the
// wrong one would still look plausible; only a differing registrar host makes
// that substitution observable.
func TestOutboundRegistration_FromHost(t *testing.T) {
	r := NewOutboundRegistration(nil, nil, nil, OutboundRegistrationConfig{}, OutboundRegistrationParams{
		ID:           "t1",
		RegistrarURI: sip.Uri{Scheme: "sip", Host: "sbc.carrier.net", Port: 5060},
		AOR:          sip.Uri{Scheme: "sip", User: "alice", Host: "pbx.example.com"},
		Password:     "x",
	})
	if got := r.FromHost(); got != "pbx.example.com" {
		t.Errorf("FromHost() = %q, want the AOR realm %q (not the registrar host)", got, "pbx.example.com")
	}
}

func TestOutboundRegistrationConfig_DefaultsApplied(t *testing.T) {
	c := OutboundRegistrationConfig{}.withDefaults()
	if c.DefaultExpiresSeconds != 3600 {
		t.Errorf("DefaultExpiresSeconds = %d, want 3600", c.DefaultExpiresSeconds)
	}
	if c.MinExpiresSeconds != 60 {
		t.Errorf("MinExpiresSeconds = %d, want 60", c.MinExpiresSeconds)
	}
	if c.MaxExpiresSeconds != 7200 {
		t.Errorf("MaxExpiresSeconds = %d, want 7200", c.MaxExpiresSeconds)
	}
	if c.RefreshRatio != 0.5 {
		t.Errorf("RefreshRatio = %v, want 0.5", c.RefreshRatio)
	}
}

func TestOutboundRegistrationConfig_RefreshRatioGuards(t *testing.T) {
	// Out-of-range values clamp back to the default ratio.
	for _, in := range []float64{0, -0.1, 1.0, 1.5} {
		got := OutboundRegistrationConfig{RefreshRatio: in}.withDefaults().RefreshRatio
		if got != 0.5 {
			t.Errorf("RefreshRatio(%v) → %v, want 0.5", in, got)
		}
	}
}

func TestExtractSource(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"10.0.0.5:5060", "10.0.0.5", 5060},
		{"[::1]:5060", "::1", 5060},
		{"", "", 0},
		{"malformed", "", 0},
	}
	for _, c := range cases {
		h, p := extractSource(c.in)
		if h != c.wantHost || p != c.wantPort {
			t.Errorf("extractSource(%q) = (%q,%d), want (%q,%d)", c.in, h, p, c.wantHost, c.wantPort)
		}
	}
}
