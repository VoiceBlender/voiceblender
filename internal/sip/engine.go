package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
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
	sipHost    string
	portAlloc  *PortAllocator
	log        *slog.Logger
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

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.SIPHost),
		sipgo.WithUserAgentHostname(advertiseIP),
	)
	if err != nil {
		return nil, fmt.Errorf("create UA: %w", err)
	}

	server, err := sipgo.NewServer(ua)
	if err != nil {
		return nil, fmt.Errorf("create server: %w", err)
	}

	// Pin Via sent-by to advertiseIP — wildcard binds make the response
	// path unroutable, so peers black-hole our REFER/BYE/re-INVITE 200s.
	client, err := sipgo.NewClient(ua,
		sipgo.WithClientHostname(advertiseIP),
		sipgo.WithClientPort(cfg.BindPort),
	)
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
		sipHost:   cfg.SIPHost,
		portAlloc: cfg.PortAllocator,
		log:       cfg.Log,
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
	e.server.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
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
	})

	e.server.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := e.dsCache.ReadAck(req, tx); err != nil {
			e.log.Debug("read ack failed", "error", err)
		}
	})

	e.server.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		if err := e.dsCache.ReadBye(req, tx); err != nil {
			if err := e.dcCache.ReadBye(req, tx); err != nil {
				// RFC 3261 §8.2.2.1
				e.log.Debug("BYE: no matching dialog, replying 481", "error", err)
				if rerr := e.respondFromSource(tx, req, 481, "Call/Transaction Does Not Exist"); rerr != nil {
					e.log.Error("BYE: respond 481 failed", "error", rerr)
				}
			}
		}
	})

	e.server.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		// This handler fires only for CANCELs that didn't match an active
		// INVITE transaction.  For matched CANCELs, sipgo's transaction
		// layer handles both 487 (for INVITE) and 200 OK (for CANCEL)
		// automatically.  Respond 481 per RFC 3261 §9.2.
		res := sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)
		res.AppendHeader(e.ServerHeader())
		tx.Respond(res)
	})

	e.server.OnRefer(e.handleRefer)
	e.server.OnNotify(e.handleNotify)
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

// Serve starts the SIP server and blocks until ctx is cancelled.
func (e *Engine) Serve(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", e.listenIP, e.bindPort)
	return e.server.ListenAndServe(ctx, "udp", addr)
}

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
	if opts.OnEarlyMedia != nil {
		answerOpts.OnResponse = func(res *sip.Response) error {
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
	}
	if err := ds.WaitAnswer(ctx, answerOpts); err != nil {
		rtpSess.Close()
		return nil, fmt.Errorf("wait answer: %w", err)
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
