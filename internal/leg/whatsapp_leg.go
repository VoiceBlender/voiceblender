package leg

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	sipproto "github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
)

// WhatsAppSIPController is satisfied by *sip.Engine and lets a WhatsApp leg
// (a) dump outbound responses when SIP_DEBUG is on and (b) send the 2xx
// answer with a transport-appropriate Contact header. The interface lives
// here so the leg package doesn't import internal/sip (which would invert
// the dependency).
type WhatsAppSIPController interface {
	LogSyntheticResponse(req *sipproto.Request, statusCode int, reason string, body []byte, headers ...sipproto.Header)
	RespondInviteSDP(dialog *sipgo.DialogServerSession, sdp []byte) error
}

// SIPResponseLogger is kept as an alias for WhatsAppSIPController so existing
// callers that only need logging still compile.
type SIPResponseLogger = WhatsAppSIPController

// WhatsAppLeg is a call leg terminated to WhatsApp Business Calling. Signalling
// is SIP over TLS with digest auth; media is Opus over ICE + DTLS-SRTP,
// delegated to PCMedia. Hold, unhold and blind/attended transfer are
// explicitly unsupported because Meta's SIP implementation rejects re-INVITEs.
type WhatsAppLeg struct {
	id      string
	legType LegType
	state   LegState
	mu      sync.RWMutex

	media *PCMedia

	// Exactly one of serverDialog / clientDialog is set: inbound calls hold a
	// UAS dialog (we answer the INVITE); outbound calls hold a UAC dialog.
	serverDialog *sipgo.DialogServerSession
	clientDialog *sipgo.DialogClientSession

	from       string
	to         string
	sipHeaders map[string]string

	roomID     string
	appID      string
	muted      atomic.Bool
	deaf       atomic.Bool
	acceptDTMF atomic.Bool

	createdAt  time.Time
	answeredAt time.Time

	// Inbound only: AnswerCh unblocks HandleInboundCall once the caller issues
	// POST /v1/legs/{id}/answer. AnswerSDP is the SDP answer to send in the 200 OK.
	answerCh  chan struct{}
	answerSDP []byte

	// SIP controller: handles outbound-response logging and the 2xx send
	// with a transport-appropriate Contact. Required for inbound legs.
	sipCtrl WhatsAppSIPController

	onDTMF func(digit rune)
	log    *slog.Logger
}

// SetSIPController wires the engine-backed helper that knows how to send
// the 200 OK with a sips: Contact on TLS (mandatory for WhatsApp inbound).
// Also enables SIP_DEBUG dumping for outbound responses.
func (l *WhatsAppLeg) SetSIPController(c WhatsAppSIPController) { l.sipCtrl = c }

// SetSIPResponseLogger is an alias retained for call-site compatibility.
func (l *WhatsAppLeg) SetSIPResponseLogger(c SIPResponseLogger) { l.sipCtrl = c }

// NewWhatsAppInboundLeg wraps an already-accepted inbound UAS dialog and the
// PCMedia that has negotiated the SDP answer. The answer is NOT sent here;
// the 200 OK is sent by Answer() so REST callers control when to pick up.
func NewWhatsAppInboundLeg(dialog *sipgo.DialogServerSession, media *PCMedia, from, to string, headers map[string]string, answerSDP []byte, log *slog.Logger) *WhatsAppLeg {
	l := &WhatsAppLeg{
		id:           uuid.New().String(),
		legType:      TypeWhatsAppInbound,
		state:        StateRinging,
		media:        media,
		serverDialog: dialog,
		from:         from,
		to:           to,
		sipHeaders:   headers,
		createdAt:    time.Now(),
		answerCh:     make(chan struct{}),
		answerSDP:    answerSDP,
		log:          log,
	}
	l.acceptDTMF.Store(true)
	return l
}

// NewWhatsAppOutboundPendingLeg creates an outbound leg in StateRinging
// without a SIP dialog. Caller drives the INVITE asynchronously and
// upgrades the leg via ConnectOutbound once a 200 OK is received.
func NewWhatsAppOutboundPendingLeg(media *PCMedia, from, to string, log *slog.Logger) *WhatsAppLeg {
	l := &WhatsAppLeg{
		id:        uuid.New().String(),
		legType:   TypeWhatsAppOutbound,
		state:     StateRinging,
		media:     media,
		from:      from,
		to:        to,
		createdAt: time.Now(),
		log:       log,
	}
	l.acceptDTMF.Store(true)
	return l
}

// ConnectOutbound transitions a pending outbound leg to StateConnected
// once the UAC INVITE has been answered. media.Start() is called here so
// no RTP egresses while the leg is still ringing.
func (l *WhatsAppLeg) ConnectOutbound(dialog *sipgo.DialogClientSession) error {
	l.mu.Lock()
	if l.state == StateHungUp {
		l.mu.Unlock()
		return fmt.Errorf("leg already hung up")
	}
	if l.state == StateConnected {
		l.mu.Unlock()
		return nil
	}
	l.clientDialog = dialog
	l.state = StateConnected
	l.answeredAt = time.Now()
	l.mu.Unlock()
	l.media.Start()
	return nil
}

// Media returns the underlying PCMedia for ICE trickle / diagnostics.
func (l *WhatsAppLeg) Media() *PCMedia { return l.media }

