package sip

import (
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func dialogWithContact(t *testing.T, requestURI, inviteTransport, contact string) *sipgo.DialogClientSession {
	t.Helper()
	var ru sip.Uri
	if err := sip.ParseUri(requestURI, &ru); err != nil {
		t.Fatal(err)
	}
	req := sip.NewRequest(sip.INVITE, ru)
	req.SetTransport(inviteTransport)
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	if contact != "" {
		var cu sip.Uri
		if err := sip.ParseUri(contact, &cu); err != nil {
			t.Fatal(err)
		}
		res.AppendHeader(&sip.ContactHeader{Address: cu})
	}
	return &sipgo.DialogClientSession{Dialog: sipgo.Dialog{InviteRequest: req, InviteResponse: res}}
}

func TestAdoptRemoteTargetTransport(t *testing.T) {
	cases := []struct {
		name, requestURI, transport, contact, want string
	}{
		{"explicit udp on a tls call", "sip:bob@peer:5061;transport=tls", "TLS", "sip:bob@peer:5060;transport=udp", "UDP"},
		{"explicit tcp", "sip:bob@peer", "UDP", "sip:bob@peer:5060;transport=tcp", "TCP"},
		{"no transport parameter keeps the invite's", "sip:bob@peer:5061;transport=tls", "TLS", "sip:bob@peer:5060", "TLS"},
		{"no contact", "sip:bob@peer", "UDP", "", "UDP"},
		{"secure dialog is not downgraded", "sips:bob@peer:5061", "TLS", "sip:bob@peer:5060;transport=udp", "TLS"},
		{"sips contact with tcp means tls", "sip:bob@peer", "UDP", "sips:bob@peer:5061;transport=tcp", "TLS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := dialogWithContact(t, tc.requestURI, tc.transport, tc.contact)
			adoptRemoteTargetTransport(ds)
			if got := ds.InviteRequest.Transport(); got != tc.want {
				t.Errorf("in-dialog transport = %s, want %s", got, tc.want)
			}
		})
	}
}
