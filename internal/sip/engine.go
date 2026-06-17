package sip

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"golang.org/x/sync/errgroup"
)

// containsToken checks if a comma-separated header value contains a token
// (case-insensitive), e.g. containsToken("100rel, timer", "timer") → true.
func containsToken(headerValue, token string) bool {
	for _, t := range strings.Split(headerValue, ",") {
		if strings.EqualFold(strings.TrimSpace(t), token) {
			return true
		}
	}
	return false
}

// EngineConfig holds configuration for the SIP engine.
type EngineConfig struct {
	BindIP      string // IPv4 advertised address for SDP c= line / Contact (when v4 is in use)
	BindIPV6    string // IPv6 advertised address; empty = v6 not advertised
	ListenIP    string // IPv4 socket bind (default: same as BindIP). Special values: "0.0.0.0", "::" (dual-stack)
	ListenIPV6  string // IPv6 socket bind (default: same as BindIPV6). Used when configured separately from ListenIP.
	ExternalIP  string // IPv4 public IP override for NAT/Docker (v6 has no equivalent — set BindIPV6 directly)
	PublicHost  string // FQDN advertised in From/Contact/Via signaling headers; falls back to ExternalIP/BindIP when empty
	BindPort    int
	TLSBindPort int    // 0 = TLS disabled
	TLSCertPath string // CA-signed cert (fullchain.pem) — required when TLSBindPort > 0
	TLSKeyPath  string // private key (privkey.pem) — required when TLSBindPort > 0
	SIPDebug    bool   // dump full SIP request/response bodies on the debug channel
	SIPHost     string
	// UseSourceSocket forces SIP responses and in-dialog requests to be
	// routed back to the request's source socket (req.Source()) instead of
	// the peer's Contact URI / Via sent-by. Required when peers are behind
	// NAT and advertise unroutable addresses.
	UseSourceSocket bool
	Codecs          []codec.CodecType
	// AMRWBMode is the AMR-WB encoder speech mode (0..8) offered/transmitted.
	AMRWBMode int
	// AMRWBOctetAligned selects octet-aligned (true) vs bandwidth-efficient
	// (false) AMR-WB framing in generated offers.
	AMRWBOctetAligned bool
	// AMRNBMode is the AMR-NB encoder speech mode (0..7) offered/transmitted.
	AMRNBMode int
	// AMRNBOctetAligned selects octet-aligned (true) vs bandwidth-efficient
	// (false) AMR-NB framing in generated offers.
	AMRNBOctetAligned bool
	Log               *slog.Logger
	PortAllocator     *PortAllocator // nil = OS-assigned ports

	// Registrar, when non-nil, enables inbound REGISTER handling and AOR
	// resolution for outbound INVITEs.
	Registrar *Registrar
}

// Engine wraps sipgo server/client + dialog caches for SIP signaling.
type Engine struct {
	ua      *sipgo.UserAgent
	server  *sipgo.Server
	client  *sipgo.Client
	dsCache *dialogServerCache
	dcCache *dialogClientCache

	onInvite          func(call *InboundCall)
	onReInvite        func(callID string, direction string) []byte // returns SDP answer for 200 OK
	onUpdate          func(callID string, direction string, hasSDP bool) []byte
	onRefer           func(callID string, target string, replaces *ReplacesParams, req *sip.Request, tx sip.ServerTransaction)
	onNotify          func(callID string, statusCode int, reason string, terminated bool)
	codecs            []codec.CodecType
	amrwbMode         int
	amrwbOctetAligned bool
	amrnbMode         int
	amrnbOctetAligned bool
	bindIP            string // IPv4 advertised address (SDP c= / Contact); empty if v6-only deployment
	bindIPV6          string // IPv6 advertised address; empty if v4-only
	publicHost        string // hostname advertised in From/Contact/Via — equals SIPDomain when set, otherwise bindIP
	listenIP          string // primary listen address (for ListenAndServe). May be "::" / "0.0.0.0" / literal.
	listenIPV6        string // optional secondary IPv6 listen address (only used when both v4 and v6 literals are configured separately)
	bindPort          int
	tlsPort           int // 0 = TLS disabled
	tlsCert           string
	tlsKey            string
	sipHost           string
	portAlloc         *PortAllocator
	log               *slog.Logger
	sipDebug          bool
	useSourceSocket   bool
	destPinned        atomic.Uint64 // count of res.Destination overrides applied
	registrar         *Registrar
}

// logSIPMessage prints the full RFC 3261 wire form of a SIP request or
// response when SIP_DEBUG is on. Called from inbound handler wrappers and
// outbound ClientRequestOptions.
func (e *Engine) logSIPMessage(direction string, m sip.Message) {
	if !e.sipDebug || m == nil {
		return
	}
	e.log.Info("SIP "+direction, "message", "\n"+m.String())
}

// SIPDebug reports whether SIP_DEBUG is enabled. Consumers that send
// responses via sipgo (dialog.Respond / RespondSDP) use this to gate
// LogSyntheticResponse calls.
func (e *Engine) SIPDebug() bool { return e.sipDebug }

// LogSyntheticResponse constructs a response from a request (mirroring
// what sipgo would build internally) purely for SIP_DEBUG logging. The
// actual response still goes out through dialog.Respond / dialog.RespondSDP;
// this is a best-effort wire-form dump so the body and headers we ask sipgo
// to include are visible on the debug channel.
func (e *Engine) LogSyntheticResponse(req *sip.Request, statusCode int, reason string, body []byte, headers ...sip.Header) {
	if !e.sipDebug || req == nil {
		return
	}
	res := sip.NewResponseFromRequest(req, statusCode, reason, body)
	for _, h := range headers {
		res.AppendHeader(h)
	}
	if len(body) > 0 && res.ContentType() == nil {
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	e.logSIPMessage("outbound (synthetic)", res)
}

// inviteIsTLS reports whether an inbound INVITE arrived over TLS (based on
// the topmost Via sent-by transport). Used to pick sip: vs sips: Contact.
func inviteIsTLS(req *sip.Request) bool {
	if req == nil {
		return false
	}
	via := req.Via()
	if via == nil {
		return false
	}
	return strings.EqualFold(via.Transport, "TLS") || strings.EqualFold(via.Transport, "WSS")
}

// ContactForInvite is the public form of contactForInvite, used by callers
// outside this package that need to attach a transport-appropriate Contact
// header to a dialog response.
func (e *Engine) ContactForInvite(req *sip.Request) *sip.ContactHeader {
	return e.contactForInvite(req)
}

// contactForInvite returns a Contact that matches the transport on which
// the INVITE arrived — sips:<ip>:<tlsPort> for TLS, sip:<ip>:<udpPort>
// otherwise. The host is family-aware: when the INVITE arrived over IPv6
// and we have an IPv6 advertised address configured, the Contact uses
// that address so the peer's ACK / BYE routes back to us.
func (e *Engine) contactForInvite(req *sip.Request) *sip.ContactHeader {
	host := e.contactHostForRequest(req)
	if inviteIsTLS(req) && e.tlsPort != 0 {
		return &sip.ContactHeader{Address: sip.Uri{Scheme: "sips", Host: host, Port: e.tlsPort}}
	}
	return &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: host, Port: e.bindPort}}
}

