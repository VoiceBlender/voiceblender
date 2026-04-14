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

// ReplacesParams carries the Replaces header dialog identity (RFC 3891).
type ReplacesParams struct {
	CallID  string
	ToTag   string
	FromTag string
}

// String formats as the Replaces value embedded in a Refer-To URI.
func (p *ReplacesParams) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%s;to-tag=%s;from-tag=%s", p.CallID, p.ToTag, p.FromTag)
}

// SendRefer sends an in-dialog REFER, returning nil on 202 Accepted.
func (e *Engine) SendRefer(ctx context.Context, dialog interface{}, referTo string, replaces *ReplacesParams) error {
	target := referTo
	if replaces != nil {
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

// SendNotifySipfrag sends a "refer" subscription NOTIFY with a sipfrag body.
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

// ParseReferTo extracts the bare URI and (optional) Replaces from a Refer-To.
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

// ParseSipfrag returns (statusCode, reason) from a sipfrag status line, or (0,"").
func ParseSipfrag(body []byte) (int, string) {
	line := string(body)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
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
