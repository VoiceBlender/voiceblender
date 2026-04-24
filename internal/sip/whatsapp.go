package sip

import (
	"context"
	"fmt"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// WhatsAppMetaHost is the fully qualified host WhatsApp calls originate from
// and terminate to for business SIP integration.
const WhatsAppMetaHost = "meta.vc"

// IsWhatsAppInvite reports whether an inbound INVITE's From URI host matches
// Meta's WhatsApp calling domain (meta.vc or any subdomain).
func IsWhatsAppInvite(call *InboundCall) bool {
	if call == nil || call.Request == nil {
		return false
	}
	from := call.Request.From()
	if from == nil {
		return false
	}
	host := strings.ToLower(from.Address.Host)
	return host == WhatsAppMetaHost || strings.HasSuffix(host, "."+WhatsAppMetaHost)
}

// WhatsAppInviteOptions holds parameters for an outbound INVITE to WhatsApp.
type WhatsAppInviteOptions struct {
	FromUser string // digest auth username = business phone number without '+'
	Password string // Meta-generated password
	SDPOffer []byte // complete SDP offer from PCMedia (post ICE gathering)
	Headers  []sip.Header
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
	if opts.FromUser == "" || opts.Password == "" {
		return nil, fmt.Errorf("FromUser and Password required (digest auth)")
	}

	req := sip.NewRequest(sip.INVITE, recipient)
	req.SetBody(opts.SDPOffer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	fromURI := sip.Uri{
		Scheme: "sips",
		User:   opts.FromUser,
		Host:   e.bindIP,
	}
	from := &sip.FromHeader{Address: fromURI}
	from.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(from)

	// Override default sip: Contact with sips: pointing to our TLS port so
	// Meta routes subsequent in-dialog requests back over TLS.
	contact := &sip.ContactHeader{Address: sip.Uri{
		Scheme: "sips",
		User:   opts.FromUser,
		Host:   e.bindIP,
		Port:   e.tlsPort,
	}}
	req.AppendHeader(contact)

	for _, h := range opts.Headers {
		req.AppendHeader(h)
	}

	e.logSIPMessage("outbound", req)

	ds, err := e.dcCache.WriteInvite(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("write invite: %w", err)
	}

	if err := ds.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: opts.FromUser,
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
// outbound call to the given destination number (with or without leading '+').
func WhatsAppRecipientURI(toUser string) sip.Uri {
	return sip.Uri{
		Scheme: "sips",
		User:   strings.TrimPrefix(toUser, "+"),
		Host:   WhatsAppMetaHost,
		Port:   5061,
	}
}
