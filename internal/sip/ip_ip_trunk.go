package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo/sip"
)

// defaultIPIPPingTimeout bounds one OPTIONS health check. sipgo would
// otherwise wait out the full 32s transaction timer on an unanswered request.
const defaultIPIPPingTimeout = 5 * time.Second

// IPIPTrunkParams is the per-trunk creation payload (the values that survive
// validation of a CreateTrunkRequest of type ip_ip).
type IPIPTrunkParams struct {
	ID      string
	AppID   string
	PeerURI sip.Uri
	// OutboundProxy, when set, is the next hop for this trunk's INVITEs and
	// OPTIONS. nil sends them at the peer.
	OutboundProxy *sip.Uri
	// AOR is the optional identity this trunk presents; it also lets an
	// outbound call select the trunk by its From.
	AOR      *sip.Uri
	Username string
	Password string
	// InboundSources are the addresses inbound INVITEs from this peer arrive
	// from, on top of the peer URI host when that is an IP literal.
	InboundSources []netip.Prefix
	// PingInterval enables the OPTIONS health check; zero disables it.
	PingInterval          time.Duration
	TLSInsecureSkipVerify bool
}

type trunkHealth int

const (
	trunkHealthUnknown trunkHealth = iota
	trunkHealthUp
	trunkHealthDown
)

// IPIPTrunk is the ip_ip Trunk implementation: a static peer, no REGISTER.
// One instance per trunk; safe for concurrent access.
type IPIPTrunk struct {
	engine *Engine
	bus    *events.Bus
	log    *slog.Logger

	id            string
	appID         string
	peerURI       sip.Uri
	outboundProxy *sip.Uri
	aor           *sip.Uri
	username      string
	password      string
	sources       []netip.Prefix
	// peerAddr is the peer URI host when it is an IP literal; the zero Addr
	// otherwise.
	peerAddr     netip.Addr
	peerPort     int
	pingInterval time.Duration
	pingTimeout  time.Duration
	createdAt    time.Time

	tlsInsecureSkipVerify bool
	trustedTLSHost        string
	untrustOnce           sync.Once

	mu             sync.RWMutex
	status         TrunkStatus
	lastError      string
	health         trunkHealth
	lastPingAt     time.Time
	lastPingStatus int
	cancelLoop     context.CancelFunc
	loopDone       chan struct{}
}

// NewIPIPTrunk constructs a trunk in the active state. Call Start to launch
// the OPTIONS health check, when one is configured.
func NewIPIPTrunk(engine *Engine, bus *events.Bus, log *slog.Logger, p IPIPTrunkParams) *IPIPTrunk {
	if log == nil {
		log = slog.Default()
	}
	username := p.Username
	if username == "" && p.Password != "" && p.AOR != nil {
		username = p.AOR.User
	}

	t := &IPIPTrunk{
		engine:                engine,
		bus:                   bus,
		log:                   log.With("trunk_id", p.ID, "peer", p.PeerURI.String()),
		id:                    p.ID,
		appID:                 p.AppID,
		peerURI:               p.PeerURI,
		outboundProxy:         p.OutboundProxy,
		aor:                   p.AOR,
		username:              username,
		password:              p.Password,
		sources:               append([]netip.Prefix(nil), p.InboundSources...),
		pingInterval:          p.PingInterval,
		pingTimeout:           defaultIPIPPingTimeout,
		createdAt:             time.Now(),
		tlsInsecureSkipVerify: p.TLSInsecureSkipVerify,
		status:                TrunkStatusActive,
	}

	host, port, _ := uriSocket(p.PeerURI)
	t.peerPort = port
	if addr, ok := parsePeerAddr(host); ok {
		t.peerAddr = addr.WithZone("")
		own := netip.PrefixFrom(t.peerAddr, t.peerAddr.BitLen())
		known := false
		for _, s := range t.sources {
			if s == own {
				known = true
				break
			}
		}
		if !known {
			t.sources = append(t.sources, own)
		}
	}

	if t.tlsInsecureSkipVerify {
		next := t.nextHopURI()
		_, _, transport := uriSocket(next)
		t.trustedTLSHost = engine.trustTLSNextHop(next, transport, t.log)
	}
	return t
}

