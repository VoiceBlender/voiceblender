package leg

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/lkmedia"
	"github.com/google/uuid"
)

// LiveKitLeg wraps a lkmedia.Transport as a Leg. Model C semantics: the
// entire LiveKit room is represented as one VoiceBlender participant.
// All N LK remote tracks are sum-mixed inside the transport before being
// exposed via AudioReader.
//
// State machine: created in StateConnected (LK signaling already
// completed during the transport's Connect). No ringing, no early-media.
// DTMF and RTT are not negotiated over LiveKit (browser SDKs don't carry
// them), so SendDTMF/SendText return errors.
type LiveKitLeg struct {
	id      string
	legType LegType
	state   LegState
	mu      sync.RWMutex

	transport *lkmedia.Transport
	headers   map[string]string

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

// Ensure *LiveKitLeg satisfies the Leg interface at compile time.
var _ Leg = (*LiveKitLeg)(nil)

// NewLiveKitLeg constructs a LiveKit-room leg already bound to a connected
// transport. The leg is StateConnected immediately — the LK signaling
// JOIN completed during Connect, so there is no ringing phase to wait on.
//
// The headers map is expected to carry observability fields surfaced via
// Leg.Headers (livekit_identity, livekit_name, livekit_room). The leg
// merges these with the transport's identity/room values so callers can
// pass nil or partial maps.
func NewLiveKitLeg(t *lkmedia.Transport, headers map[string]string, sampleRate int, log *slog.Logger) *LiveKitLeg {
	ctx, cancel := context.WithCancel(context.Background())
	if log == nil {
		log = slog.Default()
	}

	merged := make(map[string]string, len(headers)+3)
	for k, v := range headers {
		merged[k] = v
	}
	if t != nil {
		if v := t.LocalIdentity(); v != "" {
			merged["livekit_identity"] = v
		}
		if v := t.RoomName(); v != "" {
			merged["livekit_room"] = v
		}
	}
	if len(merged) == 0 {
		merged = nil
	}

	now := time.Now()
	return &LiveKitLeg{
		id:         uuid.New().String(),
		legType:    TypeLiveKitRoom,
		sampleRate: sampleRate,
		transport:  t,
		headers:    merged,
		state:      StateConnected,
		createdAt:  now,
		answeredAt: now,
		ctx:        ctx,
		cancel:     cancel,
		log:        log,
	}
}

// Transport returns the underlying lkmedia.Transport. Used by the API
// handler to wait on Done() and surface CloseReason on disconnect.
func (l *LiveKitLeg) Transport() *lkmedia.Transport { return l.transport }

func (l *LiveKitLeg) ClaimDisconnect() bool {
	return l.disconnectDone.CompareAndSwap(false, true)
}

func (l *LiveKitLeg) ID() string               { return l.id }
func (l *LiveKitLeg) Type() LegType            { return l.legType }
func (l *LiveKitLeg) Context() context.Context { return l.ctx }
func (l *LiveKitLeg) SampleRate() int          { return l.sampleRate }

func (l *LiveKitLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *LiveKitLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *LiveKitLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *LiveKitLeg) AppID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.appID
}

func (l *LiveKitLeg) SetAppID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appID = id
}

func (l *LiveKitLeg) Role() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.role
}

func (l *LiveKitLeg) SetRole(r string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.role = r
}

func (l *LiveKitLeg) IsMuted() bool        { return l.muted.Load() }
func (l *LiveKitLeg) SetMuted(m bool)      { l.muted.Store(m) }
func (l *LiveKitLeg) IsDeaf() bool         { return l.deaf.Load() }
func (l *LiveKitLeg) SetDeaf(d bool)       { l.deaf.Store(d) }
func (l *LiveKitLeg) AcceptDTMF() bool     { return l.acceptDTMF.Load() }
func (l *LiveKitLeg) SetAcceptDTMF(a bool) { l.acceptDTMF.Store(a) }
func (l *LiveKitLeg) IsHeld() bool         { return false }

func (l *LiveKitLeg) SetSpeakingTap(io.Writer) {}
func (l *LiveKitLeg) ClearSpeakingTap()        {}

func (l *LiveKitLeg) CreatedAt() time.Time { return l.createdAt }
func (l *LiveKitLeg) AnsweredAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.answeredAt
}

func (l *LiveKitLeg) SIPHeaders() map[string]string { return nil }

func (l *LiveKitLeg) Headers() map[string]string {
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

func (l *LiveKitLeg) RTPStats() RTPStats { return RTPStats{} }

// Answer is a no-op for LK legs — connection completes during NewTransport.
func (l *LiveKitLeg) Answer(_ context.Context) error { return nil }

// Hangup tears down the transport (sends Leave + closes peer connections).
// Idempotent.
func (l *LiveKitLeg) Hangup(_ context.Context) error {
	l.mu.Lock()
	if l.state == StateHungUp {
		l.mu.Unlock()
		return nil
	}
	l.state = StateHungUp
	t := l.transport
	l.mu.Unlock()

	if t != nil {
		_ = t.CloseClient()
	}
	l.cancel()
	return nil
}

func (l *LiveKitLeg) OnDTMF(_ func(rune)) {}

func (l *LiveKitLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF over LiveKit not supported")
}

func (l *LiveKitLeg) OnTextReceived(_ func(text string, lossMarker bool)) {}

func (l *LiveKitLeg) SendText(_ context.Context, _ string) error {
	return ErrRTTNotNegotiated
}

func (l *LiveKitLeg) AcceptText() bool     { return l.acceptText.Load() }
func (l *LiveKitLeg) SetAcceptText(a bool) { l.acceptText.Store(a) }
func (l *LiveKitLeg) RTTNegotiated() bool  { return false }

func (l *LiveKitLeg) AudioReader() io.Reader {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.transport == nil {
		return emptyReader{}
	}
	return l.transport.AudioReader()
}

func (l *LiveKitLeg) AudioWriter() io.Writer {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.transport == nil {
		return io.Discard
	}
	return l.transport.AudioWriter()
}