// contactHostForRequest picks the Contact host for an inbound request based
// on the address family of the request's source. Falls back to publicHost
// when no family-specific advertised IP is configured.
func (e *Engine) contactHostForRequest(req *sip.Request) string {
	if req == nil {
		return e.publicHost
	}
	src := req.Source()
	if src == "" {
		return e.publicHost
	}
	host, _, err := net.SplitHostPort(src)
	if err != nil {
		return e.publicHost
	}
	switch AddressFamily(host) {
	case "IP6":
		if e.bindIPV6 != "" {
			return e.bindIPV6
		}
	case "IP4":
		if e.bindIP != "" {
			return e.bindIP
		}
	}
	return e.publicHost
}

// RespondInviteSDP sends a 2xx response to an inbound INVITE with a
// transport-appropriate Contact header. This is required for WhatsApp
// inbound calls, which arrive over TLS and need a sips: Contact pointing
// at our TLS port — otherwise the remote's ACK is routed to the wrong
// scheme/port, the dialog stays in Early state, and retransmits eventually
// kill the transaction.
func (e *Engine) RespondInviteSDP(dialog *sipgo.DialogServerSession, sdp []byte) error {
	if dialog == nil || dialog.InviteRequest == nil {
		return fmt.Errorf("RespondInviteSDP: dialog or InviteRequest is nil")
	}
	res := sip.NewResponseFromRequest(dialog.InviteRequest, sip.StatusOK, "OK", sdp)
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.AppendHeader(e.ServerHeader())
	res.AppendHeader(e.AllowHeader())
	res.AppendHeader(e.contactForInvite(dialog.InviteRequest))
	res.SetBody(sdp)
	e.pinDestinationToSource(dialog.InviteRequest, res)

	e.logSIPMessage("outbound", res)
	return dialog.WriteResponse(res)
}

// InboundCall wraps a sipgo DialogServerSession with parsed SDP.
type InboundCall struct {
	Dialog    *sipgo.DialogServerSession
	From      string    // caller URI user part
	To        string    // callee URI user part
	RemoteSDP *SDPMedia // parsed offer SDP
	Request   *sip.Request

	// Session timer (RFC 4028) — populated when remote requests timers.
	SessionTimer *SessionTimerParams // nil when remote didn't request timers
}

// OutboundCall wraps a sipgo DialogClientSession with parsed answer SDP.
type OutboundCall struct {
	Dialog    *sipgo.DialogClientSession
	RemoteSDP *SDPMedia
	RTPSess   *RTPSession

	// Optional RTT (T.140 / RFC 4103) media. Populated when the remote's
	// answer accepts the offered m=text section. Nil otherwise.
	TextRTPSess *RTPSession

	// Session timer (RFC 4028) — populated when remote's 200 OK includes timers.
	SessionTimer *SessionTimerParams // nil when remote didn't include timers
}

// resolveExternalIPs probes the preferred outbound LAN IPs for both address
// families. No traffic is sent — UDP connect only sets routing. Either return
// value may be "" if that family has no outbound route. err is non-nil only
// when both probes fail.
func resolveExternalIPs() (v4, v6 string, err error) {
	v4, errV4 := probeOutboundIP("udp4", "8.8.8.8:53")
	v6, errV6 := probeOutboundIP("udp6", "[2606:4700:4700::1111]:53")
	if v4 == "" && v6 == "" {
		return "", "", fmt.Errorf("no outbound route (v4: %v, v6: %v)", errV4, errV6)
	}
	return v4, v6, nil
}

