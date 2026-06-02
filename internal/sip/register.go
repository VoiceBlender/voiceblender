package sip

import (
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// handleRegister processes inbound SIP REGISTER per RFC 3261 §10.3. The
// registrar is auto-approving — every REGISTER from a supported transport
// is accepted and the AOR is bound to the request's source socket. Auth is
// assumed to be enforced by a SIP proxy in front of VoiceBlender.
func (e *Engine) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	if e.registrar == nil {
		e.respondRegister(tx, req, sip.StatusNotImplemented, "Registrar not enabled", nil, nil)
		return
	}

	transport := strings.ToLower(req.Transport())
	switch transport {
	case "udp", "tcp", "tls":
		// supported
	default:
		warn := sip.NewHeader("Warning", `399 voiceblender "transport not supported"`)
		e.respondRegister(tx, req, sip.StatusServiceUnavailable, "Transport Not Supported", nil, []sip.Header{warn})
		return
	}

	toHdr := req.To()
	if toHdr == nil {
		e.respondRegister(tx, req, sip.StatusBadRequest, "Missing To", nil, nil)
		return
	}
	aor := CanonicalizeAOR(toHdr.Address)
	if aor == "" {
		e.respondRegister(tx, req, sip.StatusBadRequest, "Invalid To URI", nil, nil)
		return
	}

	callID := ""
	if c := req.CallID(); c != nil {
		callID = c.Value()
	}
	userAgent := ""
	if ua := req.GetHeader("User-Agent"); ua != nil {
		userAgent = ua.Value()
	}

	contacts := req.GetHeaders("Contact")
	headerExpires := -1
	if ex := req.GetHeader("Expires"); ex != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(ex.Value())); err == nil {
			headerExpires = n
		}
	}

	// Query REGISTER (no Contact header): reply 200 OK with the current bindings.
	if len(contacts) == 0 {
		e.respondRegister(tx, req, sip.StatusOK, "OK", e.registrar.LookupAll(aor), nil)
		return
	}

	// Full de-register: a single "Contact: *" with Expires: 0.
	if len(contacts) == 1 && isWildcardContact(contacts[0]) {
		if headerExpires != 0 {
			e.respondRegister(tx, req, sip.StatusBadRequest, "Wildcard requires Expires:0", nil, nil)
			return
		}
		e.registrar.UnbindAll(aor, "unregistered")
		e.respondRegister(tx, req, sip.StatusOK, "OK", nil, nil)
		return
	}

	source := req.Source()
	now := time.Now()

	for _, ch := range contacts {
		uri, contactExpires, err := parseContactHeader(ch)
		if err != nil {
			e.log.Warn("REGISTER: invalid Contact", "error", err, "value", ch.Value())
			e.respondRegister(tx, req, sip.StatusBadRequest, "Invalid Contact", nil, nil)
			return
		}
		expires := contactExpires
		if expires < 0 {
			expires = headerExpires
		}
		contactStr := uri.String()
		if expires == 0 {
			e.registrar.UnbindContact(aor, contactStr, "unregistered")
			continue
		}
		granted := e.registrar.ClampExpires(expires)
		e.registrar.Bind(Binding{
			AOR:            aor,
			Contact:        contactStr,
			Socket:         source,
			Transport:      transport,
			UserAgent:      userAgent,
			CallID:         callID,
			ExpiresAt:      now.Add(time.Duration(granted) * time.Second),
			GrantedExpires: granted,
		})
	}

	e.respondRegister(tx, req, sip.StatusOK, "OK", e.registrar.LookupAll(aor), nil)
}

// respondRegister builds and sends a REGISTER response. When `bindings` is
// non-nil, it echoes one Contact header per binding with the remaining
// `expires=` parameter so the UA can confirm the registrar's view.
func (e *Engine) respondRegister(tx sip.ServerTransaction, req *sip.Request, statusCode int, reason string, bindings []Binding, extra []sip.Header) {
	res := sip.NewResponseFromRequest(req, statusCode, reason, nil)
	res.AppendHeader(e.ServerHeader())
	res.AppendHeader(sip.NewHeader("Date", time.Now().UTC().Format(time.RFC1123)))
	for _, b := range bindings {
		remaining := int(time.Until(b.ExpiresAt).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		v := b.Contact + ";expires=" + strconv.Itoa(remaining)
		res.AppendHeader(sip.NewHeader("Contact", v))
	}
	for _, h := range extra {
		res.AppendHeader(h)
	}
	e.pinDestinationToSource(req, res)
	e.logSIPMessage("outbound", res)
	if err := tx.Respond(res); err != nil {
		e.log.Error("REGISTER: respond failed", "error", err, "status", statusCode)
	}
}

// isWildcardContact returns true for `Contact: *` (which de-registers all
// bindings for the AOR when paired with Expires: 0).
func isWildcardContact(h sip.Header) bool {
	return strings.TrimSpace(h.Value()) == "*"
}

// parseContactHeader extracts the Contact URI and its `expires=` param.
// The returned expires is -1 when the param is absent.
func parseContactHeader(h sip.Header) (sip.Uri, int, error) {
	raw := strings.TrimSpace(h.Value())
	// Split URI from params. The URI may be enclosed in angle brackets;
	// outside brackets, parameters are separated from the URI by ';'.
	var uriPart, paramsPart string
	if strings.HasPrefix(raw, "<") {
		end := strings.Index(raw, ">")
		if end < 0 {
			return sip.Uri{}, 0, errInvalidContact
		}
		uriPart = raw[1:end]
		paramsPart = raw[end+1:]
	} else if idx := strings.Index(raw, ";"); idx >= 0 {
		uriPart = raw[:idx]
		paramsPart = raw[idx:]
	} else {
		uriPart = raw
	}

	var u sip.Uri
	if err := sip.ParseUri(strings.TrimSpace(uriPart), &u); err != nil {
		return sip.Uri{}, 0, err
	}

	expires := -1
	for _, p := range strings.Split(paramsPart, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if eq := strings.Index(p, "="); eq > 0 {
			name := strings.ToLower(strings.TrimSpace(p[:eq]))
			val := strings.TrimSpace(p[eq+1:])
			if name == "expires" {
				if n, err := strconv.Atoi(val); err == nil {
					expires = n
				}
			}
		}
	}
	return u, expires, nil
}

type contactErr string

func (e contactErr) Error() string { return string(e) }

const errInvalidContact = contactErr("invalid Contact: missing '>'")