// nextHopURI is where this trunk's requests are actually sent: the outbound
// proxy when configured, else the peer itself.
func (t *IPIPTrunk) nextHopURI() sip.Uri {
	if t.outboundProxy != nil {
		return *t.outboundProxy
	}
	return t.peerURI
}

// --- Trunk interface ---

func (t *IPIPTrunk) ID() string          { return t.id }
func (t *IPIPTrunk) Type() TrunkType     { return TrunkTypeIPIP }
func (t *IPIPTrunk) AppID() string       { return t.appID }
func (t *IPIPTrunk) ContactUser() string { return "" }

func (t *IPIPTrunk) AOR() string {
	if t.aor == nil {
		return ""
	}
	return CanonicalizeAOR(*t.aor)
}

// PeerSocket is the peer's own socket even behind an outbound proxy: a proxy
// shared by several trunks must not make each of them a candidate for
// everything it delivers.
func (t *IPIPTrunk) PeerSocket() (host string, port int, transport string) {
	return uriSocket(t.peerURI)
}

// OutboundRoute omits the Route when the Request-URI already targets the peer:
// a Route naming the hop being contacted is redundant and some peers mishandle
// it.
func (t *IPIPTrunk) OutboundRoute(recipient sip.Uri) OutboundRoute {
	r := OutboundRoute{
		ProxyURI:     t.outboundProxy,
		AuthUsername: t.username,
		AuthPassword: t.password,
	}
	if t.aor != nil {
		r.FromHost = t.aor.Host
	}
	if !sameSocket(recipient, t.peerURI) {
		peer := t.peerURI
		r.RouteURI = &peer
	}
	return r
}

// MatchInboundSource implements InboundSourceMatcher.
func (t *IPIPTrunk) MatchInboundSource(src netip.AddrPort) (bits int, exact bool, ok bool) {
	addr := src.Addr()
	for _, p := range t.sources {
		if p.Contains(addr) && (!ok || p.Bits() > bits) {
			bits, ok = p.Bits(), true
		}
	}
	if ok && t.peerAddr.IsValid() && addr == t.peerAddr && int(src.Port()) == t.peerPort {
		exact = true
	}
	return bits, exact, ok
}

// Snapshot returns the current TrunkView. Safe to call concurrently.
func (t *IPIPTrunk) Snapshot() TrunkView {
	sources := make([]string, 0, len(t.sources))
	for _, p := range t.sources {
		sources = append(sources, p.String())
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	view := TrunkView{
		ID:        t.id,
		Type:      TrunkTypeIPIP,
		AppID:     t.appID,
		Status:    t.status,
		LastError: t.lastError,
		CreatedAt: t.createdAt.UTC().Format(time.RFC3339),
		IPIP: &IPIPTrunkView{
			PeerURI:                    t.peerURI.String(),
			OutboundProxy:              proxyString(t.outboundProxy),
			AOR:                        t.AOR(),
			Username:                   t.username,
			InboundSources:             sources,
			OptionsPingIntervalSeconds: int(t.pingInterval / time.Second),
			LastPingStatusCode:         t.lastPingStatus,
			TLSInsecureSkipVerify:      t.tlsInsecureSkipVerify,
		},
	}
	if !t.lastPingAt.IsZero() {
		view.IPIP.LastPingAt = t.lastPingAt.UTC().Format(time.RFC3339)
	}
	return view
}

// Start launches the OPTIONS health check when one is configured. Calling
// Start more than once is a no-op.
func (t *IPIPTrunk) Start(ctx context.Context) {
	if t.pingInterval <= 0 || t.engine == nil || t.engine.client == nil {
		return
	}
	t.mu.Lock()
	if t.cancelLoop != nil {
		t.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	t.cancelLoop = cancel
	t.loopDone = make(chan struct{})
	t.mu.Unlock()
	go t.run(loopCtx)
}

// Stop ends the health check. Nothing is sent to the peer.
func (t *IPIPTrunk) Stop(ctx context.Context) error {
	t.mu.Lock()
	if t.cancelLoop != nil {
		t.cancelLoop()
		t.cancelLoop = nil
	}
	loopDone := t.loopDone
	t.mu.Unlock()

	if loopDone != nil {
		select {
		case <-loopDone:
		case <-ctx.Done():
		}
	}
	if t.trustedTLSHost != "" {
		t.untrustOnce.Do(func() { t.engine.RemoveInsecureTLSPeer(t.trustedTLSHost) })
	}
	return nil
}

func (t *IPIPTrunk) run(ctx context.Context) {
	defer close(t.loopDone)
	for {
		t.pingOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.pingInterval):
		}
	}
}

