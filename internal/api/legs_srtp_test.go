package api

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
)

func newSRTPTestServer(t *testing.T, mode sipmod.SRTPMode) *Server {
	t.Helper()
	s := newTestServer(t)
	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: freeUDPPort(t),
		SIPHost:  "test",
		Log:      slog.Default(),
		SRTPMode: mode,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	s.SIPEngine = engine
	return s
}

func boolPtr(b bool) *bool { return &b }

func TestOutboundSRTP(t *testing.T) {
	cases := []struct {
		name    string
		mode    sipmod.SRTPMode
		srtp    *bool
		rtt     bool
		want    bool
		wantErr string
	}{
		{"optional default is plain", sipmod.SRTPOptional, nil, false, false, ""},
		{"optional opt in", sipmod.SRTPOptional, boolPtr(true), false, true, ""},
		{"optional explicit off", sipmod.SRTPOptional, boolPtr(false), false, false, ""},
		{"required default is srtp", sipmod.SRTPRequired, nil, false, true, ""},
		{"required cannot opt out", sipmod.SRTPRequired, boolPtr(false), false, false, "SIP_SRTP=required"},
		{"disabled default is plain", sipmod.SRTPDisabled, nil, false, false, ""},
		{"disabled cannot opt in", sipmod.SRTPDisabled, boolPtr(true), false, false, "SIP_SRTP=disabled"},
		{"rtt with srtp", sipmod.SRTPOptional, boolPtr(true), true, true, ""},
		{"rtt under required", sipmod.SRTPRequired, nil, true, true, ""},
		{"rtt on a plain call", sipmod.SRTPOptional, nil, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSRTPTestServer(t, tc.mode)
			got, err := s.outboundSRTP(CreateLegRequest{SRTP: tc.srtp, RTT: tc.rtt}, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestCreateLeg_SRTPConflictIs400(t *testing.T) {
	s := newSRTPTestServer(t, sipmod.SRTPDisabled)
	w := doRequest(s, http.MethodPost, "/v1/legs", `{"type":"sip","to":"sip:bob@example.com","srtp":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SIP_SRTP=disabled") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func newSRTPTrunk(t *testing.T, s *Server, srtp string) sipmod.Trunk {
	t.Helper()
	resp, err := s.doCreateTrunk(CreateTrunkRequest{Type: "ip_ip", SRTP: srtp, IPIP: &IPIPTrunkSpec{PeerURI: "sip:192.0.2.50:5060"}})
	if err != nil {
		t.Fatalf("create trunk: %v", err)
	}
	tr := s.SIPEngine.Trunks().Get(resp.ID)
	t.Cleanup(func() { _ = s.doDeleteTrunk(resp.ID) })
	return tr
}

func TestOutboundSRTP_TrunkOverridesServer(t *testing.T) {
	cases := []struct {
		name    string
		server  sipmod.SRTPMode
		trunk   string
		srtp    *bool
		want    bool
		wantErr string
	}{
		{"trunk relaxes required", sipmod.SRTPRequired, "disabled", nil, false, ""},
		{"trunk relaxes required, opt in refused", sipmod.SRTPRequired, "disabled", boolPtr(true), false, "srtp=disabled"},
		{"trunk tightens optional", sipmod.SRTPOptional, "required", nil, true, ""},
		{"trunk tightens optional, opt out refused", sipmod.SRTPOptional, "required", boolPtr(false), false, "srtp=required"},
		{"optional trunk under disabled server", sipmod.SRTPDisabled, "optional", boolPtr(true), true, ""},
		{"trunk without srtp follows the server", sipmod.SRTPRequired, "", nil, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSRTPTestServer(t, tc.server)
			tr := newSRTPTrunk(t, s, tc.trunk)
			got, err := s.outboundSRTP(CreateLegRequest{SRTP: tc.srtp}, tr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "trunk "+tr.ID()) {
					t.Fatalf("err = %v, want one naming the trunk and %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestApplyFromIdentity_CarriesTrunkSRTP(t *testing.T) {
	s := newSRTPTestServer(t, sipmod.SRTPOptional)
	tr := newSRTPTrunk(t, s, "required")
	var opts sipmod.InviteOptions
	s.applyFromIdentity(tr, "", sip.Uri{Scheme: "sip", User: "bob", Host: "192.0.2.60"}, &opts)
	if opts.SRTPMode != sipmod.SRTPRequired {
		t.Errorf("InviteOptions.SRTPMode = %q, want required", opts.SRTPMode)
	}
	opts = sipmod.InviteOptions{}
	s.applyFromIdentity(nil, "", sip.Uri{Scheme: "sip", User: "bob", Host: "192.0.2.60"}, &opts)
	if opts.SRTPMode != "" {
		t.Errorf("no trunk: SRTPMode = %q", opts.SRTPMode)
	}
}

func TestCreateTrunk_SRTPValidation(t *testing.T) {
	s := newSRTPTestServer(t, sipmod.SRTPOptional)
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `{"type":"ip_ip","srtp":"maybe","ip_ip":{"peer_uri":"sip:192.0.2.50"}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "srtp") {
		t.Errorf("status = %d body = %s", w.Code, w.Body.String())
	}
	tr := newSRTPTrunk(t, s, "disabled")
	if v := tr.Snapshot().SRTP; v != sipmod.SRTPDisabled {
		t.Errorf("view srtp = %q", v)
	}
}
