package sip

import (
	"log/slog"
	"testing"

	"github.com/emiago/sipgo/sip"
)

const (
	// Fingerprint value is arbitrary; only the a=fingerprint: attribute matters.
	webrtcOfferSDP = "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=fingerprint:sha-256 AB:CD:EF:00\r\n" +
		"a=setup:actpass\r\n" +
		"a=ice-ufrag:abcd\r\n" +
		"a=ice-pwd:abcdefghijklmnopqrstuvwx\r\n"

	plainRTPSDP = "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"m=audio 40000 RTP/AVP 0\r\n" +
		"c=IN IP4 127.0.0.1\r\n"
)

func newInboundCallWithFrom(fromHost string, sdp []byte) *InboundCall {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sips", User: "1234", Host: "business.example"})
	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sips", User: "15551234567", Host: fromHost}}
	req.AppendHeader(from)
	if len(sdp) > 0 {
		req.SetBody(sdp)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	return &InboundCall{Request: req}
}

func TestIsWhatsAppInvite(t *testing.T) {
	cases := []struct {
		name string
		host string
		sdp  []byte
		want bool
	}{
		{"exact meta.vc + DTLS", "meta.vc", []byte(webrtcOfferSDP), true},
		{"wa subdomain + DTLS", "wa.meta.vc", []byte(webrtcOfferSDP), true},
		{"deep subdomain + DTLS", "us-east-1.wa.meta.vc", []byte(webrtcOfferSDP), true},
		{"mixed case + DTLS", "WA.Meta.VC", []byte(webrtcOfferSDP), true},
		{"meta.vc + plain RTP", "meta.vc", []byte(plainRTPSDP), false},
		{"wa subdomain + plain RTP", "wa.meta.vc", []byte(plainRTPSDP), false},
		{"meta.vc + no SDP", "meta.vc", nil, false},
		{"lookalike suffix + DTLS", "evilmeta.vc", []byte(webrtcOfferSDP), false},
		{"unrelated host + DTLS", "example.com", []byte(webrtcOfferSDP), false},
		{"empty host + DTLS", "", []byte(webrtcOfferSDP), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsWhatsAppInvite(newInboundCallWithFrom(tc.host, tc.sdp))
			if got != tc.want {
				t.Errorf("IsWhatsAppInvite(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestIsWhatsAppInvite_FingerprintCaseInsensitive(t *testing.T) {
	sdp := []byte("v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\nA=Fingerprint:sha-256 AA:BB\r\n")
	if !IsWhatsAppInvite(newInboundCallWithFrom("meta.vc", sdp)) {
		t.Error("expected match when a=fingerprint is mixed-case")
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
