package sip

import (
	"context"
	"fmt"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const (
	WhatsAppOutboundHost  = "wa.meta.vc"
	whatsAppInboundDomain = "meta.vc"
)

// IsWhatsAppInvite reports whether an inbound INVITE comes from a Meta
// WhatsApp gateway (meta.vc or any subdomain).
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

type WhatsAppInviteOptions struct {
	// FromNumber is the business phone number in E.164. Used as the From
	// URI user (with leading '+'). When DigestUsername is empty it also
	// serves as the digest auth username (with the '+' stripped, per Meta).
	FromNumber     string
	DigestUsername string // optional override; defaults to FromNumber without '+'
	Password       string
	SDPOffer       []byte
	Headers        []sip.Header
}

type WhatsAppOutboundCall struct {
	Dialog    *sipgo.DialogClientSession
	AnswerSDP []byte
}

// InviteWhatsApp sends an outbound INVITE over SIP/TLS with a pre-built
// WebRTC SDP offer and digest auth credentials.
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
	fromURIUser := "+" + strings.TrimPrefix(opts.FromNumber, "+")
	digestUser := opts.DigestUsername
	if digestUser == "" {
		digestUser = strings.TrimPrefix(opts.FromNumber, "+")
	}
	fromHost := e.publicHost

	req := sip.NewRequest(sip.INVITE, recipient)
	// sipgo picks UDP unless transport is forced; sips: alone only
	// upgrades TCP→TLS.
	req.SetTransport("TLS")
	req.SetBody(opts.SDPOffer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: fromURIUser, Host: fromHost}}
	from.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(from)

	contactURI := sip.Uri{Scheme: "sip", User: fromURIUser, Host: fromHost, Port: e.tlsPort}
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

	waitErr := ds.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: digestUser,
		Password: opts.Password,
		OnResponse: func(res *sip.Response) error {
			e.logSIPMessage("inbound", res)
			return nil
		},
	})
	// sipgo mutates ds.InviteRequest in place to handle 401/407 retries.
	// Log the final shape so SIP_DEBUG sees the auth-bearing request.
	if ds.InviteRequest != nil &&
		(ds.InviteRequest.GetHeader("Authorization") != nil ||
			ds.InviteRequest.GetHeader("Proxy-Authorization") != nil) {
		e.logSIPMessage("outbound (post-auth)", ds.InviteRequest)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("wait answer: %w", waitErr)
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

// WhatsAppRecipientURI builds the Request-URI for an outbound call. Meta
// uses "sip:+E164@wa.meta.vc;transport=tls" — using "sips:" returns 404
// because their internal routing isn't strict-TLS end-to-end.
func WhatsAppRecipientURI(toUser string) sip.Uri {
	uri := sip.Uri{
		Scheme: "sip",
		User:   "+" + strings.TrimPrefix(toUser, "+"),
		Host:   WhatsAppOutboundHost,
		Port:   5061,
	}
	uri.UriParams = sip.NewParams()
	uri.UriParams.Add("transport", "tls")
	return uri
}