// AnswerCh signals that a REST Answer call has arrived (inbound only).
func (l *WhatsAppLeg) AnswerCh() <-chan struct{} { return l.answerCh }

// ServerDialog returns the UAS dialog for inbound calls (nil for outbound).
func (l *WhatsAppLeg) ServerDialog() *sipgo.DialogServerSession { return l.serverDialog }

// ClientDialog returns the UAC dialog for outbound calls (nil for inbound).
func (l *WhatsAppLeg) ClientDialog() *sipgo.DialogClientSession { return l.clientDialog }

func (l *WhatsAppLeg) ID() string      { return l.id }
func (l *WhatsAppLeg) Type() LegType   { return l.legType }
func (l *WhatsAppLeg) SampleRate() int { return l.media.SampleRate() }

func (l *WhatsAppLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *WhatsAppLeg) Context() context.Context { return l.media.Context() }

func (l *WhatsAppLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *WhatsAppLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *WhatsAppLeg) AppID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.appID
}

func (l *WhatsAppLeg) SetAppID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appID = id
}

func (l *WhatsAppLeg) IsMuted() bool              { return l.muted.Load() }
func (l *WhatsAppLeg) SetMuted(m bool)            { l.muted.Store(m) }
func (l *WhatsAppLeg) IsDeaf() bool               { return l.deaf.Load() }
func (l *WhatsAppLeg) SetDeaf(d bool)             { l.deaf.Store(d) }
func (l *WhatsAppLeg) AcceptDTMF() bool           { return l.acceptDTMF.Load() }
func (l *WhatsAppLeg) SetAcceptDTMF(a bool)       { l.acceptDTMF.Store(a) }
func (l *WhatsAppLeg) SetSpeakingTap(w io.Writer) { l.media.SetSpeakingTap(w) }
func (l *WhatsAppLeg) ClearSpeakingTap()          { l.media.ClearSpeakingTap() }
func (l *WhatsAppLeg) IsHeld() bool               { return false }

func (l *WhatsAppLeg) CreatedAt() time.Time  { return l.createdAt }
func (l *WhatsAppLeg) AnsweredAt() time.Time { return l.answeredAt }
func (l *WhatsAppLeg) SIPHeaders() map[string]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]string, len(l.sipHeaders))
	for k, v := range l.sipHeaders {
		out[k] = v
	}
	return out
}
func (l *WhatsAppLeg) RTPStats() RTPStats { return RTPStats{} }

// From returns the caller identity (remote for inbound, business number for outbound).
func (l *WhatsAppLeg) From() string { return l.from }

// To returns the callee identity.
func (l *WhatsAppLeg) To() string { return l.to }

// RequestAnswer signals from the REST layer that the caller should be answered
// (inbound only). Second calls are no-ops.
func (l *WhatsAppLeg) RequestAnswer() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.answerCh == nil {
		return fmt.Errorf("outbound leg: nothing to answer")
	}
	if l.state != StateRinging && l.state != StateEarlyMedia {
		return fmt.Errorf("leg is %s, expected ringing", l.state)
	}
	select {
	case <-l.answerCh:
		return fmt.Errorf("already answering")
	default:
		close(l.answerCh)
	}
	return nil
}

// Answer finalises an inbound call by sending the 200 OK with the SDP answer
// that was prepared during INVITE processing.
func (l *WhatsAppLeg) Answer(_ context.Context) error {
	l.mu.Lock()
	if l.answerCh == nil {
		l.mu.Unlock()
		return fmt.Errorf("outbound leg: Answer not applicable")
	}
	if l.state == StateConnected {
		l.mu.Unlock()
		return nil
	}
	dialog := l.serverDialog
	sdp := l.answerSDP
	l.mu.Unlock()

	if dialog != nil {
		// Use the engine-backed sender when wired (inbound legs from the
		// API layer always have this set). It attaches a transport-aware
		// sips: Contact so Meta can route the ACK back over TLS.
		if l.sipCtrl != nil {
			if err := l.sipCtrl.RespondInviteSDP(dialog, sdp); err != nil {
				return fmt.Errorf("respond 200 OK: %w", err)
			}
		} else {
			if err := dialog.RespondSDP(sdp); err != nil {
				return fmt.Errorf("respond 200 OK: %w", err)
			}
		}
	}

	l.mu.Lock()
	l.state = StateConnected
	l.answeredAt = time.Now()
	l.mu.Unlock()

	l.media.Start()
	return nil
}

func (l *WhatsAppLeg) Hangup(ctx context.Context) error {
	l.mu.Lock()
	if l.state == StateHungUp {
		l.mu.Unlock()
		return nil
	}
	l.state = StateHungUp
	server := l.serverDialog
	client := l.clientDialog
	l.mu.Unlock()

	if server != nil {
		_ = server.Bye(ctx)
	}
	if client != nil {
		_ = client.Bye(ctx)
	}
	return l.media.Close()
}

func (l *WhatsAppLeg) OnDTMF(f func(digit rune)) {
	l.mu.Lock()
	l.onDTMF = f
	l.mu.Unlock()
	l.media.SetOnDTMF(f)
}

func (l *WhatsAppLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF send over WhatsApp not yet implemented")
}

func (l *WhatsAppLeg) AudioReader() io.Reader { return l.media.AudioReader() }
func (l *WhatsAppLeg) AudioWriter() io.Writer { return l.media.AudioWriter() }
