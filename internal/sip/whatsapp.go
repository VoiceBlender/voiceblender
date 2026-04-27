package sip

import (
	"context"
	"fmt"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const (
	// WhatsAppOutboundHost is the SIP target for outbound calls to WhatsApp
	// users, per Meta's docs: sip:+E164@wa.meta.vc;transport=tls.
	WhatsAppOutboundHost = "wa.meta.vc"
	// whatsAppInboundDomain matches inbound From URIs from any Meta SIP
	// gateway: meta.vc or any subdomain (wa.meta.vc, etc.).
	whatsAppInboundDomain = "meta.vc"
)

// IsWhatsAppInvite reports whether an inbound INVITE's From URI host matches
// any Meta WhatsApp calling host (meta.vc or any subdomain).
func IsWhatsAppInvite(call *InboundCall) bool {
	if call == nil || call.Request == nil {
		return false
	}
	from := call.Request.From()
	if from == nil {
		return false
	}
	host := strings.ToLower(from.Address.Host)
	return host == whatsAppInboundDomain || strings.HasSuffix(host, "."+whatsAppInboundDomain)
}

// WhatsAppInviteOptions holds parameters for an outbound INVITE to WhatsApp.
type WhatsAppInviteOptions struct {
	// FromNumber is the business phone number in E.164 form. Used both as
	// the user-part of the From URI (with leading '+') and as the digest
	// auth username (with the '+' stripped) per Meta's docs.
	FromNumber string
	Password   string // Meta-generated password
	SDPOffer   []byte // complete SDP offer from PCMedia (post ICE gathering)
	Headers    []sip.Header
}

// WhatsAppOutboundCall wraps the UAC dialog and the remote SDP answer.
type WhatsAppOutboundCall struct {
	Dialog    *sipgo.DialogClientSession
	AnswerSDP []byte
}

// InviteWhatsApp sends an outbound INVITE over SIP/TLS to the given recipient.
// Unlike Invite(), it carries a pre-built WebRTC-style SDP offer (from
// PCMedia) and does not allocate a classic RTP session. Digest authentication
// is driven by sipgo's dialog layer using the Meta-issued password.
func (e *Engine) InviteWhatsApp(ctx context.Context, recipient sip.Uri, opts WhatsAppInviteOptions) (*WhatsAppOutboundCall, error) {
	if e.tlsPort == 0 {
		return nil, fmt.Errorf("SIP TLS not configured; cannot place WhatsApp call")
	}
	if len(opts.SDPOffer) == 0 {
		return nil, fmt.Errorf("SDPOffer required")
	}
	if opts.FromNumber == "" || opts.Password == "" {
		return nil, fmt.Errorf("FromNumber and Password required (digest auth)")
	}
	// Meta requires the URI user-part in E.164 with leading '+', and the
	// digest auth username in E.164 without '+'.
	fromURIUser := "+" + strings.TrimPrefix(opts.FromNumber, "+")
	digestUser := strings.TrimPrefix(opts.FromNumber, "+")

	req := sip.NewRequest(sip.INVITE, recipient)
	// sipgo picks UDP unless transport is forced; "sips:" scheme alone
	// only upgrades TCP→TLS.
	req.SetTransport("TLS")
	req.SetBody(opts.SDPOffer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	fromURI := sip.Uri{
		Scheme: "sip",
		User:   fromURIUser,
		Host:   e.bindIP,
	}
	from := &sip.FromHeader{Address: fromURI}
	from.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(from)

	// Contact must route subsequent in-dialog requests back over TLS.
	contactURI := sip.Uri{
		Scheme: "sip",
		User:   fromURIUser,
		Host:   e.bindIP,
		Port:   e.tlsPort,
	}
	contactURI.UriParams = sip.NewParams()
	contactURI.UriParams.Add("transport", "tls")
	req.AppendHeader(&sip.ContactHeader{Address: contactURI})

	for _, h := range opts.Headers {
		req.AppendHeader(h)
	}

	e.logSIPMessage("outbound", req)

	ds, err := e.dcCache.WriteInvite(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("write invite: %w", err)
	}

	if err := ds.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: digestUser,
		Password: opts.Password,
		OnResponse: func(res *sip.Response) error {
			e.logSIPMessage("inbound", res)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("wait answer: %w", err)
	}
	if ds.InviteResponse != nil {
		e.logSIPMessage("inbound", ds.InviteResponse)
	}

	if err := ds.Ack(ctx); err != nil {
		return nil, fmt.Errorf("ack: %w", err)
	}

	answerBody := ds.InviteResponse.Body()
	if len(answerBody) == 0 {
		_ = ds.Bye(ctx)
		return nil, fmt.Errorf("200 OK missing SDP body")
	}

	return &WhatsAppOutboundCall{
		Dialog:    ds,
		AnswerSDP: append([]byte(nil), answerBody...),
	}, nil
}

// WhatsAppRecipientURI builds the standard WhatsApp Request-URI for an
// outbound call to the given destination number. Meta's docs use a
// "sip:+E164@wa.meta.vc;transport=tls" form rather than sips:; the latter
// causes 404s because Meta's internal routing is not strict-TLS end-to-end.
func WhatsAppRecipientURI(toUser string) sip.Uri {
	user := "+" + strings.TrimPrefix(toUser, "+")
	uri := sip.Uri{
		Scheme: "sip",
		User:   user,
		Host:   WhatsAppOutboundHost,
		Port:   5061,
	}
	uri.UriParams = sip.NewParams()
	uri.UriParams.Add("transport", "tls")
	return uri
}