func probeOutboundIP(network, addr string) (string, error) {
	conn, err := net.Dial(network, addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// NewEngine creates a SIP engine with the given configuration.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	advertiseIP := cfg.BindIP
	advertiseIPV6 := cfg.BindIPV6
	listenIP := cfg.ListenIP
	listenIPV6 := cfg.ListenIPV6

	// Auto-detect when BindIP / BindIPV6 is unroutable. The wildcard listen
	// values are preserved for the socket; the advertised value gets the
	// detected literal so SDP / Contact have something resolvable.
	//
	// "Empty" BindIP is treated as a probe trigger only when no v6 is
	// configured either — otherwise the user explicitly wants a v6-only
	// deployment and we should not insist on a v4 probe (which would fail
	// on v6-only hosts).
	needV4Probe := advertiseIP == "0.0.0.0" || advertiseIP == "::" ||
		(advertiseIP == "" && advertiseIPV6 == "" && cfg.ExternalIP == "")
	needV6Probe := advertiseIPV6 == "::"
	if needV4Probe || needV6Probe {
		detectedV4, detectedV6, err := resolveExternalIPs()
		if err != nil {
			return nil, fmt.Errorf("SIP_BIND_IP=%q SIP_BIND_IPV6=%q; auto-detect failed: %w", cfg.BindIP, cfg.BindIPV6, err)
		}
		if needV4Probe {
			if listenIP == "" {
				listenIP = advertiseIP // keep the wildcard for the socket
			}
			advertiseIP = detectedV4
			// "::" wildcard with no v6 advertised IP yet — fill in if probe succeeded.
			if cfg.BindIP == "::" && advertiseIPV6 == "" && detectedV6 != "" {
				advertiseIPV6 = detectedV6
			}
		}
		if needV6Probe {
			if listenIPV6 == "" {
				listenIPV6 = advertiseIPV6
			}
			advertiseIPV6 = detectedV6
		}
	}

	if listenIP == "" {
		listenIP = advertiseIP
	}
	if listenIPV6 == "" && advertiseIPV6 != "" {
		listenIPV6 = advertiseIPV6
	}

	// Explicit external IPv4 overrides advertised v4 (NAT/Docker). IPv6 has
	// no equivalent — set BindIPV6 directly, since IPv6 deployments don't
	// typically NAT.
	if cfg.ExternalIP != "" {
		advertiseIP = cfg.ExternalIP
	}

	// publicHost is the canonical signalling identity used in
	// From/Contact/Via. SIP_DOMAIN takes precedence; otherwise we use the
	// advertised IPv4 IP, falling back to IPv6 in v6-only deployments.
	publicHost := cfg.PublicHost
	if publicHost == "" {
		if advertiseIP != "" {
			publicHost = advertiseIP
		} else {
			publicHost = advertiseIPV6
		}
	}

	// Route sipgo's own internal debug logs (transport/transaction layer) to
	// our logger when SIP_DEBUG is on. These cover messages sipgo sends or
	// receives automatically (100 Trying, 487 Request Terminated after CANCEL,
	// retransmits) that our handler-level wrappers can't observe.
	sipgoLog := cfg.Log
	if cfg.SIPDebug {
		sipgoLog = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
		sip.SetDefaultLogger(sipgoLog)
	}

	uaOpts := []sipgo.UserAgentOption{
		sipgo.WithUserAgent(cfg.SIPHost),
		sipgo.WithUserAgentHostname(publicHost),
	}
	if cfg.SIPDebug {
		uaOpts = append(uaOpts,
			sipgo.WithUserAgentTransportLayerOptions(sip.WithTransportLayerLogger(sipgoLog)),
			sipgo.WithUserAgentTransactionLayerOptions(sip.WithTransactionLayerLogger(sipgoLog)),
		)
	}
	if cfg.TLSBindPort != 0 {
		// Needed for outbound TLS dials (e.g. wa.meta.vc:5061). The listener's
		// own cert is still supplied separately via ListenAndServeTLS.
		uaOpts = append(uaOpts, sipgo.WithUserAgenTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	}
	ua, err := sipgo.NewUA(uaOpts...)
	if err != nil {
		return nil, fmt.Errorf("create UA: %w", err)
	}

	serverOpts := []sipgo.ServerOption{}
	if cfg.SIPDebug {
		serverOpts = append(serverOpts, sipgo.WithServerLogger(sipgoLog))
	}
	server, err := sipgo.NewServer(ua, serverOpts...)
	if err != nil {
		return nil, fmt.Errorf("create server: %w", err)
	}

	// Pin Via sent-by to publicHost — wildcard binds make the response
	// path unroutable, so peers black-hole our REFER/BYE/re-INVITE 200s.
	clientOpts := []sipgo.ClientOption{
		sipgo.WithClientHostname(publicHost),
		sipgo.WithClientPort(cfg.BindPort),
	}
	if cfg.SIPDebug {
		clientOpts = append(clientOpts, sipgo.WithClientLogger(sipgoLog))
	}
	client, err := sipgo.NewClient(ua, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	contactHdr := sip.ContactHeader{
		Address: sip.Uri{
			Scheme: "sip",
			Host:   publicHost,
			Port:   cfg.BindPort,
		},
	}

	if cfg.TLSBindPort != 0 {
		if cfg.TLSCertPath == "" || cfg.TLSKeyPath == "" {
			return nil, fmt.Errorf("TLS enabled (port %d) but TLSCertPath/TLSKeyPath not set", cfg.TLSBindPort)
		}
		if _, err := tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath); err != nil {
			return nil, fmt.Errorf("load TLS cert: %w", err)
		}
	}

	serverUA := &sipgo.DialogUA{
		Client:         client,
		ContactHDR:     contactHdr,
		RewriteContact: cfg.UseSourceSocket,
	}
	clientUA := &sipgo.DialogUA{
		Client:         client,
		ContactHDR:     contactHdr,
		RewriteContact: cfg.UseSourceSocket,
	}

	e := &Engine{
		ua:                ua,
		server:            server,
		client:            client,
		dsCache:           newDialogServerCache(serverUA),
		dcCache:           newDialogClientCache(clientUA),
		codecs:            cfg.Codecs,
		amrwbMode:         cfg.AMRWBMode,
		amrwbOctetAligned: cfg.AMRWBOctetAligned,
		amrnbMode:         cfg.AMRNBMode,
		amrnbOctetAligned: cfg.AMRNBOctetAligned,
		bindIP:            advertiseIP,
		bindIPV6:          advertiseIPV6,
		publicHost:        publicHost,
		listenIP:          listenIP,
		listenIPV6:        listenIPV6,
		bindPort:          cfg.BindPort,
		tlsPort:           cfg.TLSBindPort,
		tlsCert:           cfg.TLSCertPath,
		tlsKey:            cfg.TLSKeyPath,
		sipHost:           cfg.SIPHost,
		portAlloc:         cfg.PortAllocator,
		log:               cfg.Log,
		sipDebug:          cfg.SIPDebug,
		useSourceSocket:   cfg.UseSourceSocket,
		registrar:         cfg.Registrar,
	}

	if cfg.Log != nil {
		warnIfBindV6OnlyConflict(cfg.Log, listenIP, listenIPV6)
	}

	e.registerHandlers()
	return e, nil
}

// OnInvite registers a handler for inbound INVITE requests.
func (e *Engine) OnInvite(handler func(*InboundCall)) {
	e.onInvite = handler
}

// OnReInvite registers a handler for in-dialog re-INVITE requests (hold/unhold).
// The handler receives the SIP Call-ID and the SDP direction attribute, and
// returns the SDP body to include in the 200 OK response (nil = no SDP).
func (e *Engine) OnReInvite(handler func(callID string, direction string) []byte) {
	e.onReInvite = handler
}

// OnUpdate registers a handler for in-dialog UPDATE requests (RFC 3311),
// used for session-timer refresh (RFC 4028) and mid-dialog media changes.
// hasSDP indicates whether the UPDATE carried an SDP offer; when true,
// direction is the parsed a=sendrecv/sendonly/recvonly/inactive attribute
// and the returned []byte is the SDP answer for the 200 OK. When false,
// direction is "" and the handler should only refresh session-timer state.
func (e *Engine) OnUpdate(handler func(callID string, direction string, hasSDP bool) []byte) {
	e.onUpdate = handler
}

// OnRefer registers a handler for in-dialog REFER requests (transfer). The
// handler is responsible for sending the SIP response (typically 202
// Accepted, or 603 Decline when transfers are disabled). req is provided
// so the handler can pass it to sip.NewResponseFromRequest.
func (e *Engine) OnRefer(handler func(callID string, target string, replaces *ReplacesParams, req *sip.Request, tx sip.ServerTransaction)) {
	e.onRefer = handler
}

// OnNotify registers a handler for in-dialog NOTIFY requests carrying a
// "refer" subscription (RFC 3515 sipfrag). It is invoked once per NOTIFY
// with the subscription's terminal/transient SIP status parsed from the
// sipfrag body.
func (e *Engine) OnNotify(handler func(callID string, statusCode int, reason string, terminated bool)) {
	e.onNotify = handler
}

// handleReInvite processes an in-dialog re-INVITE (e.g. hold/unhold).
func (e *Engine) handleReInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID()
	if callID == nil {
		e.log.Error("re-INVITE missing Call-ID")
		res := sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Missing Call-ID", nil)
		if rerr := e.respondMaybeFromSource(tx, req, res); rerr != nil {
			e.log.Error("re-INVITE: respond 400 failed", "error", rerr)
		}
		return
	}

	// Update the existing dialog's CSeq tracking so that the subsequent
	// ACK (which carries the same CSeq as this re-INVITE) can be matched
	// by dsCache.ReadAck / dcCache without "invalid CSEQ number" errors.
	if ds, err := e.dsCache.MatchDialogRequest(req); err == nil {
		if err := ds.ReadRequest(req, tx); err != nil {
			e.log.Debug("re-INVITE: ReadRequest on server dialog", "error", err)
		}
	} else if dc, err := e.dcCache.MatchRequestDialog(req); err == nil {
		if err := dc.ReadRequest(req, tx); err != nil {
			e.log.Debug("re-INVITE: ReadRequest on client dialog", "error", err)
		}
	}

	body := req.Body()
	direction := "sendrecv"
	if len(body) > 0 {
		remoteSDP, err := ParseSDP(body)
		if err != nil {
			e.log.Warn("re-INVITE: parse SDP failed", "error", err)
		} else if remoteSDP.Direction != "" {
			direction = remoteSDP.Direction
		}
	}

	// Call the handler before responding so it can provide the SDP answer
	// and update hold state.
	var answerSDP []byte
	if e.onReInvite != nil {
		answerSDP = e.onReInvite(callID.Value(), direction)
	}

	// Respond 200 OK with SDP answer (RFC 3261 §14.2 requires SDP in 200).
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", answerSDP)
	res.AppendHeader(e.ServerHeader())
	res.AppendHeader(e.AllowHeader())
	if len(answerSDP) > 0 {
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	// Echo Session-Expires in the re-INVITE 200 OK if present (RFC 4028).
	if seHdr := req.GetHeader("Session-Expires"); seHdr != nil {
		interval, refresher := ParseSessionExpires(seHdr.Value())
		if interval > 0 {
			if refresher == "" {
				refresher = "uac"
			}
			res.AppendHeader(sip.NewHeader("Supported", "timer"))
			res.AppendHeader(sip.NewHeader("Session-Expires", FormatSessionExpires(interval, refresher)))
		}
	}
	if err := e.respondMaybeFromSource(tx, req, res); err != nil {
		e.log.Error("re-INVITE: respond failed", "error", err)
		return
	}

	e.log.Info("re-INVITE handled", "call_id", callID.Value(), "direction", direction)
}

// handleUpdate processes an in-dialog UPDATE (RFC 3311). Typical uses are
// session-timer refresh without an SDP body (RFC 4028) and mid-dialog media
// renegotiation with an SDP offer.
func (e *Engine) handleUpdate(req *sip.Request, tx sip.ServerTransaction) {
	callID := req.CallID()
	if callID == nil {
		e.log.Error("UPDATE missing Call-ID")
		res := sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Missing Call-ID", nil)
		res.AppendHeader(e.ServerHeader())
		if rerr := e.respondMaybeFromSource(tx, req, res); rerr != nil {
			e.log.Error("UPDATE: respond 400 failed", "error", rerr)
		}
		return
	}

	// RFC 3311 §5.2: UPDATE must target an existing dialog. Find it on
	// either the UAS or UAC dialog cache so we advance the CSeq tracker.
	matched := false
	if ds, err := e.dsCache.MatchDialogRequest(req); err == nil {
		matched = true
		if rerr := ds.ReadRequest(req, tx); rerr != nil {
			e.log.Debug("UPDATE: ReadRequest on server dialog", "error", rerr)
		}
	} else if dc, err := e.dcCache.MatchRequestDialog(req); err == nil {
		matched = true
		if rerr := dc.ReadRequest(req, tx); rerr != nil {
			e.log.Debug("UPDATE: ReadRequest on client dialog", "error", rerr)
		}
	}
	if !matched {
		e.log.Debug("UPDATE: no matching dialog, replying 481", "call_id", callID.Value())
		res := sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)
		res.AppendHeader(e.ServerHeader())
		if rerr := e.respondMaybeFromSource(tx, req, res); rerr != nil {
			e.log.Error("UPDATE: respond 481 failed", "error", rerr)
		}
		return
	}

	body := req.Body()
	hasSDP := len(body) > 0
	direction := ""
	if hasSDP {
		remoteSDP, err := ParseSDP(body)
		if err != nil {
			e.log.Warn("UPDATE: parse SDP failed", "error", err)
			res := sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad SDP", nil)
			res.AppendHeader(e.ServerHeader())
			if rerr := e.respondMaybeFromSource(tx, req, res); rerr != nil {
				e.log.Error("UPDATE: respond 400 failed", "error", rerr)
			}
			return
		}
		direction = remoteSDP.Direction
		if direction == "" {
			direction = "sendrecv"
		}
	}

	var answerSDP []byte
	if e.onUpdate != nil {
		answerSDP = e.onUpdate(callID.Value(), direction, hasSDP)
	}

	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", answerSDP)
	res.AppendHeader(e.ServerHeader())
	res.AppendHeader(e.AllowHeader())
	if len(answerSDP) > 0 {
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	// Echo Session-Expires per RFC 4028 §9.
	if seHdr := req.GetHeader("Session-Expires"); seHdr != nil {
		interval, refresher := ParseSessionExpires(seHdr.Value())
		if interval > 0 {
			if refresher == "" {
				refresher = "uac"
			}
			res.AppendHeader(sip.NewHeader("Supported", "timer"))
			res.AppendHeader(sip.NewHeader("Session-Expires", FormatSessionExpires(interval, refresher)))
		}
	}
	if err := e.respondMaybeFromSource(tx, req, res); err != nil {
		e.log.Error("UPDATE: respond failed", "error", err)
		return
	}

	e.log.Info("UPDATE handled", "call_id", callID.Value(), "has_sdp", hasSDP, "direction", direction)
}

// SendReInvite sends a re-INVITE within an existing dialog for hold/unhold.
// dialog must be either *sipgo.DialogServerSession or *sipgo.DialogClientSession.
func (e *Engine) SendReInvite(ctx context.Context, dialog interface{}, sdpBody []byte) error {
	switch d := dialog.(type) {
	case *sipgo.DialogServerSession:
		req := sip.NewRequest(sip.INVITE, d.InviteRequest.Contact().Address)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.AppendHeader(e.AllowHeader())
		req.SetBody(sdpBody)

		res, err := d.Do(ctx, req)
		if err != nil {
			return fmt.Errorf("re-INVITE Do: %w", err)
		}
		if !res.IsSuccess() {
			return fmt.Errorf("re-INVITE rejected: %d %s", res.StatusCode, res.Reason)
		}

		// Send ACK
		cont := res.Contact()
		if cont != nil {
			ack := sip.NewRequest(sip.ACK, cont.Address)
			return d.WriteRequest(ack)
		}
		return nil

	case *sipgo.DialogClientSession:
		req := sip.NewRequest(sip.INVITE, d.InviteResponse.Contact().Address)
		req.AppendHeader(d.InviteRequest.Contact())
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.AppendHeader(e.AllowHeader())
		req.SetBody(sdpBody)

		res, err := d.Do(ctx, req)
		if err != nil {
			return fmt.Errorf("re-INVITE Do: %w", err)
		}
		if !res.IsSuccess() {
			return fmt.Errorf("re-INVITE rejected: %d %s", res.StatusCode, res.Reason)
		}

		// Send ACK
		cont := res.Contact()
		if cont != nil {
			ack := sip.NewRequest(sip.ACK, cont.Address)
			return d.WriteRequest(ack)
		}
		return nil

	default:
		return fmt.Errorf("unsupported dialog type: %T", dialog)
	}
}

func (e *Engine) registerHandlers() {
	// wrap prepends SIP_DEBUG message dumping to a handler. Identity
	// wrapper when SIP_DEBUG is off.
	wrap := func(h sipgo.RequestHandler) sipgo.RequestHandler {
		if !e.sipDebug {
			return h
		}
		return func(req *sip.Request, tx sip.ServerTransaction) {
			e.logSIPMessage("inbound", req)
			h(req, tx)
		}
	}

	e.server.OnInvite(wrap(func(req *sip.Request, tx sip.ServerTransaction) {
		// Check if this is a re-INVITE (in-dialog request with To tag).
		if to := req.To(); to != nil {
			if tag, ok := to.Params.Get("tag"); ok && tag != "" {
				e.handleReInvite(req, tx)
				return
			}
		}

		ds, err := e.dsCache.ReadInvite(req, tx)
		if err != nil {
			e.log.Error("read invite failed", "error", err)
			res := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
			res.AppendHeader(e.ServerHeader())
			res.AppendHeader(e.AllowHeader())
			if rerr := e.respondMaybeFromSource(tx, req, res); rerr != nil {
				e.log.Error("INVITE: respond 500 failed", "error", rerr)
			}
			return
		}

		remoteSDP, err := ParseSDP(req.Body())
		if err != nil {
			e.log.Error("parse offer SDP failed", "error", err)
			ds.Respond(sip.StatusBadRequest, "Bad SDP", nil, e.ServerHeader(), e.AllowHeader())
			return
		}

		from := ""
		if f := req.From(); f != nil {
			from = f.Address.User
		}
		to := ""
		if t := req.To(); t != nil {
			to = t.Address.User
		}

		// Parse session timer headers (RFC 4028).
		var sessionTimer *SessionTimerParams
		if seHdr := req.GetHeader("Session-Expires"); seHdr != nil {
			interval, refresher := ParseSessionExpires(seHdr.Value())
			if interval > 0 {
				var minSE uint32
				if mseHdr := req.GetHeader("Min-SE"); mseHdr != nil {
					minSE = ParseMinSE(mseHdr.Value())
				}
				// Enforce our minimum.
				if minSE < DefaultMinSE {
					minSE = DefaultMinSE
				}
				if interval < minSE {
					interval = minSE
				}
				// Default refresher: prefer uac if they support timer.
				if refresher == "" {
					refresher = "uac"
					if sup := req.GetHeader("Supported"); sup != nil {
						if !containsToken(sup.Value(), "timer") {
							refresher = "uas"
						}
					} else {
						refresher = "uas"
					}
				}
				sessionTimer = &SessionTimerParams{
					Interval:  interval,
					Refresher: refresher,
					MinSE:     minSE,
				}
			}
		}

		call := &InboundCall{
			Dialog:       ds,
			From:         from,
			To:           to,
			RemoteSDP:    remoteSDP,
			Request:      req,
			SessionTimer: sessionTimer,
		}

		if e.onInvite != nil {
			// Must block — sipgo calls tx.TerminateGracefully() when this
			// handler returns, which would kill the transaction before any
			// response is sent.  HandleInboundCall blocks until the call ends.
			e.onInvite(call)
		}
	}))

	e.server.OnAck(wrap(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := e.dsCache.ReadAck(req, tx); err != nil {
			e.log.Debug("read ack failed", "error", err)
		}
	}))

	e.server.OnBye(wrap(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := e.dsCache.ReadBye(req, tx); err != nil {
			if err := e.dcCache.ReadBye(req, tx); err != nil {
				// RFC 3261 §8.2.2.1
				e.log.Debug("BYE: no matching dialog, replying 481", "error", err)
				if rerr := e.respondFromSource(tx, req, 481, "Call/Transaction Does Not Exist"); rerr != nil {
					e.log.Error("BYE: respond 481 failed", "error", rerr)
				}
			}
		}
	}))

	e.server.OnCancel(wrap(func(req *sip.Request, tx sip.ServerTransaction) {
		// This handler fires only for CANCELs that didn't match an active
		// INVITE transaction.  For matched CANCELs, sipgo's transaction
		// layer handles both 487 (for INVITE) and 200 OK (for CANCEL)
		// automatically.  Respond 481 per RFC 3261 §9.2.
		callID := ""
		if c := req.CallID(); c != nil {
			callID = c.Value()
		}
		e.log.Info("CANCEL received (unmatched)", "call_id", callID, "source", req.Source())
		res := sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)
		res.AppendHeader(e.ServerHeader())
		if rerr := e.respondMaybeFromSource(tx, req, res); rerr != nil {
			e.log.Error("CANCEL: respond 481 failed", "error", rerr)
		}
	}))

	e.server.OnUpdate(wrap(e.handleUpdate))
	e.server.OnRefer(wrap(e.handleRefer))
	e.server.OnNotify(wrap(e.handleNotify))
	e.server.OnRegister(wrap(e.handleRegister))
}

