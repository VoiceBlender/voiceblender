package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/csiwek/VoiceBlender/internal/codec"
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

	onInvite func(call *InboundCall)
	codecs   []codec.CodecType
	bindIP   string // externally-reachable IP (for SDP/Contact)
	listenIP string // original bind address (for ListenAndServe)
	bindPort int
	log      *slog.Logger
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

func (e *Engine) registerHandlers() {
	e.server.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
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
	Codecs  []codec.CodecType // Override engine codecs for this call; nil = use engine default
	Headers []sip.Header      // Extra SIP headers to include in the INVITE
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

	// Prepend Content-Type for SDP body (sipgo does not add it automatically).
	headers := make([]sip.Header, 0, len(opts.Headers)+1)
	headers = append(headers, sip.NewHeader("Content-Type", "application/sdp"))
	headers = append(headers, opts.Headers...)

	// Send INVITE via dialog client cache
	ds, err := e.dcCache.Invite(ctx, recipient, sdpOffer, headers...)
	if err != nil {
		rtpSess.Close()
		return nil, fmt.Errorf("invite: %w", err)
	}

	// Wait for 200 OK
	if err := ds.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
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
