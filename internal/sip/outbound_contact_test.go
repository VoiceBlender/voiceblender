package sip

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

func tlsContactInvite(t *testing.T, uri, transport string) *sip.Request {
	t.Helper()
	var u sip.Uri
	if err := sip.ParseUri(uri, &u); err != nil {
		t.Fatal(err)
	}
	req := sip.NewRequest(sip.INVITE, u)
	if transport != "" {
		req.SetTransport(transport)
	}
	return req
}

// A TLS INVITE advertises the TLS listener: the peer takes the dialog's
// transport from our Contact, so a UDP Contact sends its ACK and BYE there.
func TestOutboundContact(t *testing.T) {
	e := &Engine{publicHost: "vb.example", bindPort: 5060, tlsPort: 5061}
	cases := []struct {
		name, uri, transport, host, want string
	}{
		{"udp keeps the dialog default", "sip:bob@peer.example", "", "", ""},
		{"udp local peer", "sip:bob@10.0.0.2", "", "10.0.0.1", "sip:10.0.0.1:5060"},
		{"tls sip uri", "sip:bob@peer.example;transport=tls", "TLS", "", "sip:vb.example:5061;transport=tls"},
		{"tls sips uri", "sips:bob@peer.example", "TLS", "", "sips:vb.example:5061"},
		{"tls local peer", "sips:bob@10.0.0.2", "TLS", "10.0.0.1", "sips:10.0.0.1:5061"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := e.outboundContact(tlsContactInvite(t, tc.uri, tc.transport), tc.host)
			got := ""
			if c != nil {
				got = c.Address.String()
			}
			if got != tc.want {
				t.Errorf("Contact = %q, want %q", got, tc.want)
			}
		})
	}
}

// Without a TLS listener there is no TLS address to advertise.
func TestOutboundContact_NoTLSListener(t *testing.T) {
	e := &Engine{publicHost: "vb.example", bindPort: 5060}
	if c := e.outboundContact(tlsContactInvite(t, "sips:bob@peer.example", "TLS"), ""); c != nil {
		t.Errorf("Contact = %s, want the dialog default", c.Address.String())
	}
}