func (t *IPIPTrunk) pingOnce(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, t.pingTimeout)
	defer cancel()
	res, err := t.sendOptions(pingCtx)
	// A ping cut short by Stop says nothing about the peer.
	if ctx.Err() != nil {
		return
	}
	switch {
	case err != nil:
		reason := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "timeout"
		}
		t.markDown(0, reason)
	case optionsReplyIsUp(int(res.StatusCode)):
		t.markUp(int(res.StatusCode))
	default:
		t.markDown(int(res.StatusCode), res.Reason)
	}
}

// optionsReplyIsUp reports whether an OPTIONS reply shows the peer is serving.
// Any answer counts, a refusal included — many peers reject OPTIONS with 404
// or 405 yet take calls — except those that mean no working peer was reached:
// 408, and 5xx other than 501 (which only says OPTIONS is unsupported).
func optionsReplyIsUp(code int) bool {
	if code == sip.StatusRequestTimeout {
		return false
	}
	if code >= 500 && code < 600 {
		return code == sip.StatusNotImplemented
	}
	return true
}

func (t *IPIPTrunk) sendOptions(ctx context.Context) (*sip.Response, error) {
	req := sip.NewRequest(sip.OPTIONS, t.peerURI)
	if _, _, transport := uriSocket(t.nextHopURI()); !strings.EqualFold(transport, "udp") {
		req.SetTransport(strings.ToUpper(transport))
	}
	// Sent straight at the proxy rather than through a pre-loaded Route, for
	// the reason buildRegister gives.
	if t.outboundProxy != nil {
		host, port, _ := uriSocket(*t.outboundProxy)
		req.SetDestination(JoinHostPort(host, port))
	}
	if t.aor != nil {
		from := &sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: t.aor.User, Host: t.aor.Host}}
		from.Params = sip.NewParams()
		from.Params.Add("tag", sip.GenerateTagN(16))
		req.AppendHeader(from)
	}
	req.AppendHeader(sip.NewHeader("Accept", "application/sdp"))
	req.AppendHeader(t.engine.UserAgentHeader())

	res, err := t.engine.client.Do(ctx, req)
	t.engine.logSIPMessage("outbound", req)
	if err != nil {
		return nil, err
	}
	t.engine.logSIPMessage("inbound", res)
	return res, nil
}

func (t *IPIPTrunk) markUp(statusCode int) {
	t.mu.Lock()
	changed := t.health != trunkHealthUp
	t.health = trunkHealthUp
	t.status = TrunkStatusActive
	t.lastError = ""
	t.lastPingAt = time.Now()
	t.lastPingStatus = statusCode
	t.mu.Unlock()

	if !changed {
		return
	}
	t.log.Info("trunk peer reachable", "status_code", statusCode)
	t.publish(events.SIPTrunkUp, statusCode, "")
}

func (t *IPIPTrunk) markDown(statusCode int, reason string) {
	lastError := reason
	if statusCode != 0 {
		lastError = fmt.Sprintf("%d %s", statusCode, reason)
	}
	t.mu.Lock()
	changed := t.health != trunkHealthDown
	t.health = trunkHealthDown
	t.status = TrunkStatusFailed
	t.lastError = lastError
	t.lastPingAt = time.Now()
	t.lastPingStatus = statusCode
	t.mu.Unlock()

	if !changed {
		return
	}
	t.log.Warn("trunk peer unreachable", "status_code", statusCode, "reason", reason)
	t.publish(events.SIPTrunkDown, statusCode, reason)
}

func (t *IPIPTrunk) publish(typ events.EventType, statusCode int, reason string) {
	if t.bus == nil {
		return
	}
	t.bus.Publish(typ, &events.SIPTrunkStatusData{
		SIPTrunkScope: events.SIPTrunkScope{AppID: t.appID},
		TrunkID:       t.id,
		TrunkType:     string(TrunkTypeIPIP),
		PeerURI:       t.peerURI.String(),
		StatusCode:    statusCode,
		Reason:        reason,
	})
}
