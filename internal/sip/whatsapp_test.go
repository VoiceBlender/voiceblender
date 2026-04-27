package sip

import (
	"log/slog"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func newInboundCallWithFrom(fromHost string) *InboundCall {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sips", User: "1234", Host: "business.example"})
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sips", User: "15551234567", Host: fromHost}}
	req.AppendHeader(from)
	return &InboundCall{Request: req}
}

func TestIsWhatsAppInvite(t *testing.T) {
	cases := []struct {
		name string
		host string
		want bool
	}{
		{"exact meta.vc", "meta.vc", true},
		{"wa subdomain", "wa.meta.vc", true},
		{"deep subdomain", "us-east-1.wa.meta.vc", true},
		{"mixed case", "WA.Meta.VC", true},
		{"lookalike suffix", "evilmeta.vc", false},
		{"unrelated host", "example.com", false},
		{"empty host", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsWhatsAppInvite(newInboundCallWithFrom(tc.host))
			if got != tc.want {
				t.Errorf("IsWhatsAppInvite(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestIsWhatsAppInvite_NilSafe(t *testing.T) {
	if IsWhatsAppInvite(nil) {
		t.Error("nil call should not match")
	}
	if IsWhatsAppInvite(&InboundCall{}) {
		t.Error("empty call should not match")
	}
}

func TestWhatsAppRecipientURI(t *testing.T) {
	cases := []struct {
		in       string
		wantUser string
	}{
		{"+15551234567", "+15551234567"},
		{"15551234567", "+15551234567"},
		{"+442071234567", "+442071234567"},
	}
	for _, tc := range cases {
		uri := WhatsAppRecipientURI(tc.in)
		if uri.User != tc.wantUser {
			t.Errorf("user = %q, want %q", uri.User, tc.wantUser)
		}
		if uri.Host != WhatsAppOutboundHost {
			t.Errorf("host = %q, want %q", uri.Host, WhatsAppOutboundHost)
		}
		if uri.Port != 5061 {
			t.Errorf("port = %d, want 5061", uri.Port)
		}
		if uri.Scheme != "sip" {
			t.Errorf("scheme = %q, want sip", uri.Scheme)
		}
		if v, ok := uri.UriParams.Get("transport"); !ok || v != "tls" {
			t.Errorf("transport param = %q ok=%v, want tls", v, ok)
		}
	}
}

func TestInviteWhatsApp_RejectsWithoutTLS(t *testing.T) {
	udpPort := pickFreePort(t, "udp")
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: udpPort,
		SIPHost:  "test",
		Log:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	_, err = engine.InviteWhatsApp(t.Context(), WhatsAppRecipientURI("+15551234567"), WhatsAppInviteOptions{
		FromNumber: "15551234567",
		Password:   "x",
		SDPOffer:   []byte("v=0\r\n"),
	})
	if err == nil {
		t.Fatal("expected error when TLS not configured")
	}
}

func TestInviteWhatsApp_RejectsMissingFields(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	engine, err := NewEngine(EngineConfig{
		BindIP:      "127.0.0.1",
		BindPort:    pickFreePort(t, "udp"),
		TLSBindPort: pickFreePort(t, "tcp"),
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
		SIPHost:     "test",
		Log:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	cases := []struct {
		name string
		opts WhatsAppInviteOptions
	}{
		{"no SDPOffer", WhatsAppInviteOptions{FromNumber: "u", Password: "p"}},
		{"no FromNumber", WhatsAppInviteOptions{SDPOffer: []byte("v=0\r\n"), Password: "p"}},
		{"no Password", WhatsAppInviteOptions{SDPOffer: []byte("v=0\r\n"), FromNumber: "u"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := engine.InviteWhatsApp(t.Context(), WhatsAppRecipientURI("+15551234567"), tc.opts)
			if err == nil {
				t.Fatal("expected error for incomplete options")
			}
		})
	}
}
