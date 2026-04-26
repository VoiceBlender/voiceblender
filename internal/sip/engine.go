package sip

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

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
	BindIP        string // IP advertised in SDP/Contact/Via headers
	ListenIP      string // IP to bind the UDP socket on (default: same as BindIP)
	ExternalIP    string // Public IP override for NAT/Docker (used in Contact/SDP/Via when set)
	BindPort      int
	TLSBindPort   int    // 0 = TLS disabled
	TLSCertPath   string // CA-signed cert (fullchain.pem) — required when TLSBindPort > 0
	TLSKeyPath    string // private key (privkey.pem) — required when TLSBindPort > 0
	SIPDebug      bool   // dump full SIP request/response bodies on the debug channel
	SIPHost       string
	Codecs        []codec.CodecType
	Log           *slog.Logger
	PortAllocator *PortAllocator // nil = OS-assigned ports
}

// Engine wraps sipgo server/client + dialog caches for SIP signaling.
type Engine struct {
	ua      *sipgo.UserAgent
	server  *sipgo.Server
	client  *sipgo.Client
	dsCache *sipgo.DialogServerCache
	dcCache *sipgo.DialogClientCache

	onInvite   func(call *InboundCall)
	onReInvite func(callID string, direction string) []byte // returns SDP answer for 200 OK
	onRefer    func(callID string, target string, replaces *ReplacesParams, req *sip.Request, tx sip.ServerTransaction)
	onNotify   func(callID string, statusCode int, reason string, terminated bool)
	codecs     []codec.CodecType
	bindIP     string // externally-reachable IP (for SDP/Contact)
	listenIP   string // original bind address (for ListenAndServe)
	bindPort   int
	tlsPort    int // 0 = TLS disabled
	tlsCert    string
	tlsKey     string
	sipHost    string
	portAlloc  *PortAllocator
	log        *slog.Logger
	sipDebug   bool
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

// contactForInvite returns a Contact that matches the transport on which
// the INVITE arrived — sips:<ip>:<tlsPort> for TLS, sip:<ip>:<udpPort>
// otherwise. When no TLS port is configured it always returns the sip:
// form so classic SIP behaviour is unchanged.
func (e *Engine) contactForInvite(req *sip.Request) *sip.ContactHeader {
	if inviteIsTLS(req) && e.tlsPort != 0 {
		return &sip.ContactHeader{Address: sip.Uri{Scheme: "sips", Host: e.bindIP, Port: e.tlsPort}}
	}
	return &sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: e.bindIP, Port: e.bindPort}}
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
	res.AppendHeader(e.contactForInvite(dialog.InviteRequest))
	res.SetBody(sdp)

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

	// Session timer (RFC 4028) — populated when remote's 200 OK includes timers.
	SessionTimer *SessionTimerParams // nil when remote didn't include timers
}

