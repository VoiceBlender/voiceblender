package sip

import (
	"log/slog"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestOfferCrypto_PerCallPolicy(t *testing.T) {
	cases := []struct {
		engine, override SRTPMode
		requested, want  bool
	}{
		{SRTPOptional, "", false, false},
		{SRTPOptional, "", true, true},
		{SRTPOptional, SRTPRequired, false, true},
		{SRTPRequired, "", false, true},
		{SRTPRequired, SRTPDisabled, true, false},
		{SRTPRequired, SRTPOptional, false, false},
		{SRTPDisabled, SRTPOptional, true, true},
	}
	for _, tc := range cases {
		e := &Engine{srtpMode: tc.engine}
		c, err := e.offerCrypto(tc.override, tc.requested)
		if err != nil {
			t.Fatal(err)
		}
		if got := c != nil; got != tc.want {
			t.Errorf("engine %s override %q requested %v: offers SRTP = %v", tc.engine, tc.override, tc.requested, got)
		}
		want := tc.override
		if want == "" {
			want = tc.engine
		}
		if got := e.EffectiveSRTPMode(tc.override); got != want {
			t.Errorf("EffectiveSRTPMode(%q) = %s, want %s", tc.override, got, want)
		}
	}
}

func TestTrunkSRTPMode_Snapshot(t *testing.T) {
	ipip := NewIPIPTrunk(nil, nil, slog.Default(), IPIPTrunkParams{
		ID: "t1", PeerURI: sip.Uri{Scheme: "sip", Host: "192.0.2.1"}, SRTPMode: SRTPRequired,
	})
	if ipip.SRTPMode() != SRTPRequired || ipip.Snapshot().SRTP != SRTPRequired {
		t.Errorf("ip_ip trunk srtp = %q / view %q", ipip.SRTPMode(), ipip.Snapshot().SRTP)
	}
	reg := NewOutboundRegistration(&Engine{}, nil, slog.Default(), OutboundRegistrationConfig{}, OutboundRegistrationParams{
		ID: "t2", RegistrarURI: sip.Uri{Scheme: "sip", Host: "pbx.example.com", Port: 5060},
		AOR: sip.Uri{Scheme: "sip", User: "a", Host: "pbx.example.com"}, SRTPMode: SRTPDisabled,
	})
	if reg.SRTPMode() != SRTPDisabled || reg.Snapshot().SRTP != SRTPDisabled {
		t.Errorf("sip_register trunk srtp = %q / view %q", reg.SRTPMode(), reg.Snapshot().SRTP)
	}
	if v := NewIPIPTrunk(nil, nil, slog.Default(), IPIPTrunkParams{ID: "t3"}).Snapshot().SRTP; v != "" {
		t.Errorf("a trunk without srtp reports %q", v)
	}
}