// Registrar returns the engine's AOR registry (may be nil when the engine
// was created without one).
func (e *Engine) Registrar() *Registrar { return e.registrar }

// RespondFromSource pins the response destination to the request's UDP
// source so peers with unroutable Via headers still get our reply.
func (e *Engine) RespondFromSource(tx sip.ServerTransaction, req *sip.Request, statusCode int, reason string) error {
	res := sip.NewResponseFromRequest(req, statusCode, reason, nil)
	res.AppendHeader(e.ServerHeader())
	if src := req.Source(); src != "" {
		res.SetDestination(src)
	}
	return tx.Respond(res)
}

func (e *Engine) respondFromSource(tx sip.ServerTransaction, req *sip.Request, statusCode int, reason string) error {
	return e.RespondFromSource(tx, req, statusCode, reason)
}

// pinDestinationToSource sets res.Destination to req.Source() when the
// SIP_USE_SOURCE_SOCKET flag is on. No-op when the flag is off, or when
// req.Source() doesn't resolve to a real address (sipgo's Request.Source
// falls back to a Via-derived ":port" form when SetSource has never been
// called — that's a synthetic request, not something we should route to).
func (e *Engine) pinDestinationToSource(req *sip.Request, res *sip.Response) {
	if !e.useSourceSocket {
		return
	}
	src := req.Source()
	if src == "" || strings.HasPrefix(src, ":") {
		return
	}
	res.SetDestination(src)
	e.destPinned.Add(1)
}

