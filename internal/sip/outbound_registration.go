package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// OutboundRegistrationConfig holds tunables shared across every
// sip_register trunk.
type OutboundRegistrationConfig struct {
	DefaultExpiresSeconds int
	MinExpiresSeconds     int
	MaxExpiresSeconds     int
	RefreshRatio          float64
	FailureBackoffMax     time.Duration
}

func (c OutboundRegistrationConfig) withDefaults() OutboundRegistrationConfig {
	if c.DefaultExpiresSeconds <= 0 {
		c.DefaultExpiresSeconds = 3600
	}
	if c.MinExpiresSeconds <= 0 {
		c.MinExpiresSeconds = 60
	}
	if c.MaxExpiresSeconds <= 0 {
		c.MaxExpiresSeconds = 7200
	}
	if c.RefreshRatio <= 0 || c.RefreshRatio >= 1 {
		c.RefreshRatio = 0.5
	}
	if c.FailureBackoffMax <= 0 {
		c.FailureBackoffMax = 5 * time.Minute
	}
	return c
}

// OutboundRegistrationParams is the per-trunk creation payload (the values
// that survive validation of a CreateTrunkRequest of type sip_register).
type OutboundRegistrationParams struct {
	ID                      string
	AppID                   string
	RegistrarURI            sip.Uri
	AOR                     sip.Uri
	Username                string
	Password                string
	ContactUser             string
	RequestedExpiresSeconds int
}

// OutboundRegistration is the sip_register Trunk implementation. One
// instance per trunk; safe for concurrent access.
type OutboundRegistration struct {
	cfg    OutboundRegistrationConfig
	engine *Engine
	bus    *events.Bus
	log    *slog.Logger

	id           string
	appID        string
	registrarURI sip.Uri
	aor          sip.Uri
	username     string
	password     string
	contactUser  string

	requestedExpires int

	mu             sync.RWMutex
	status         TrunkStatus
	lastError      string
	createdAt      time.Time
	lastRegistered time.Time
	nextRefresh    time.Time
	grantedExpires int
	callID         string
	cseq           uint32
	peerHost       string
	peerPort       int
	peerTransport  string
	// expiredEmitted guards the one-shot `sip.outbound_registration_expired`
	// (reason: refresh_failed) event so it fires once per outage instead of
	// on every retry. Reset to false on every successful REGISTER.
	expiredEmitted bool

	cancelLoop context.CancelFunc
	loopDone   chan struct{}
}

// NewOutboundRegistration constructs a trunk in the registering state.
// Call Start to launch the background lifecycle.
func NewOutboundRegistration(engine *Engine, bus *events.Bus, log *slog.Logger, cfg OutboundRegistrationConfig, p OutboundRegistrationParams) *OutboundRegistration {
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.withDefaults()

	expires := p.RequestedExpiresSeconds
	if expires <= 0 {
		expires = cfg.DefaultExpiresSeconds
	}
	if expires < cfg.MinExpiresSeconds {
		expires = cfg.MinExpiresSeconds
	}
	if expires > cfg.MaxExpiresSeconds {
		expires = cfg.MaxExpiresSeconds
	}

	contactUser := p.ContactUser
	if contactUser == "" {
		contactUser = p.AOR.User
	}
	username := p.Username
	if username == "" {
		username = p.AOR.User
	}

	r := &OutboundRegistration{
		cfg:              cfg,
		engine:           engine,
		bus:              bus,
		log:              log.With("trunk_id", p.ID, "aor", CanonicalizeAOR(p.AOR)),
		id:               p.ID,
		appID:            p.AppID,
		registrarURI:     p.RegistrarURI,
		aor:              p.AOR,
		username:         username,
		password:         p.Password,
		contactUser:      contactUser,
		requestedExpires: expires,
		status:           TrunkStatusRegistering,
		createdAt:        time.Now(),
		callID:           sip.GenerateTagN(16) + "@" + engineHostOrFallback(engine),
	}
	r.computePeerSocket()
	return r
}

func engineHostOrFallback(e *Engine) string {
	if e == nil {
		return "voiceblender"
	}
	if e.publicHost != "" {
		return e.publicHost
	}
	return "voiceblender"
}

