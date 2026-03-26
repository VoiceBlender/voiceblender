package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// EngineConfig holds configuration for the SIP engine.
type EngineConfig struct {
	BindIP   string // IP advertised in SDP/Contact/Via headers
	ListenIP string // IP to bind the UDP socket on (default: same as BindIP)
	BindPort int
	SIPHost  string
	Codecs   []codec.CodecType
	Log      *slog.Logger
}

// Engine wraps sipgo server/client + dialog caches for SIP signaling.
type Engine struct {
	ua      *sipgo.UserAgent
	server  *sipgo.Server
	client  *sipgo.Client
	dsCache *sipgo.DialogServerCache
	dcCache *sipgo.DialogClientCache

	onInvite   func(call *InboundCall)
	onReInvite func(callID string, direction string)
	codecs     []codec.CodecType
	bindIP     string // externally-reachable IP (for SDP/Contact)
	listenIP   string // original bind address (for ListenAndServe)
	bindPort   int
	log        *slog.Logger
}

// InboundCall wraps a sipgo DialogServerSession with parsed SDP.
type InboundCall struct {
	Dialog    *sipgo.DialogServerSession
	From      string     // caller URI user part
	To        string     // callee URI user part
	RemoteSDP *SDPMedia  // parsed offer SDP
	Request   *sip.Request
}

// OutboundCall wraps a sipgo DialogClientSession with parsed answer SDP.
type OutboundCall struct {
	Dialog    *sipgo.DialogClientSession
	RemoteSDP *SDPMedia
	RTPSess   *RTPSession
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

	client, err := sipgo.NewClient(ua)
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
		ua:       ua,
		server:   server,
		client:   client,
		dsCache:  sipgo.NewDialogServerCache(client, contactHdr),
		dcCache:  sipgo.NewDialogClientCache(client, contactHdr),
		codecs:   cfg.Codecs,
		bindIP:   advertiseIP,
		listenIP: listenIP,
		bindPort: cfg.BindPort,
		log:      cfg.Log,
	}

	e.registerHandlers()
	return e, nil
}

// OnInvite registers a handler for inbound INVITE requests.
func (e *Engine) OnInvite(handler func(*InboundCall)) {
	e.onInvite = handler
}

// OnReInvite registers a handler for in-dialog re-INVITE requests (hold/unhold).
// The handler receives the SIP Call-ID and the SDP direction attribute.
func (e *Engine) OnReInvite(handler func(callID string, direction string)) {
	e.onReInvite = handler
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

	// Respond 200 OK to the re-INVITE.
	// We don't change our media, so respond without SDP body.
	// Many endpoints accept a 200 OK without SDP for re-INVITEs.
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	if err := tx.Respond(res); err != nil {
		e.log.Error("re-INVITE: respond failed", "error", err)
		return
	}

	e.log.Info("re-INVITE handled", "call_id", callID.Value(), "direction", direction)

	if e.onReInvite != nil {
		e.onReInvite(callID.Value(), direction)
	}
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
			tx.Respond(res)
			return
		}

		remoteSDP, err := ParseSDP(req.Body())
		if err != nil {
			e.log.Error("parse offer SDP failed", "error", err)
			ds.Respond(sip.StatusBadRequest, "Bad SDP", nil)
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

		call := &InboundCall{
			Dialog:    ds,
			From:      from,
			To:        to,
			RemoteSDP: remoteSDP,
			Request:   req,
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
		// Try inbound dialog cache first, then outbound
		if err := e.dsCache.ReadBye(req, tx); err != nil {
			if err := e.dcCache.ReadBye(req, tx); err != nil {
				e.log.Debug("read bye: no matching dialog", "error", err)
			}
		}
	})

	e.server.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		// Respond 200 OK to the CANCEL request
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		tx.Respond(res)
	})
}

// Serve starts the SIP server and blocks until ctx is cancelled.
func (e *Engine) Serve(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", e.listenIP, e.bindPort)
	return e.server.ListenAndServe(ctx, "udp", addr)
}

// InviteOptions holds optional parameters for outbound INVITE.
type InviteOptions struct {
	Codecs       []codec.CodecType // Override engine codecs for this call; nil = use engine default
	Headers      []sip.Header      // Extra SIP headers to include in the INVITE
	FromUser     string            // Override the user part of the From header (caller ID)
	OnEarlyMedia func(remoteSDP *SDPMedia, rtpSess *RTPSession) // Called on first 183 with SDP
}

// Invite sends an outbound INVITE and returns an OutboundCall on success.
func (e *Engine) Invite(ctx context.Context, recipient sip.Uri, opts InviteOptions) (*OutboundCall, error) {
	// Create RTP session for media
	rtpSess, err := NewRTPSession()
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
	answerOpts := sipgo.AnswerOptions{}
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

	return &OutboundCall{
		Dialog:    ds,
		RemoteSDP: remoteSDP,
		RTPSess:   rtpSess,
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