// DestinationsPinned returns the cumulative count of responses whose
// destination has been overridden via SIP_USE_SOURCE_SOCKET. Tests use this
// to assert the flag actually fired rather than silently no-op'd.
func (e *Engine) DestinationsPinned() uint64 {
	return e.destPinned.Load()
}

// respondMaybeFromSource sends a pre-built response, pinning the destination
// to req.Source() when SIP_USE_SOURCE_SOCKET is enabled. Use this for
// response paths that already build their own *sip.Response (to attach
// Server / Content-Type / Session-Expires headers etc).
func (e *Engine) respondMaybeFromSource(tx sip.ServerTransaction, req *sip.Request, res *sip.Response) error {
	e.pinDestinationToSource(req, res)
	return tx.Respond(res)
}

// DialogRespond sends a response on a UAS dialog, pinning the destination to
// the original INVITE source when SIP_USE_SOURCE_SOCKET is enabled. Replaces
// direct calls to dialog.Respond / dialog.RespondSDP — those reach
// tx.Respond internally without giving us a hook to override Destination.
func (e *Engine) DialogRespond(d *sipgo.DialogServerSession, statusCode int, reason string, body []byte, headers ...sip.Header) error {
	res := sip.NewResponseFromRequest(d.InviteRequest, statusCode, reason, body)
	for _, h := range headers {
		res.AppendHeader(h)
	}
	res.AppendHeader(e.AllowHeader())
	e.pinDestinationToSource(d.InviteRequest, res)
	return d.WriteResponse(res)
}