// resolveExternalIP detects the preferred outbound LAN IP.
// No traffic is sent — UDP connect only sets routing.
func resolveExternalIP() (string, error) {
	conn, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// NewEngine creates a SIP engine with the given configuration.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	advertiseIP := cfg.BindIP
	listenIP := cfg.ListenIP

	// Auto-detect when BindIP is unroutable
	if advertiseIP == "" || advertiseIP == "0.0.0.0" || advertiseIP == "::" {
		detected, err := resolveExternalIP()
		if err != nil {
			return nil, fmt.Errorf("SIP_BIND_IP is %q; auto-detect failed: %w", cfg.BindIP, err)
		}
		if listenIP == "" {
			listenIP = advertiseIP // keep the wildcard for the socket
		}
		advertiseIP = detected
	}

	if listenIP == "" {
		listenIP = advertiseIP
	}

	// Explicit external IP overrides advertised IP (NAT/Docker).
	if cfg.ExternalIP != "" {
		advertiseIP = cfg.ExternalIP
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
		sipgo.WithUserAgentHostname(advertiseIP),
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

	// Pin Via sent-by to advertiseIP — wildcard binds make the response
	// path unroutable, so peers black-hole our REFER/BYE/re-INVITE 200s.
	clientOpts := []sipgo.ClientOption{
		sipgo.WithClientHostname(advertiseIP),
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
			Host:   advertiseIP,
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

	e := &Engine{
		ua:        ua,
		server:    server,
		client:    client,
		dsCache:   sipgo.NewDialogServerCache(client, contactHdr),
		dcCache:   sipgo.NewDialogClientCache(client, contactHdr),
		codecs:    cfg.Codecs,
		bindIP:    advertiseIP,
		listenIP:  listenIP,
		bindPort:  cfg.BindPort,
		tlsPort:   cfg.TLSBindPort,
		tlsCert:   cfg.TLSCertPath,
		tlsKey:    cfg.TLSKeyPath,
		sipHost:   cfg.SIPHost,
		portAlloc: cfg.PortAllocator,
		log:       cfg.Log,
		sipDebug:  cfg.SIPDebug,
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
		tx.Respond(res)
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
	if err := tx.Respond(res); err != nil {
		e.log.Error("re-INVITE: respond failed", "error", err)
		return
	}

	e.log.Info("re-INVITE handled", "call_id", callID.Value(), "direction", direction)
}

// SendReInvite sends a re-INVITE within an existing dialog for hold/unhold.
// dialog must be either *sipgo.DialogServerSession or *sipgo.DialogClientSession.
func (e *Engine) SendReInvite(ctx context.Context, dialog interface{}, sdpBody []byte) error {
	switch d := dialog.(type) {
	case *sipgo.DialogServerSession:
		req := sip.NewRequest(sip.INVITE, d.InviteRequest.Contact().Address)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
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
			tx.Respond(res)
			return
		}

		remoteSDP, err := ParseSDP(req.Body())
		if err != nil {
			e.log.Error("parse offer SDP failed", "error", err)
			ds.Respond(sip.StatusBadRequest, "Bad SDP", nil, e.ServerHeader())
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
		tx.Respond(res)
	}))

	e.server.OnRefer(wrap(e.handleRefer))
	e.server.OnNotify(wrap(e.handleNotify))
}

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
// either fails the other is torn down via ctx cancellation.
func (e *Engine) Serve(ctx context.Context) error {
	udpAddr := fmt.Sprintf("%s:%d", e.listenIP, e.bindPort)

	if e.tlsPort == 0 {
		return e.server.ListenAndServe(ctx, "udp", udpAddr)
	}

	cert, err := tls.LoadX509KeyPair(e.tlsCert, e.tlsKey)
	if err != nil {
		return fmt.Errorf("load TLS cert: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	tlsAddr := fmt.Sprintf("%s:%d", e.listenIP, e.tlsPort)

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := e.server.ListenAndServe(gCtx, "udp", udpAddr); err != nil && gCtx.Err() == nil {
			return fmt.Errorf("UDP listener: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := e.server.ListenAndServeTLS(gCtx, "tls", tlsAddr, tlsCfg); err != nil && gCtx.Err() == nil {
			return fmt.Errorf("TLS listener: %w", err)
		}
		return nil
	})
	return g.Wait()
}

// TLSPort returns the configured SIP TLS port (0 = disabled).
func (e *Engine) TLSPort() int { return e.tlsPort }

// InviteOptions holds optional parameters for outbound INVITE.
type InviteOptions struct {
	Codecs       []codec.CodecType                              // Override engine codecs for this call; nil = use engine default
	Headers      []sip.Header                                   // Extra SIP headers to include in the INVITE
	FromUser     string                                         // Override the user part of the From header (caller ID)
	OnEarlyMedia func(remoteSDP *SDPMedia, rtpSess *RTPSession) // Called on first 183 with SDP
	AuthUsername string                                         // SIP digest auth username (optional)
	AuthPassword string                                         // SIP digest auth password (optional)
}

// Invite sends an outbound INVITE and returns an OutboundCall on success.
func (e *Engine) Invite(ctx context.Context, recipient sip.Uri, opts InviteOptions) (*OutboundCall, error) {
	// Create RTP session for media
	rtpSess, err := NewRTPSessionFromAllocator(e.portAlloc)
	if err != nil {
		return nil, fmt.Errorf("create RTP session: %w", err)
	}

	codecs := e.codecs
	if len(opts.Codecs) > 0 {
		codecs = opts.Codecs
	}

	e.log.Info("outbound INVITE", "recipient", recipient.String(), "codecs", fmt.Sprintf("%v", codecs))

	// Generate SDP offer
	sdpOffer := GenerateOffer(SDPConfig{
		LocalIP: e.bindIP,
		RTPPort: rtpSess.LocalPort(),
		Codecs:  codecs,
	})

	// Build the INVITE request. We construct it manually so we can set
	// a proper typed FromHeader when FromUser is specified (appending a
	// generic "From" header would create a duplicate).
	req := sip.NewRequest(sip.INVITE, recipient)
	req.SetBody(sdpOffer)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	if opts.FromUser != "" {
		fromURI := sip.Uri{
			Scheme: "sip",
			User:   opts.FromUser,
			Host:   e.bindIP,
		}
		from := &sip.FromHeader{Address: fromURI}
		from.Params.Add("tag", sip.GenerateTagN(16))
		req.AppendHeader(from)
		req.AppendHeader(sip.NewHeader("P-Asserted-Identity", fromURI.String()))
	}

	for _, h := range opts.Headers {
		req.AppendHeader(h)
	}

	e.logSIPMessage("outbound", req)

	// Send INVITE via dialog client cache
	ds, err := e.dcCache.WriteInvite(ctx, req)
	if err != nil {
		rtpSess.Close()
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
		rtpSess.Close()
		return nil, fmt.Errorf("wait answer: %w", err)
	}
	if ds.InviteResponse != nil {
		e.logSIPMessage("inbound", ds.InviteResponse)
	}

	// Send ACK
	if err := ds.Ack(ctx); err != nil {
		rtpSess.Close()
		return nil, fmt.Errorf("ack: %w", err)
	}

	// Parse answer SDP from 200 OK response
	remoteSDP, err := ParseSDP(ds.InviteResponse.Body())
	if err != nil {
		rtpSess.Close()
		ds.Bye(ctx)
		return nil, fmt.Errorf("parse answer SDP: %w", err)
	}

	// Set remote RTP address
	if err := rtpSess.SetRemote(remoteSDP.RemoteIP, remoteSDP.RemotePort); err != nil {
		rtpSess.Close()
		ds.Bye(ctx)
		return nil, fmt.Errorf("set remote: %w", err)
	}

	// Send a burst of silence RTP for NAT port-latching. The leg's full
	// media pipeline (writeLoop) starts shortly after, but this ensures
	// the first packets go out immediately after we learn the remote address.
	if len(remoteSDP.Codecs) > 0 {
		rtpSess.SendKeepalive(remoteSDP.Codecs[0].PayloadType(), 3)
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

	return &OutboundCall{
		Dialog:       ds,
		RemoteSDP:    remoteSDP,
		RTPSess:      rtpSess,
		SessionTimer: sessionTimer,
	}, nil
}

// Codecs returns the engine's supported codecs.
func (e *Engine) Codecs() []codec.CodecType {
	return e.codecs
}

// BindIP returns the engine's bind IP address.
func (e *Engine) BindIP() string {
	return e.bindIP
}

func (e *Engine) SIPHost() string {
	return e.sipHost
}

// ServerHeader returns a SIP Server header for UAS responses.
func (e *Engine) ServerHeader() sip.Header {
	return sip.NewHeader("Server", e.sipHost)
}

// PortAllocator returns the engine's port allocator (nil if OS-assigned).
func (e *Engine) PortAllocator() *PortAllocator {
	return e.portAlloc
}
