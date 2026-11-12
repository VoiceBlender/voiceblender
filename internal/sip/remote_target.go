package sip

import (
	"strings"

	"github.com/emiago/sipgo"
)

// adoptRemoteTargetTransport sends the dialog's later requests (ACK, BYE,
// re-INVITE, NOTIFY) over the transport the peer's Contact names. RFC 3261
// §12.2.1.1 resolves the remote target with RFC 3263, where an explicit
// transport parameter decides; sipgo instead stamps every in-dialog request
// with the INVITE's transport. A Contact without the parameter keeps the
// INVITE's transport, and a dialog set up to a sips: URI stays on TLS.
func adoptRemoteTargetTransport(ds *sipgo.DialogClientSession) {
	if ds == nil || ds.InviteRequest == nil || ds.InviteResponse == nil {
		return
	}
	c := ds.InviteResponse.Contact()
	if c == nil || c.Address.UriParams == nil {
		return
	}
	tp, ok := c.Address.UriParams.Get("transport")
	if !ok || tp == "" {
		return
	}
	tp = strings.ToUpper(tp)
	if c.Address.IsEncrypted() && tp == "TCP" {
		tp = "TLS"
	}
	current := ds.InviteRequest.Transport()
	if strings.EqualFold(current, tp) {
		return
	}
	if ds.InviteRequest.Recipient.IsEncrypted() && tp != "TLS" {
		return
	}
	ds.InviteRequest.SetTransport(tp)
}
