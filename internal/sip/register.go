package sip

import (
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// RegisterDecisionKind is the action a VSI/REST client chooses for an inbound
// REGISTER it was consulted about.
type RegisterDecisionKind int

const (
	RegisterAccept    RegisterDecisionKind = iota // bind and reply 200 OK
	RegisterChallenge                             // reply 401 with a digest challenge
	RegisterReject                                // reply with a non-2xx (default 403)
)

// RegisterDecision is returned by the OnRegisterAttempt callback.
type RegisterDecision struct {
	Kind         RegisterDecisionKind
	Challenge    ChallengeParams // used when Kind == RegisterChallenge
	RejectCode   int             // used when Kind == RegisterReject; 0 => 403
	RejectReason string          // used when Kind == RegisterReject; "" => "Forbidden"
	// MaxExpires, when > 0, caps the granted binding TTL (seconds) for a REGISTER
	// this decision admits (accept, or the credentialed retry of a challenge). It
	// only ever shortens: the effective grant is min(ClampExpires(requested),
	// MaxExpires), still subject to the 60s floor. 0 leaves the registrar's normal
	// clamp in force.
	MaxExpires int
}

// RegisterAttempt describes an inbound REGISTER surfaced to the decision
// callback so the client can decide whether to challenge it (e.g. based on the
// source address).
type RegisterAttempt struct {
	Request   *sip.Request
	AOR       string
	Contact   string
	Source    string
	Transport string
	UserAgent string
	CallID    string
	HasAuth   bool
}

// handleRegister processes inbound SIP REGISTER per RFC 3261 §10.3. A REGISTER
// that creates or removes a binding is first passed to the OnRegisterAttempt
// decision callback (when registered), which may accept it, challenge it with a
// 401 digest challenge, or reject it. With no callback wired the registrar is
// auto-approving — every REGISTER from a supported transport is accepted and
// the AOR is bound to the request's source socket.
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

	// Query REGISTER (no Contact header): reply 200 OK with the current
	// bindings. A pure query never binds, so it is not subject to the auth gate.
	if len(contacts) == 0 {
		e.respondRegister(tx, req, sip.StatusOK, "OK", e.registrar.LookupAll(aor), nil)
		return
	}

	// Authentication gate: verify a credentialed retry, or consult the client
	// for a challenge/accept/reject decision. A 401/403 is sent inside when the
	// REGISTER is not authorized to proceed. maxExpires (>0) caps the granted
	// binding TTL for this REGISTER when the admitting decision requested it.
	proceed, maxExpires := e.authorizeRegister(req, tx, aor)
	if !proceed {
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
		if maxExpires > 0 && maxExpires < granted {
			granted = maxExpires
		}
		if granted < 60 { // the 60s floor holds even against an explicit cap
			granted = 60
		}
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

// authorizeRegister applies the inbound-REGISTER auth gate. proceed is true
// when the REGISTER may bind; otherwise it has already sent the appropriate 401
// (challenge) or 4xx (reject/forbidden) response. maxExpires (>0) is the TTL cap
// carried by the admitting decision — recovered from the pending challenge on a
// credentialed retry, or taken directly from an accept decision. A credentialed
// retry is verified against the issued challenge first; absent valid
// credentials, the OnRegisterAttempt callback is consulted.
func (e *Engine) authorizeRegister(req *sip.Request, tx sip.ServerTransaction, aor string) (proceed bool, maxExpires int) {
	hasAuth := req.GetHeader("Authorization") != nil
	if hasAuth {
		switch res, _, override := e.VerifyInboundAuth(req, sip.REGISTER.String()); res {
		case AuthValid:
			return true, override
		case AuthInvalid:
			e.respondRegister(tx, req, sip.StatusForbidden, "Forbidden", nil, nil)
			return false, 0
		}
		// AuthNone (no live challenge matched) falls through to a fresh consult.
	}

	if e.onRegisterAttempt == nil {
		return true, 0
	}

	contact := ""
	if c := req.GetHeader("Contact"); c != nil {
		contact = c.Value()
	}
	userAgent := ""
	if ua := req.GetHeader("User-Agent"); ua != nil {
		userAgent = ua.Value()
	}
	decision := e.onRegisterAttempt(&RegisterAttempt{
		Request:   req,
		AOR:       aor,
		Contact:   contact,
		Source:    req.Source(),
		Transport: strings.ToLower(req.Transport()),
		UserAgent: userAgent,
		CallID:    callIDOf(req),
		HasAuth:   hasAuth,
	})

	switch decision.Kind {
	case RegisterChallenge:
		val := e.recordChallenge(callIDOf(req), decision.Challenge, decision.MaxExpires)
		e.respondRegister(tx, req, sip.StatusUnauthorized, "Unauthorized", nil,
			[]sip.Header{sip.NewHeader("WWW-Authenticate", val)})
		return false, 0
	case RegisterReject:
		code := decision.RejectCode
		if code == 0 {
			code = sip.StatusForbidden
		}
		reason := decision.RejectReason
		if reason == "" {
			reason = "Forbidden"
		}
		e.respondRegister(tx, req, code, reason, nil, nil)
		return false, 0
	default:
		return true, decision.MaxExpires
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