// handleRefer dispatches inbound REFER to the onRefer hook (which decides 202 vs decline).
func (e *Engine) handleRefer(req *sip.Request, tx sip.ServerTransaction) {
	e.log.Info("REFER received", "call_id", req.CallID().Value(), "from", req.From().Address.String(), "source", req.Source())
	callID := ""
	if cid := req.CallID(); cid != nil {
		callID = cid.Value()
	}
	hdr := req.GetHeader("Refer-To")
	if hdr == nil {
		if err := e.respondFromSource(tx, req, sip.StatusBadRequest, "Missing Refer-To"); err != nil {
			e.log.Error("REFER: respond 400 failed", "error", err)
		}
		return
	}
	target, replaces, err := ParseReferTo(hdr.Value())
	if err != nil {
		e.log.Error("REFER: bad Refer-To", "error", err, "value", hdr.Value())
		if err := e.respondFromSource(tx, req, sip.StatusBadRequest, "Bad Refer-To"); err != nil {
			e.log.Error("REFER: respond 400 failed", "error", err)
		}
		return
	}
	if e.onRefer == nil {
		if err := e.respondFromSource(tx, req, 501, "Not Implemented"); err != nil {
			e.log.Error("REFER: respond 501 failed", "error", err)
		}
		return
	}
	e.onRefer(callID, target, replaces, req, tx)
}

// handleNotify acks any in-dialog NOTIFY and dispatches "refer" sipfrag bodies.
func (e *Engine) handleNotify(req *sip.Request, tx sip.ServerTransaction) {
	if err := e.respondFromSource(tx, req, sip.StatusOK, "OK"); err != nil {
		e.log.Error("NOTIFY: respond 200 failed", "error", err)
	}

	if e.onNotify == nil {
		return
	}
	if ev := req.GetHeader("Event"); ev == nil || !strings.HasPrefix(strings.ToLower(ev.Value()), "refer") {
		return
	}
	terminated := false
	if ss := req.GetHeader("Subscription-State"); ss != nil {
		terminated = strings.HasPrefix(strings.ToLower(ss.Value()), "terminated")
	}
	code, reason := ParseSipfrag(req.Body())
	callID := ""
	if cid := req.CallID(); cid != nil {
		callID = cid.Value()
	}
	e.onNotify(callID, code, reason, terminated)
}

// Serve starts the SIP server and blocks until ctx is cancelled. When
// TLSBindPort is configured it runs UDP and TLS listeners concurrently; if
// either fails the other is torn down via ctx cancellation. A secondary
// UDP listener is started when listenIPV6 is set to a literal distinct from
// listenIP (rare dual-bind case for hosts with bindv6only=1).
func (e *Engine) Serve(ctx context.Context) error {
	udpNet := UDPNetwork(e.listenIP)
	udpAddr := JoinHostPort(e.listenIP, e.bindPort)

	dualBind := e.listenIPV6 != "" && e.listenIPV6 != e.listenIP && UDPNetwork(e.listenIP) != "udp"

	if e.tlsPort == 0 && !dualBind {
		return e.server.ListenAndServe(ctx, udpNet, udpAddr)
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := e.server.ListenAndServe(gCtx, udpNet, udpAddr); err != nil && gCtx.Err() == nil {
			return fmt.Errorf("UDP listener: %w", err)
		}
		return nil
	})

	if dualBind {
		v6Net := UDPNetwork(e.listenIPV6)
		v6Addr := JoinHostPort(e.listenIPV6, e.bindPort)
		g.Go(func() error {
			if err := e.server.ListenAndServe(gCtx, v6Net, v6Addr); err != nil && gCtx.Err() == nil {
				return fmt.Errorf("UDP v6 listener: %w", err)
			}
			return nil
		})
	}

	if e.tlsPort != 0 {
		cert, err := tls.LoadX509KeyPair(e.tlsCert, e.tlsKey)
		if err != nil {
			return fmt.Errorf("load TLS cert: %w", err)
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		tlsAddr := JoinHostPort(e.listenIP, e.tlsPort)
		g.Go(func() error {
			if err := e.server.ListenAndServeTLS(gCtx, "tls", tlsAddr, tlsCfg); err != nil && gCtx.Err() == nil {
				return fmt.Errorf("TLS listener: %w", err)
			}
			return nil
		})
	}

	return g.Wait()
}

// TLSPort returns the configured SIP TLS port (0 = disabled).
func (e *Engine) TLSPort() int { return e.tlsPort }

// ForkTarget identifies one branch of a parallel-forked INVITE: an explicit
// transport-layer destination (ip:port) that overrides the routing implied
// by the Request-URI's host. Used when dialing a registered AOR with one or
// more bound contacts; one ForkTarget per bound contact.
type ForkTarget struct {
	Socket    string // "ip:port"
	Transport string // "udp" | "tcp" | "tls"
}