// computePeerSocket derives host/port/transport from the registrar URI; used
// for initial PeerSocket indexing before the first REGISTER reveals the
// real upstream source.
func (r *OutboundRegistration) computePeerSocket() {
	r.peerHost = r.registrarURI.Host
	r.peerPort = r.registrarURI.Port
	if r.peerPort == 0 {
		if strings.EqualFold(r.registrarURI.Scheme, "sips") {
			r.peerPort = 5061
		} else {
			r.peerPort = 5060
		}
	}
	if t, ok := r.registrarURI.UriParams.Get("transport"); ok {
		r.peerTransport = strings.ToLower(t)
	} else if strings.EqualFold(r.registrarURI.Scheme, "sips") {
		r.peerTransport = "tls"
	} else {
		r.peerTransport = "udp"
	}
}

// --- Trunk interface ---

func (r *OutboundRegistration) ID() string      { return r.id }
func (r *OutboundRegistration) Type() TrunkType { return TrunkTypeSIPRegister }
func (r *OutboundRegistration) AOR() string     { return CanonicalizeAOR(r.aor) }
func (r *OutboundRegistration) AppID() string   { return r.appID }

func (r *OutboundRegistration) PeerSocket() (host string, port int, transport string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.peerHost, r.peerPort, r.peerTransport
}

// RegistrarURI exposes the configured upstream registrar URI; used by the
// outbound INVITE path to attach a Route header.
func (r *OutboundRegistration) RegistrarURI() sip.Uri { return r.registrarURI }

// Credentials returns the trunk's digest username/password. Used by the
// outbound INVITE path; never exposed over the API.
func (r *OutboundRegistration) Credentials() (string, string) {
	return r.username, r.password
}

// Snapshot returns the current TrunkView. Safe to call concurrently.
func (r *OutboundRegistration) Snapshot() TrunkView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	contactURI := sip.Uri{
		Scheme: schemeForTransport(r.peerTransport),
		User:   r.contactUser,
		Host:   r.engine.publicHost,
		Port:   r.engine.bindPort,
	}
	if strings.EqualFold(r.peerTransport, "tls") && r.engine.tlsPort != 0 {
		contactURI.Port = r.engine.tlsPort
	}
	view := TrunkView{
		ID:        r.id,
		Type:      TrunkTypeSIPRegister,
		AppID:     r.appID,
		Status:    r.status,
		LastError: r.lastError,
		CreatedAt: r.createdAt.UTC().Format(time.RFC3339),
		SIPRegister: &SIPRegisterTrunkView{
			RegistrarURI:            r.registrarURI.String(),
			AOR:                     CanonicalizeAOR(r.aor),
			Username:                r.username,
			ContactURI:              contactURI.String(),
			RequestedExpiresSeconds: r.requestedExpires,
			GrantedExpiresSeconds:   r.grantedExpires,
			CallID:                  r.callID,
			CSeq:                    r.cseq,
			SourceAddress:           socketKey(r.peerHost, r.peerPort),
		},
	}
	if !r.lastRegistered.IsZero() {
		view.SIPRegister.LastRegisteredAt = r.lastRegistered.UTC().Format(time.RFC3339)
	}
	if !r.nextRefresh.IsZero() {
		view.SIPRegister.NextRefreshAt = r.nextRefresh.UTC().Format(time.RFC3339)
	}
	return view
}

func schemeForTransport(transport string) string {
	if strings.EqualFold(transport, "tls") {
		return "sips"
	}
	return "sip"
}

