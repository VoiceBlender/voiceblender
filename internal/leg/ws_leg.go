package leg

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsmedia"
	"github.com/google/uuid"
)

// WebSocketLeg wraps a wsmedia.Transport as a Leg, supporting both inbound
// (server-side upgrade) and outbound (client-side dial) directions. Both
// share state machinery; only Type() differs.
type WebSocketLeg struct {
	id      string
	legType LegType
	state   LegState
	mu      sync.RWMutex

	transport *wsmedia.Transport
	headers   map[string]string

	egressPR *io.PipeReader
	egressPW *io.PipeWriter

	roomID     string
	appID      string
	muted      atomic.Bool
	deaf       atomic.Bool
	acceptDTMF atomic.Bool
	acceptText atomic.Bool
	rttEnabled bool
	sampleRate int

	onText atomic.Value // func(text string, lossMarker bool)

	createdAt  time.Time
	answeredAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	log    *slog.Logger

	disconnectDone atomic.Bool
}

// NewWebSocketInboundLeg constructs an inbound WS leg already bound to a
// transport. The leg goes straight to StateConnected (no ringing).
func NewWebSocketInboundLeg(t *wsmedia.Transport, headers map[string]string, sampleRate int, rtt bool, log *slog.Logger) *WebSocketLeg {
	l := newWebSocketLeg(TypeWebSocketInbound, sampleRate, rtt, log)
	l.AttachTransport(t, headers)
	l.mu.Lock()
	l.state = StateConnected
	l.answeredAt = time.Now()
	l.mu.Unlock()
	return l
}

// NewWebSocketOutboundPendingLeg constructs an outbound WS leg in
// StateRinging. Call AttachTransport once DialClient completes to
// transition to StateConnected.
func NewWebSocketOutboundPendingLeg(sampleRate int, rtt bool, log *slog.Logger) *WebSocketLeg {
	l := newWebSocketLeg(TypeWebSocketOutbound, sampleRate, rtt, log)
	l.mu.Lock()
	l.state = StateRinging
	l.mu.Unlock()
	return l
}

func newWebSocketLeg(t LegType, sampleRate int, rtt bool, log *slog.Logger) *WebSocketLeg {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	l := &WebSocketLeg{
		id:         uuid.New().String(),
		legType:    t,
		sampleRate: sampleRate,
		rttEnabled: rtt,
		egressPR:   pr,
		egressPW:   pw,
		createdAt:  time.Now(),
		ctx:        ctx,
		cancel:     cancel,
		log:        log,
	}
	l.acceptDTMF.Store(true)
	l.acceptText.Store(rtt)
	return l
}

// AttachTransport binds the wsmedia.Transport to the leg and starts the
// send loop reading from the egress pipe. headers may be nil for outbound
// dials where the server returned no relevant response headers.
func (l *WebSocketLeg) AttachTransport(t *wsmedia.Transport, headers map[string]string) {
	l.mu.Lock()
	l.transport = t
	if headers != nil {
		l.headers = headers
	}
	if l.state == StateRinging {
		l.state = StateConnected
		l.answeredAt = time.Now()
	}
	l.mu.Unlock()

	t.SetOnText(func(text string) {
		if !l.acceptText.Load() {
			return
		}
		fn, _ := l.onText.Load().(func(string, bool))
		if fn != nil {
			fn(text, false)
		}
	})
	t.Start(l.egressPR)
}

// Transport returns the underlying wsmedia.Transport.
func (l *WebSocketLeg) Transport() *wsmedia.Transport { return l.transport }

// ClaimDisconnect single-flights the disconnect publication.
func (l *WebSocketLeg) ClaimDisconnect() bool {
	return l.disconnectDone.CompareAndSwap(false, true)
}

func (l *WebSocketLeg) ID() string               { return l.id }
func (l *WebSocketLeg) Type() LegType            { return l.legType }
func (l *WebSocketLeg) Context() context.Context { return l.ctx }
func (l *WebSocketLeg) SampleRate() int          { return l.sampleRate }

func (l *WebSocketLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *WebSocketLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *WebSocketLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *WebSocketLeg) AppID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.appID
}

func (l *WebSocketLeg) SetAppID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appID = id
}

func (l *WebSocketLeg) IsMuted() bool        { return l.muted.Load() }
func (l *WebSocketLeg) SetMuted(m bool)      { l.muted.Store(m) }
func (l *WebSocketLeg) IsDeaf() bool         { return l.deaf.Load() }
func (l *WebSocketLeg) SetDeaf(d bool)       { l.deaf.Store(d) }
func (l *WebSocketLeg) AcceptDTMF() bool     { return l.acceptDTMF.Load() }
func (l *WebSocketLeg) SetAcceptDTMF(a bool) { l.acceptDTMF.Store(a) }
func (l *WebSocketLeg) IsHeld() bool         { return false }

func (l *WebSocketLeg) SetSpeakingTap(io.Writer) {}
func (l *WebSocketLeg) ClearSpeakingTap()        {}

func (l *WebSocketLeg) CreatedAt() time.Time { return l.createdAt }
func (l *WebSocketLeg) AnsweredAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.answeredAt
}

func (l *WebSocketLeg) SIPHeaders() map[string]string { return nil }
func (l *WebSocketLeg) Headers() map[string]string {
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

func (l *WebSocketLeg) RTPStats() RTPStats { return RTPStats{} }

func (l *WebSocketLeg) Answer(_ context.Context) error { return nil }

func (l *WebSocketLeg) Hangup(_ context.Context) error {
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

func (l *WebSocketLeg) OnDTMF(_ func(rune)) {}

func (l *WebSocketLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF over WebSocket not supported")
}

func (l *WebSocketLeg) OnTextReceived(fn func(text string, lossMarker bool)) {
	if fn == nil {
		l.onText.Store(func(string, bool) {})
		return
	}
	l.onText.Store(fn)
}

func (l *WebSocketLeg) SendText(ctx context.Context, text string) error {
	if !l.rttEnabled {
		return ErrRTTNotNegotiated
	}
	l.mu.RLock()
	t := l.transport
	l.mu.RUnlock()
	if t == nil {
		return fmt.Errorf("websocket leg: not yet connected")
	}
	return t.WriteText(ctx, text)
}

func (l *WebSocketLeg) AcceptText() bool     { return l.acceptText.Load() }
func (l *WebSocketLeg) SetAcceptText(a bool) { l.acceptText.Store(a) }
func (l *WebSocketLeg) RTTNegotiated() bool  { return l.rttEnabled }

// AudioReader returns inbound PCM as it arrives off the WebSocket.
func (l *WebSocketLeg) AudioReader() io.Reader {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.transport == nil {
		return emptyReader{}
	}
	return l.transport.AudioReader()
}

// AudioWriter returns the write side of the egress pipe; the room writes
// mixed-minus-self PCM here, the transport's send loop reads it.
func (l *WebSocketLeg) AudioWriter() io.Writer { return l.egressPW }

// emptyReader yields no bytes; used until AttachTransport runs.
type emptyReader struct{}

func (emptyReader) Read(p []byte) (int, error) { return 0, io.EOF }