// InviteOptions holds optional parameters for outbound INVITE.
type InviteOptions struct {
	Codecs       []codec.CodecType                              // Override engine codecs for this call; nil = use engine default
	Headers      []sip.Header                                   // Extra SIP headers to include in the INVITE
	FromUser     string                                         // Override the user part of the From header (caller ID)
	OnEarlyMedia func(remoteSDP *SDPMedia, rtpSess *RTPSession) // Called on first 183 with SDP
	AuthUsername string                                         // SIP digest auth username (optional)
	AuthPassword string                                         // SIP digest auth password (optional)

	// ForkTargets, when non-empty, routes the INVITE to the listed transport
	// addresses. A single target overrides the destination for a single
	// outbound dialog; multiple targets parallel-fork the INVITE and race
	// for the first 2xx (RFC 3261 §16).
	ForkTargets []ForkTarget

	// RTT (T.140 / RFC 4103) parameters. RTTEnabled offers m=text alongside
	// audio in the INVITE. RTTRedundancy controls the RFC 2198 RED depth
	// (0 = plain T.140, no RED).
	RTTEnabled    bool
	RTTRedundancy int
}

// Invite sends an outbound INVITE and returns an OutboundCall on success.
// When opts.ForkTargets has more than one entry the INVITE is parallel-forked
// (RFC 3261 §16); the first branch to reach a 2xx response wins and the others
// are CANCELled.
func (e *Engine) Invite(ctx context.Context, recipient sip.Uri, opts InviteOptions) (*OutboundCall, error) {
	if len(opts.ForkTargets) > 1 {
		return e.inviteFork(ctx, recipient, opts)
	}

	// Create RTP session for media
	rtpSess, err := NewRTPSessionFromAllocator(e.portAlloc)
	if err != nil {
		return nil, fmt.Errorf("create RTP session: %w", err)
	}

	var (
		textRtpSess *RTPSession
		ds          *sipgo.DialogClientSession
		committed   bool
		byeOnError  bool
	)
	defer func() {
		if committed {
			return
		}
		if byeOnError && ds != nil {
			ds.Bye(ctx)
		}
		rtpSess.Close()
		if textRtpSess != nil {
			textRtpSess.Close()
		}
	}()

	codecs := e.codecs
	if len(opts.Codecs) > 0 {
		codecs = opts.Codecs
	}

	e.log.Info("outbound INVITE", "recipient", recipient.String(), "codecs", fmt.Sprintf("%v", codecs))

	// Pick advertised IP family based on the resolved recipient host. Literal
	// hosts decide directly; hostnames go through the OS resolver.
	localIP := e.advertisedIPForRecipient(ctx, recipient.Host)

	// Optionally allocate a second RTP session for RTT (m=text).
	cfg := SDPConfig{
		LocalIP:           localIP,
		RTPPort:           rtpSess.LocalPort(),
		Codecs:            codecs,
		AMRWBOctetAligned: e.amrwbOctetAligned,
		AMRNBOctetAligned: e.amrnbOctetAligned,
	}
	if opts.RTTEnabled {
		ts, terr := NewRTPSessionFromAllocator(e.portAlloc)
		if terr != nil {
			return nil, fmt.Errorf("create text RTP session: %w", terr)
		}
		textRtpSess = ts
		cfg.TextRTPPort = ts.LocalPort()
		cfg.TextT140PT = 99
		cfg.TextREDPT = 98
		cfg.RTTRedundancy = opts.RTTRedundancy
	}

	// Generate SDP offer
	sdpOffer := GenerateOffer(cfg)

	// Build the INVITE request. We construct it manually so we can set
	// a proper typed FromHeader when FromUser is specified (appending a
	// generic "From" header would create a duplicate).
	req := sip.NewRequest(sip.INVITE, recipient)
	req.SetBody(sdpOffer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(e.AllowHeader())

	if opts.FromUser != "" {
		fromURI := sip.Uri{
			Scheme: "sip",
			User:   opts.FromUser,
			Host:   e.publicHost,
		}
		from := &sip.FromHeader{Address: fromURI}
		from.Params.Add("tag", sip.GenerateTagN(16))
		req.AppendHeader(from)
		req.AppendHeader(sip.NewHeader("P-Asserted-Identity", fromURI.String()))
	}

	for _, h := range opts.Headers {
		req.AppendHeader(h)
	}

	// Optional single-destination override (e.g. a single-contact AOR lookup
	// produced one ForkTarget). Sets the transport-layer destination without
	// touching the Request-URI.
	if len(opts.ForkTargets) == 1 {
		t := opts.ForkTargets[0]
		if t.Transport != "" {
			req.SetTransport(strings.ToUpper(t.Transport))
		}
		if t.Socket != "" {
			req.SetDestination(t.Socket)
		}
	}

	e.logSIPMessage("outbound", req)

	// Send INVITE via dialog client cache
	ds, err = e.dcCache.WriteInvite(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("invite: %w", err)
	}

	// Wait for 200 OK, processing provisional responses (183) for early media
	var earlyMediaSent bool
	answerOpts := sipgo.AnswerOptions{
		Username: opts.AuthUsername,
		Password: opts.AuthPassword,
	}
	answerOpts.OnResponse = func(res *sip.Response) error {
		e.logSIPMessage("inbound", res)
		if opts.OnEarlyMedia == nil {
			return nil
		}
		if earlyMediaSent {
			return nil
		}
		if res.StatusCode != sip.StatusSessionInProgress {
			return nil
		}
		body := res.Body()
		if len(body) == 0 {
			return nil
		}
		remoteSDP, err := ParseSDP(body)
		if err != nil {
			e.log.Warn("early media: parse 183 SDP failed", "error", err)
			return nil // non-fatal, keep waiting for 200
		}
		if err := rtpSess.SetRemote(remoteSDP.RemoteIP, remoteSDP.RemotePort); err != nil {
			e.log.Warn("early media: set remote failed", "error", err)
			return nil
		}
		earlyMediaSent = true
		// Send a burst of silence RTP for NAT port-latching before
		// the leg's media pipeline starts its own writeLoop.
		if len(remoteSDP.Codecs) > 0 {
			rtpSess.SendKeepalive(remoteSDP.Codecs[0].PayloadType(), 3)
		}
		opts.OnEarlyMedia(remoteSDP, rtpSess)
		return nil
	}
	if err := ds.WaitAnswer(ctx, answerOpts); err != nil {
		return nil, fmt.Errorf("wait answer: %w", err)
	}
	if ds.InviteResponse != nil {
		e.logSIPMessage("inbound", ds.InviteResponse)
	}

	// Send ACK
	if err := ds.Ack(ctx); err != nil {
		return nil, fmt.Errorf("ack: %w", err)
	}
	// Dialog fully established — any subsequent error must BYE.
	byeOnError = true

	// Parse answer SDP from 200 OK response
	remoteSDP, err := ParseSDP(ds.InviteResponse.Body())
	if err != nil {
		return nil, fmt.Errorf("parse answer SDP: %w", err)
	}

	// Set remote RTP address
	if err := rtpSess.SetRemote(remoteSDP.RemoteIP, remoteSDP.RemotePort); err != nil {
		return nil, fmt.Errorf("set remote: %w", err)
	}

	// Send a burst of silence RTP for NAT port-latching. The leg's full
	// media pipeline (writeLoop) starts shortly after, but this ensures
	// the first packets go out immediately after we learn the remote address.
	if len(remoteSDP.Codecs) > 0 {
		rtpSess.SendKeepalive(remoteSDP.Codecs[0].PayloadType(), 3)
	}

	// Wire the text RTP session to the answer's text section, if accepted.
	// Mirrors the acceptance test in SIPLeg.adoptOutboundTextSession so the
	// engine never returns a session the leg will discard.
	if textRtpSess != nil {
		if remoteSDP.Text != nil && remoteSDP.Text.RemotePort != 0 && remoteSDP.Text.T140PT != 0 {
			if err := textRtpSess.SetRemote(remoteSDP.Text.RemoteIP, remoteSDP.Text.RemotePort); err != nil {
				e.log.Warn("text RTP set remote failed", "error", err)
				textRtpSess.Close()
				textRtpSess = nil
			}
		} else {
			// Peer rejected RTT (no m=text, port=0, or no t140/1000 codec).
			textRtpSess.Close()
			textRtpSess = nil
		}
	}

	// Parse session timer from 200 OK if present.
	var sessionTimer *SessionTimerParams
	if seHdr := ds.InviteResponse.GetHeader("Session-Expires"); seHdr != nil {
		interval, refresher := ParseSessionExpires(seHdr.Value())
		if interval > 0 {
			if refresher == "" {
				refresher = "uac" // we are UAC
			}
			sessionTimer = &SessionTimerParams{
				Interval:  interval,
				Refresher: refresher,
			}
		}
	}

	committed = true
	return &OutboundCall{
		Dialog:       ds,
		RemoteSDP:    remoteSDP,
		RTPSess:      rtpSess,
		TextRTPSess:  textRtpSess,
		SessionTimer: sessionTimer,
	}, nil
}

// Codecs returns the engine's supported codecs.
func (e *Engine) Codecs() []codec.CodecType {
	return e.codecs
}

// AMRWBMode returns the configured AMR-WB encoder speech mode (0..8).
func (e *Engine) AMRWBMode() int { return e.amrwbMode }

// AMRWBOctetAligned reports whether AMR-WB offers advertise octet-aligned framing.
func (e *Engine) AMRWBOctetAligned() bool { return e.amrwbOctetAligned }

// AMRNBMode returns the configured AMR-NB encoder speech mode (0..7).
func (e *Engine) AMRNBMode() int { return e.amrnbMode }

// AMRNBOctetAligned reports whether AMR-NB offers advertise octet-aligned framing.
func (e *Engine) AMRNBOctetAligned() bool { return e.amrnbOctetAligned }

// BindIP returns the engine's IPv4 advertised address. May be empty in
// IPv6-only deployments.
func (e *Engine) BindIP() string {
	return e.bindIP
}

// BindIPV6 returns the engine's IPv6 advertised address. Empty when no v6
// is configured.
func (e *Engine) BindIPV6() string {
	return e.bindIPV6
}

// AdvertisedIPForFamily returns the configured advertised IP for the given
// SDP address family token ("IP4" or "IP6"). Falls back to the other family
// when the requested one is unconfigured. Returns empty when neither is
// configured (caller should reject the call).
func (e *Engine) AdvertisedIPForFamily(family string) string {
	switch family {
	case "IP6":
		if e.bindIPV6 != "" {
			return e.bindIPV6
		}
		return e.bindIP
	default:
		if e.bindIP != "" {
			return e.bindIP
		}
		return e.bindIPV6
	}
}

// advertisedIPForRecipient picks the advertised IP for an outbound INVITE
// based on the resolved family of the target host. Literal hosts decide
// directly; hostnames are resolved with a short timeout and fall back to
// the IPv4 advertised IP on failure (preserves prior behavior).
func (e *Engine) advertisedIPForRecipient(ctx context.Context, host string) string {
	// SIP URI hosts may carry the IPv6 brackets ("[::1]"); strip for parsing.
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if family := AddressFamily(host); family != "" {
		return e.AdvertisedIPForFamily(family)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil || len(addrs) == 0 {
		return e.AdvertisedIPForFamily("IP4")
	}
	for _, a := range addrs {
		if a.IP.To4() != nil {
			if e.bindIP != "" {
				return e.bindIP
			}
			break
		}
	}
	for _, a := range addrs {
		if a.IP.To4() == nil && a.IP.To16() != nil {
			if e.bindIPV6 != "" {
				return e.bindIPV6
			}
			break
		}
	}
	return e.AdvertisedIPForFamily("IP4")
}

// PublicHost returns the canonical signalling hostname (SIP_DOMAIN when
// set, otherwise the advertised IP).
func (e *Engine) PublicHost() string {
	return e.publicHost
}

func (e *Engine) SIPHost() string {
	return e.sipHost
}

// ServerHeader returns a SIP Server header for UAS responses.
func (e *Engine) ServerHeader() sip.Header {
	return sip.NewHeader("Server", e.sipHost)
}

// AllowHeader returns an Allow header listing every SIP method this UA
// answers (RFC 3261 §20.5). The list comes from sipgo's registered request
// handlers so it can never drift from the methods we actually accept.
func (e *Engine) AllowHeader() sip.Header {
	methods := e.server.RegisteredMethods()
	if len(methods) == 0 {
		return sip.NewHeader("Allow", "")
	}
	order := []string{"INVITE", "ACK", "CANCEL", "BYE", "UPDATE", "INFO", "PRACK", "REFER", "NOTIFY", "OPTIONS", "MESSAGE", "REGISTER", "SUBSCRIBE", "PUBLISH"}
	seen := make(map[string]bool, len(methods))
	for _, m := range methods {
		seen[m] = true
	}
	out := make([]string, 0, len(methods))
	for _, m := range order {
		if seen[m] {
			out = append(out, m)
			delete(seen, m)
		}
	}
	for m := range seen {
		out = append(out, m)
	}
	return sip.NewHeader("Allow", strings.Join(out, ", "))
}

// PortAllocator returns the engine's port allocator (nil if OS-assigned).
func (e *Engine) PortAllocator() *PortAllocator {
	return e.portAlloc
}