// Start launches the background register-and-refresh loop. Calling Start
// more than once is a no-op.
func (r *OutboundRegistration) Start(ctx context.Context) {
	r.mu.Lock()
	if r.cancelLoop != nil {
		r.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	r.cancelLoop = cancel
	r.loopDone = make(chan struct{})
	r.mu.Unlock()
	go r.run(loopCtx)
}

// run drives one REGISTER, then sleeps until the next refresh, repeating
// until the loop context is cancelled. Failures are reported via events and
// retried with exponential backoff.
func (r *OutboundRegistration) run(ctx context.Context) {
	defer close(r.loopDone)
	backoff := time.Second
	for {
		err := r.registerOnce(ctx, r.requestedExpires)
		var wait time.Duration
		if err != nil {
			r.log.Warn("REGISTER failed", "error", err)
			backoff *= 2
			if backoff > r.cfg.FailureBackoffMax {
				backoff = r.cfg.FailureBackoffMax
			}
			wait = backoff
		} else {
			backoff = time.Second
			r.mu.RLock()
			granted := r.grantedExpires
			r.mu.RUnlock()
			refreshSeconds := float64(granted) * r.cfg.RefreshRatio
			if refreshSeconds < 1 {
				refreshSeconds = 1
			}
			wait = time.Duration(refreshSeconds * float64(time.Second))
			r.mu.Lock()
			r.nextRefresh = time.Now().Add(wait)
			r.mu.Unlock()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// registerOnce sends a single REGISTER (with optional digest retry) for the
// given Expires value. Updates state + emits events.
func (r *OutboundRegistration) registerOnce(ctx context.Context, expires int) error {
	res, err := r.sendRegister(ctx, expires, "", "")
	if err != nil {
		r.markFailed(0, err.Error())
		return err
	}

	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		// Build the auth header from the challenge, then send a fresh REGISTER.
		authHeaderName := "Authorization"
		challengeHdr := res.GetHeader("WWW-Authenticate")
		if res.StatusCode == sip.StatusProxyAuthRequired {
			authHeaderName = "Proxy-Authorization"
			challengeHdr = res.GetHeader("Proxy-Authenticate")
		}
		if challengeHdr == nil {
			r.markFailed(int(res.StatusCode), "missing challenge header")
			return fmt.Errorf("REGISTER %d but no challenge header", res.StatusCode)
		}
		credValue, err := r.buildDigestResponse(challengeHdr.Value())
		if err != nil {
			r.markFailed(int(res.StatusCode), "digest: "+err.Error())
			return err
		}
		res, err = r.sendRegister(ctx, expires, authHeaderName, credValue)
		if err != nil {
			r.markFailed(res.StatusCode, "digest retry: "+err.Error())
			return err
		}
	}

	if !res.IsSuccess() {
		r.markFailed(int(res.StatusCode), res.Reason)
		return fmt.Errorf("REGISTER rejected: %d %s", res.StatusCode, res.Reason)
	}

	granted := parseGrantedExpires(res, expires)

	// Capture the actual upstream source socket from the response so that
	// inbound INVITEs delivered from this peer can be tagged with the
	// trunk id even when the source port differs from the configured port.
	srcHost, srcPort := extractSource(res.Source())

	r.mu.Lock()
	r.status = TrunkStatusActive
	r.lastError = ""
	r.lastRegistered = time.Now()
	r.grantedExpires = granted
	r.expiredEmitted = false
	if srcHost != "" {
		r.peerHost = srcHost
		if srcPort > 0 {
			r.peerPort = srcPort
		}
	}
	r.mu.Unlock()

	if r.bus != nil {
		r.mu.RLock()
		sourceAddr := socketKey(r.peerHost, r.peerPort)
		r.mu.RUnlock()
		r.bus.Publish(events.SIPOutboundRegistrationActive, &events.SIPOutboundRegistrationActiveData{
			SIPRegistrationScope:  events.SIPRegistrationScope{AppID: r.appID},
			TrunkID:               r.id,
			AOR:                   CanonicalizeAOR(r.aor),
			Registrar:             r.registrarURI.String(),
			Contact:               r.contactString(),
			GrantedExpiresSeconds: granted,
			ExpiresAt:             time.Now().Add(time.Duration(granted) * time.Second).UTC().Format(time.RFC3339),
			CallID:                r.callID,
			SourceAddress:         sourceAddr,
		})
	}
	r.log.Info("REGISTER accepted", "granted_expires", granted)
	return nil
}

// markFailed transitions to TrunkStatusFailed and emits the failed event.
// When the previously-granted upstream lifetime has now lapsed (and we
// haven't said so yet during this outage), also emits a one-shot
// `sip.outbound_registration_expired` with reason `refresh_failed` so
// observers know the binding is definitely dead at the registrar even
// though VoiceBlender is still retrying.
func (r *OutboundRegistration) markFailed(statusCode int, reason string) {
	r.mu.Lock()
	r.status = TrunkStatusFailed
	r.lastError = reason
	// Detect that the upstream binding has lapsed (we had a successful
	// REGISTER at lastRegistered with grantedExpires seconds of lifetime,
	// and that window has now closed). Emit once per outage.
	var emitExpired bool
	if !r.expiredEmitted && !r.lastRegistered.IsZero() && r.grantedExpires > 0 {
		deadline := r.lastRegistered.Add(time.Duration(r.grantedExpires) * time.Second)
		if time.Now().After(deadline) {
			r.expiredEmitted = true
			emitExpired = true
		}
	}
	r.mu.Unlock()

	if r.bus == nil {
		return
	}
	r.bus.Publish(events.SIPOutboundRegistrationFailed, &events.SIPOutboundRegistrationFailedData{
		SIPRegistrationScope: events.SIPRegistrationScope{AppID: r.appID},
		TrunkID:              r.id,
		AOR:                  CanonicalizeAOR(r.aor),
		Registrar:            r.registrarURI.String(),
		StatusCode:           statusCode,
		Reason:               reason,
	})
	if emitExpired {
		r.bus.Publish(events.SIPOutboundRegistrationExpired, &events.SIPOutboundRegistrationExpiredData{
			SIPRegistrationScope: events.SIPRegistrationScope{AppID: r.appID},
			TrunkID:              r.id,
			AOR:                  CanonicalizeAOR(r.aor),
			Registrar:            r.registrarURI.String(),
			Reason:               "refresh_failed",
		})
	}
}

// Stop cancels the refresh loop and sends a final REGISTER with Expires: 0.
// Best-effort; honours ctx.
func (r *OutboundRegistration) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.cancelLoop != nil {
		r.cancelLoop()
		r.cancelLoop = nil
	}
	loopDone := r.loopDone
	r.status = TrunkStatusUnregistering
	r.mu.Unlock()

	if loopDone != nil {
		select {
		case <-loopDone:
		case <-ctx.Done():
		}
	}

	// Best-effort unregister; ignore error but log.
	err := r.unregister(ctx)
	if err != nil {
		r.log.Warn("unregister failed", "error", err)
	}

	r.mu.Lock()
	r.status = TrunkStatusExpired
	r.mu.Unlock()

	if r.bus != nil {
		reason := "unregistered"
		if err != nil {
			reason = "shutdown"
		}
		r.bus.Publish(events.SIPOutboundRegistrationExpired, &events.SIPOutboundRegistrationExpiredData{
			SIPRegistrationScope: events.SIPRegistrationScope{AppID: r.appID},
			TrunkID:              r.id,
			AOR:                  CanonicalizeAOR(r.aor),
			Registrar:            r.registrarURI.String(),
			Reason:               reason,
		})
	}
	return err
}

func (r *OutboundRegistration) unregister(ctx context.Context) error {
	res, err := r.sendRegister(ctx, 0, "", "")
	if err != nil {
		return err
	}
	if res.StatusCode == sip.StatusUnauthorized || res.StatusCode == sip.StatusProxyAuthRequired {
		authHeaderName := "Authorization"
		challengeHdr := res.GetHeader("WWW-Authenticate")
		if res.StatusCode == sip.StatusProxyAuthRequired {
			authHeaderName = "Proxy-Authorization"
			challengeHdr = res.GetHeader("Proxy-Authenticate")
		}
		if challengeHdr == nil {
			return fmt.Errorf("unregister: %d but no challenge header", res.StatusCode)
		}
		credValue, err := r.buildDigestResponse(challengeHdr.Value())
		if err != nil {
			return err
		}
		res, err = r.sendRegister(ctx, 0, authHeaderName, credValue)
		if err != nil {
			return err
		}
	}
	if !res.IsSuccess() {
		return fmt.Errorf("unregister rejected: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// sendRegister builds a REGISTER (optionally with a pre-computed digest
// Authorization header) and runs one full transaction against the
// registrar. Each call is a fresh transaction with a fresh Via — no state
// is shared between the initial and digest-retry requests.
func (r *OutboundRegistration) sendRegister(ctx context.Context, expires int, authHeaderName, authHeaderValue string) (*sip.Response, error) {
	req, err := r.buildRegister(expires)
	if err != nil {
		return nil, err
	}
	if authHeaderName != "" && authHeaderValue != "" {
		req.AppendHeader(sip.NewHeader(authHeaderName, authHeaderValue))
	}
	r.engine.logSIPMessage("outbound", req)
	res, err := r.engine.client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return nil, err
	}
	r.engine.logSIPMessage("inbound", res)
	return res, nil
}

// buildDigestResponse parses a WWW-Authenticate / Proxy-Authenticate
// challenge value and returns the matching Authorization header value
// computed from the trunk's credentials.
func (r *OutboundRegistration) buildDigestResponse(challengeValue string) (string, error) {
	chal, err := digest.ParseChallenge(challengeValue)
	if err != nil {
		return "", fmt.Errorf("parse challenge: %w", err)
	}
	// Fix lower-case algorithm (RFC permits any case but icholy/digest
	// expects upper).
	chal.Algorithm = strings.ToUpper(chal.Algorithm)
	regURI := r.registrarURI
	regURI.User = ""
	cred, err := digest.Digest(chal, digest.Options{
		Method:   sip.REGISTER.String(),
		URI:      regURI.Addr(),
		Username: r.username,
		Password: r.password,
	})
	if err != nil {
		return "", fmt.Errorf("compute digest: %w", err)
	}
	return cred.String(), nil
}

// buildRegister constructs a REGISTER request with stable Call-ID, the
// trunk's AOR in From/To, and the trunk's Contact pointing at our public
// socket. CSeq is set to the next value and reused by sipgo's
// ClientRequestRegisterBuild option.
func (r *OutboundRegistration) buildRegister(expiresSeconds int) (*sip.Request, error) {
	if r.engine == nil || r.engine.client == nil {
		return nil, fmt.Errorf("engine/client not initialised")
	}

	regURI := r.registrarURI
	regURI.User = ""
	req := sip.NewRequest(sip.REGISTER, regURI)
	transport := r.peerTransport
	if transport != "" && !strings.EqualFold(transport, "udp") {
		req.SetTransport(strings.ToUpper(transport))
	}

	from := &sip.FromHeader{Address: r.aor}
	from.Params = sip.NewParams()
	from.Params.Add("tag", sip.GenerateTagN(16))
	req.AppendHeader(from)

	to := &sip.ToHeader{Address: r.aor}
	to.Params = sip.NewParams()
	req.AppendHeader(to)

	callIDHdr := sip.CallIDHeader(r.callID)
	req.AppendHeader(&callIDHdr)

	r.mu.Lock()
	r.cseq++
	seq := r.cseq
	r.mu.Unlock()
	req.AppendHeader(&sip.CSeqHeader{SeqNo: uint32(seq), MethodName: sip.REGISTER})

	maxFwd := sip.MaxForwardsHeader(70)
	req.AppendHeader(&maxFwd)

	req.AppendHeader(&sip.ContactHeader{Address: r.contactURI()})
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(expiresSeconds)))
	req.AppendHeader(r.engine.AllowHeader())
	req.AppendHeader(r.engine.UserAgentHeader())

	return req, nil
}

func (r *OutboundRegistration) contactURI() sip.Uri {
	uri := sip.Uri{
		Scheme: "sip",
		User:   r.contactUser,
		Host:   r.engine.publicHost,
		Port:   r.engine.bindPort,
	}
	if strings.EqualFold(r.peerTransport, "tls") && r.engine.tlsPort != 0 {
		uri.Scheme = "sips"
		uri.Port = r.engine.tlsPort
		uri.UriParams = sip.NewParams()
		uri.UriParams.Add("transport", "tls")
	}
	return uri
}

func (r *OutboundRegistration) contactString() string {
	u := r.contactURI()
	return u.String()
}

// parseGrantedExpires picks the actual granted expiry from a 200 OK
// response. RFC 3261 §10.3 allows either a top-level Expires header or a
// per-Contact `expires=` param. Falls back to the requested value.
func parseGrantedExpires(res *sip.Response, requested int) int {
	if c := res.GetHeader("Contact"); c != nil {
		raw := c.Value()
		// look for `expires=N` (case-insensitive) anywhere in the value
		lower := strings.ToLower(raw)
		if idx := strings.Index(lower, "expires="); idx >= 0 {
			rest := raw[idx+len("expires="):]
			end := len(rest)
			for i, ch := range rest {
				if ch == ';' || ch == ',' || ch == ' ' {
					end = i
					break
				}
			}
			if n, err := strconv.Atoi(strings.TrimSpace(rest[:end])); err == nil && n > 0 {
				return n
			}
		}
	}
	if e := res.GetHeader("Expires"); e != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(e.Value())); err == nil && n > 0 {
			return n
		}
	}
	return requested
}

// extractSource splits a "host:port" source into host and port. Returns
// empty + 0 when the source is malformed.
func extractSource(src string) (string, int) {
	if src == "" {
		return "", 0
	}
	host, portStr, err := net.SplitHostPort(src)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}
