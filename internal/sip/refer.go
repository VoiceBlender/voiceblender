package sip

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ReplacesParams identifies the dialog of an existing call so a SIP REFER
// recipient can construct an INVITE with a Replaces header (RFC 3891) — the
// mechanism behind attended transfer.
type ReplacesParams struct {
	CallID  string // dialog Call-ID
	ToTag   string // dialog "remote" tag (To from caller's view)
	FromTag string // dialog "local" tag  (From from caller's view)
}

// String renders the Replaces value the way it appears as a URI parameter
// inside a Refer-To header: callid;to-tag=...;from-tag=...
func (p *ReplacesParams) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%s;to-tag=%s;from-tag=%s", p.CallID, p.ToTag, p.FromTag)
}

// SendRefer sends a REFER request inside an existing dialog. dialog must be
// either *sipgo.DialogServerSession or *sipgo.DialogClientSession (same
// constraint as SendReInvite). On success the peer has accepted the
// transfer (202 Accepted) and will emit NOTIFY sipfrag updates for the
// transfer subscription created implicitly by RFC 3515.
func (e *Engine) SendRefer(ctx context.Context, dialog interface{}, referTo string, replaces *ReplacesParams) error {
	// Build the Refer-To value. If a Replaces parameter is supplied it
	// goes inside the URI as a header (?Replaces=...) per RFC 3891 §4.
	target := referTo
	if replaces != nil {
		// URL-encode the embedded URI parameter; semicolons inside the
		// Replaces value would otherwise terminate the URI parameters.
		target = fmt.Sprintf("%s?Replaces=%s", referTo, url.QueryEscape(replaces.String()))
	}
	referToHdr := sip.NewHeader("Refer-To", "<"+target+">")

	switch d := dialog.(type) {
	case *sipgo.DialogServerSession:
		req := sip.NewRequest(sip.REFER, d.InviteRequest.Contact().Address)
		req.AppendHeader(referToHdr)
		req.AppendHeader(sip.NewHeader("Referred-By", "<sip:"+e.bindIP+">"))
		res, err := d.Do(ctx, req)
		if err != nil {
			return fmt.Errorf("REFER Do: %w", err)
		}
		if res.StatusCode != sip.StatusAccepted {
			return fmt.Errorf("REFER rejected: %d %s", res.StatusCode, res.Reason)
		}
		return nil
	case *sipgo.DialogClientSession:
		req := sip.NewRequest(sip.REFER, d.InviteResponse.Contact().Address)
		req.AppendHeader(referToHdr)
		req.AppendHeader(sip.NewHeader("Referred-By", "<sip:"+e.bindIP+">"))
		res, err := d.Do(ctx, req)
		if err != nil {
			return fmt.Errorf("REFER Do: %w", err)
		}
		if res.StatusCode != sip.StatusAccepted {
			return fmt.Errorf("REFER rejected: %d %s", res.StatusCode, res.Reason)
		}
		return nil
	default:
		return fmt.Errorf("unsupported dialog type: %T", dialog)
	}
}

// SendNotifySipfrag sends an in-dialog NOTIFY for a "refer" subscription.
// statusCode/reason are formatted as a sipfrag body ("SIP/2.0 200 OK").
// terminated=true marks the subscription as terminated (final NOTIFY).
func (e *Engine) SendNotifySipfrag(ctx context.Context, dialog interface{}, statusCode int, reason string, terminated bool) error {
	subState := "active;expires=60"
	if terminated {
		subState = "terminated;reason=noresource"
	}
	body := []byte(fmt.Sprintf("SIP/2.0 %d %s\r\n", statusCode, reason))

	build := func(target sip.Uri) *sip.Request {
		req := sip.NewRequest(sip.NOTIFY, target)
		req.AppendHeader(sip.NewHeader("Event", "refer"))
		req.AppendHeader(sip.NewHeader("Subscription-State", subState))
		req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
		req.SetBody(body)
		return req
	}

	switch d := dialog.(type) {
	case *sipgo.DialogServerSession:
		req := build(d.InviteRequest.Contact().Address)
		_, err := d.Do(ctx, req)
		return err
	case *sipgo.DialogClientSession:
		req := build(d.InviteResponse.Contact().Address)
		_, err := d.Do(ctx, req)
		return err
	default:
		return fmt.Errorf("unsupported dialog type: %T", dialog)
	}
}

// ParseReferTo extracts the bare URI and (when present) the Replaces
// parameters from a Refer-To header value of the form
//
//	<sip:bob@host>
//	<sip:bob@host?Replaces=callid%3Bto-tag%3Dxx%3Bfrom-tag%3Dyy>
//
// The function is permissive about angle brackets and percent-encoding.
func ParseReferTo(value string) (string, *ReplacesParams, error) {
	v := strings.TrimSpace(value)
	v = strings.TrimPrefix(v, "<")
	if i := strings.Index(v, ">"); i >= 0 {
		v = v[:i]
	}
	uri, raw, hasParams := strings.Cut(v, "?")
	if !hasParams {
		return uri, nil, nil
	}
	// Refer-To URI parameters are application/x-www-form-urlencoded-like:
	// header=value pairs separated by '&'.
	for _, pair := range strings.Split(raw, "&") {
		k, val, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if !strings.EqualFold(k, "Replaces") {
			continue
		}
		decoded, err := url.QueryUnescape(val)
		if err != nil {
			return uri, nil, fmt.Errorf("Refer-To Replaces decode: %w", err)
		}
		// callid;to-tag=...;from-tag=...
		parts := strings.Split(decoded, ";")
		rp := &ReplacesParams{CallID: parts[0]}
		for _, p := range parts[1:] {
			pk, pv, ok := strings.Cut(p, "=")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(pk)) {
			case "to-tag":
				rp.ToTag = pv
			case "from-tag":
				rp.FromTag = pv
			}
		}
		return uri, rp, nil
	}
	return uri, nil, nil
}

// ParseSipfrag extracts the status line of a sipfrag body. Returns
// (statusCode, reasonPhrase) or (0, "") if the body cannot be parsed.
func ParseSipfrag(body []byte) (int, string) {
	line := string(body)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	// "SIP/2.0 200 OK"
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "SIP/") {
		return 0, ""
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, ""
	}
	reason := ""
	if len(parts) == 3 {
		reason = parts[2]
	}
	return code, reason
}
