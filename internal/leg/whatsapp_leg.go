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

// WhatsAppSIPController is satisfied by *sip.Engine. The interface lives
// here to avoid a cyclic import on internal/sip.
type WhatsAppSIPController interface {
	LogSyntheticResponse(req *sipproto.Request, statusCode int, reason string, body []byte, headers ...sipproto.Header)
	RespondInviteSDP(dialog *sipgo.DialogServerSession, sdp []byte) error
}

// SIPResponseLogger is an alias retained for older call sites.
type SIPResponseLogger = WhatsAppSIPController

// WhatsAppLeg is a SIP-over-TLS leg with Opus media via ICE+DTLS-SRTP.
// Hold/unhold/transfer are unsupported (Meta rejects re-INVITE).
type WhatsAppLeg struct {
	id      string
	legType LegType
	state   LegState
	mu      sync.RWMutex

	media *PCMedia

	// Exactly one of serverDialog/clientDialog is set.
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

	// Inbound only.
	answerCh  chan struct{}
	answerSDP []byte

	sipCtrl WhatsAppSIPController

	onDTMF func(digit rune)
	log    *slog.Logger
}

func (l *WhatsAppLeg) SetSIPController(c WhatsAppSIPController) { l.sipCtrl = c }
func (l *WhatsAppLeg) SetSIPResponseLogger(c SIPResponseLogger) { l.sipCtrl = c }

// NewWhatsAppInboundLeg wraps an accepted UAS dialog. The 200 OK is sent
// by Answer() once REST issues POST /v1/legs/{id}/answer.
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

// NewWhatsAppOutboundPendingLeg creates a ringing-state leg without a
// dialog. Caller drives the INVITE asynchronously and upgrades via
// ConnectOutbound on 200 OK.
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

func (l *WhatsAppLeg) Media() *PCMedia                          { return l.media }
func (l *WhatsAppLeg) AnswerCh() <-chan struct{}                { return l.answerCh }
func (l *WhatsAppLeg) ServerDialog() *sipgo.DialogServerSession { return l.serverDialog }
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

func (l *WhatsAppLeg) From() string { return l.from }
func (l *WhatsAppLeg) To() string   { return l.to }

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
