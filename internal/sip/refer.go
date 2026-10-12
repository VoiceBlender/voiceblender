package sip

import (
	"context"
	"fmt"
	"net/url"
	"sort"
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

// ReferOptions carries the optional headers of an outgoing REFER.
type ReferOptions struct {
	// ReferToHeaders are embedded in the Refer-To URI (see BuildReferTo).
	ReferToHeaders map[string]string
	// Headers are set on the REFER request itself.
	Headers map[string]string
}

// SendRefer sends an in-dialog REFER, returning nil on 202 Accepted.
func (e *Engine) SendRefer(ctx context.Context, dialog interface{}, referTo string, replaces *ReplacesParams, opts ReferOptions) error {
	hdrs := []sip.Header{sip.NewHeader("Refer-To", "<"+BuildReferTo(referTo, replaces, opts.ReferToHeaders)+">")}
	hdrs = append(hdrs, referRequestHeaders(e.bindIP, opts.Headers)...)

	var req *sip.Request
	var do func(context.Context, *sip.Request) (*sip.Response, error)
	switch d := dialog.(type) {
	case *sipgo.DialogServerSession:
		req = sip.NewRequest(sip.REFER, d.InviteRequest.Contact().Address)
		e.appendDialogContact(req, d)
		do = d.Do
	case *sipgo.DialogClientSession:
		req = sip.NewRequest(sip.REFER, d.InviteResponse.Contact().Address)
		e.appendDialogContact(req, d)
		do = d.Do
	default:
		return fmt.Errorf("unsupported dialog type: %T", dialog)
	}
	for _, h := range hdrs {
		req.AppendHeader(h)
	}
	res, err := do(ctx, req)
	if err != nil {
		return fmt.Errorf("REFER Do: %w", err)
	}
	if res.StatusCode != sip.StatusAccepted {
		return fmt.Errorf("REFER rejected: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// referRequestHeaders returns the headers a REFER carries besides Refer-To:
// the caller's own, sorted by name, plus a default Referred-By unless the
// caller supplied one.
func referRequestHeaders(bindIP string, headers map[string]string) []sip.Header {
	names := sortedNames(headers)
	out := make([]sip.Header, 0, len(names)+1)
	referredBy := false
	for _, name := range names {
		if l := strings.ToLower(name); l == "referred-by" || l == "b" {
			referredBy = true
		}
	}
	if !referredBy {
		out = append(out, sip.NewHeader("Referred-By", "<sip:"+bindIP+">"))
	}
	for _, name := range names {
		out = append(out, sip.NewHeader(name, headers[name]))
	}
	return out
}

// BuildReferTo returns the Refer-To URI: referTo plus Replaces (attended) and
// any extra headers as URI headers (RFC 3261 §19.1.1), which the referee
// copies into the INVITE it sends to the target (RFC 3515 §2.1). Headers are
// sorted by name so the URI is deterministic; headers already on referTo are
// kept.
func BuildReferTo(referTo string, replaces *ReplacesParams, headers map[string]string) string {
	var parts []string
	if replaces != nil {
		parts = append(parts, "Replaces="+url.QueryEscape(replaces.String()))
	}
	for _, name := range sortedNames(headers) {
		parts = append(parts, escapeURIHeader(name)+"="+escapeURIHeader(headers[name]))
	}
	if len(parts) == 0 {
		return referTo
	}
	sep := "?"
	if strings.Contains(referTo, "?") {
		sep = "&"
	}
	return referTo + sep + strings.Join(parts, "&")
}

func sortedNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// escapeURIHeader percent-encodes everything outside RFC 3261's "unreserved"
// set, so '&', '=', '?', '>', ';' and spaces inside a value can't break the
// header list or the surrounding <...>.
func escapeURIHeader(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAlphaNum(c) || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Lower-case names, compact forms included.
var (
	// Headers a referee should not honour from a URI (RFC 3261 §19.1.5), the
	// "body" pseudo-header, and Replaces, which only the attended flow sets.
	referToDeniedHeaders = headerSet(
		"from", "f", "to", "t", "call-id", "i", "cseq", "via", "v",
		"route", "record-route", "contact", "m", "max-forwards",
		"content-length", "l", "content-type", "c",
		"accept", "accept-encoding", "accept-language", "allow",
		"organization", "supported", "k", "user-agent",
		"body", "replaces",
	)
	// Headers the dialog layer owns on the REFER request.
	referDeniedHeaders = headerSet(
		"refer-to", "r", "from", "f", "to", "t", "call-id", "i", "cseq",
		"via", "v", "contact", "m", "max-forwards", "route", "record-route",
		"content-length", "l", "content-type", "c",
	)
)

func headerSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// ValidateReferToHeaders checks headers destined for the Refer-To URI.
func ValidateReferToHeaders(headers map[string]string) error {
	return validateHeaders(headers, referToDeniedHeaders)
}

// ValidateReferHeaders checks headers destined for the REFER request itself.
func ValidateReferHeaders(headers map[string]string) error {
	return validateHeaders(headers, referDeniedHeaders)
}

func validateHeaders(headers map[string]string, denied map[string]struct{}) error {
	seen := make(map[string]struct{}, len(headers))
	for _, name := range sortedNames(headers) {
		if !IsHeaderToken(name) {
			return fmt.Errorf("invalid header name %q", name)
		}
		lower := strings.ToLower(name)
		if _, dup := seen[lower]; dup {
			return fmt.Errorf("duplicate header %q", name)
		}
		seen[lower] = struct{}{}
		if _, bad := denied[lower]; bad {
			return fmt.Errorf("header %q is not allowed", name)
		}
		if hasControlChar(headers[name]) {
			return fmt.Errorf("header %q value contains control characters", name)
		}
	}
	return nil
}

func hasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return true
		}
	}
	return false
}

// IsHeaderToken reports whether name is a valid SIP header name (RFC 3261
// "token").
func IsHeaderToken(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isAlphaNum(c) && strings.IndexByte("-.!%*_+`'~", c) < 0 {
			return false
		}
	}
	return true
}

func isAlphaNum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
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
		e.appendDialogContact(req, d)
		_, err := d.Do(ctx, req)
		return err
	case *sipgo.DialogClientSession:
		req := build(d.InviteResponse.Contact().Address)
		e.appendDialogContact(req, d)
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
