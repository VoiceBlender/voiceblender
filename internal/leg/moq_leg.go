package leg

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/moqmedia"
	"github.com/google/uuid"
)

// MoQLeg wraps a moqmedia.Transport as a Leg. PoC scope: inbound only,
// straight to StateConnected, no DTMF, no RTT/text, no mute parity beyond
// the atomic bit. Mute/deaf are honored by the room's mixer adapter
// because the leg exposes IsMuted/IsDeaf.
type MoQLeg struct {
	id      string
	legType LegType
	state   LegState
	mu      sync.RWMutex

	transport *moqmedia.Transport
	headers   map[string]string

	egressPR *io.PipeReader
	egressPW *io.PipeWriter

	roomID     string
	appID      string
	role       string
	muted      atomic.Bool
	deaf       atomic.Bool
	acceptDTMF atomic.Bool
	acceptText atomic.Bool
	sampleRate int

	createdAt  time.Time
	answeredAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	log    *slog.Logger

	disconnectDone atomic.Bool
}

// NewMoQInboundLeg constructs an inbound MoQ leg already bound to a
// transport and starts the transport's send loop reading from the egress
// pipe. The leg goes straight to StateConnected (no ringing flow).
func NewMoQInboundLeg(t *moqmedia.Transport, headers map[string]string, sampleRate int, log *slog.Logger) *MoQLeg {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	l := &MoQLeg{
		id:         uuid.New().String(),
		legType:    TypeMoQInbound,
		sampleRate: sampleRate,
		transport:  t,
		headers:    headers,
		egressPR:   pr,
		egressPW:   pw,
		state:      StateConnected,
		createdAt:  time.Now(),
		answeredAt: time.Now(),
		ctx:        ctx,
		cancel:     cancel,
		log:        log,
	}
	t.Start(l.egressPR)
	return l
}

// Transport returns the underlying moqmedia.Transport.
func (l *MoQLeg) Transport() *moqmedia.Transport { return l.transport }

func (l *MoQLeg) ClaimDisconnect() bool {
	return l.disconnectDone.CompareAndSwap(false, true)
}

func (l *MoQLeg) ID() string               { return l.id }
func (l *MoQLeg) Type() LegType            { return l.legType }
func (l *MoQLeg) Context() context.Context { return l.ctx }
func (l *MoQLeg) SampleRate() int          { return l.sampleRate }

func (l *MoQLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *MoQLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *MoQLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *MoQLeg) AppID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.appID
}

func (l *MoQLeg) SetAppID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appID = id
}

func (l *MoQLeg) Role() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.role
}

func (l *MoQLeg) SetRole(r string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.role = r
}

func (l *MoQLeg) IsMuted() bool        { return l.muted.Load() }
func (l *MoQLeg) SetMuted(m bool)      { l.muted.Store(m) }
func (l *MoQLeg) IsDeaf() bool         { return l.deaf.Load() }
func (l *MoQLeg) SetDeaf(d bool)       { l.deaf.Store(d) }
func (l *MoQLeg) AcceptDTMF() bool     { return l.acceptDTMF.Load() }
func (l *MoQLeg) SetAcceptDTMF(a bool) { l.acceptDTMF.Store(a) }
func (l *MoQLeg) IsHeld() bool         { return false }

func (l *MoQLeg) SetSpeakingTap(io.Writer) {}
func (l *MoQLeg) ClearSpeakingTap()        {}

func (l *MoQLeg) CreatedAt() time.Time { return l.createdAt }
func (l *MoQLeg) AnsweredAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.answeredAt
}

func (l *MoQLeg) SIPHeaders() map[string]string { return nil }
func (l *MoQLeg) Headers() map[string]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(l.headers))
	for k, v := range l.headers {
		out[k] = v
	}
	return out
}

func (l *MoQLeg) RTPStats() RTPStats { return RTPStats{} }

func (l *MoQLeg) Answer(_ context.Context) error { return nil }

func (l *MoQLeg) Hangup(_ context.Context) error {
	l.mu.Lock()
	if l.state == StateHungUp {
		l.mu.Unlock()
		return nil
	}
	l.state = StateHungUp
	t := l.transport
	l.mu.Unlock()

	_ = l.egressPW.Close()
	if t != nil {
		_ = t.Close()
	}
	l.cancel()
	return nil
}

func (l *MoQLeg) OnDTMF(_ func(rune)) {}

func (l *MoQLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF over MoQ not supported")
}

func (l *MoQLeg) OnTextReceived(_ func(text string, lossMarker bool)) {}

func (l *MoQLeg) SendText(_ context.Context, _ string) error {
	return ErrRTTNotNegotiated
}

func (l *MoQLeg) AcceptText() bool     { return l.acceptText.Load() }
func (l *MoQLeg) SetAcceptText(a bool) { l.acceptText.Store(a) }
func (l *MoQLeg) RTTNegotiated() bool  { return false }

func (l *MoQLeg) AudioReader() io.Reader {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.transport == nil {
		return emptyReader{}
	}
	return l.transport.AudioReader()
}

func (l *MoQLeg) AudioWriter() io.Writer { return l.egressPW }
