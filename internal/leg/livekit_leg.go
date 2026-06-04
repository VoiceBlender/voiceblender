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

// LiveKitPublishLeg represents the outbound (publish) audio direction of
// a LiveKit-room connection. One per umbrella `POST /v1/legs
// type=livekit_room` call; it owns the lkmedia.Transport (signaling +
// publisher PC + subscriber PC).
//
// Audio model: the publish leg's AudioWriter is fed by the VB room mixer
// with mixed-minus-self PCM; that PCM is Opus-encoded and pushed onto the
// local LiveKit track. The leg has no upstream audio of its own —
// AudioReader returns emptyReader{}. Remote LK participants surface as
// separate LiveKitParticipantLeg entries in the same VB room (auto-managed
// by the API layer).
//
// State machine: created in StateConnected (LK signaling already
// completed during the transport's Connect). No ringing, no early-media.
// DTMF and RTT are not negotiated over LiveKit (browser SDKs don't carry
// them), so SendDTMF/SendText return errors.
type LiveKitPublishLeg struct {
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

// Ensure *LiveKitPublishLeg satisfies the Leg interface at compile time.
var _ Leg = (*LiveKitPublishLeg)(nil)

// NewLiveKitPublishLeg constructs the umbrella publish leg already bound
// to a connected transport. The leg is StateConnected immediately — the
// LK signaling JOIN completed during Connect, so there is no ringing
// phase to wait on.
//
// The headers map is expected to carry observability fields surfaced via
// Leg.Headers (livekit_identity, livekit_room). The leg merges these
// with the transport's identity/room values so callers can pass nil or
// partial maps.
func NewLiveKitPublishLeg(t *lkmedia.Transport, headers map[string]string, sampleRate int, log *slog.Logger) *LiveKitPublishLeg {
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
	return &LiveKitPublishLeg{
		id:         uuid.New().String(),
		legType:    TypeLiveKitPublish,
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
func (l *LiveKitPublishLeg) Transport() *lkmedia.Transport { return l.transport }

func (l *LiveKitPublishLeg) ClaimDisconnect() bool {
	return l.disconnectDone.CompareAndSwap(false, true)
}

func (l *LiveKitPublishLeg) ID() string               { return l.id }
func (l *LiveKitPublishLeg) Type() LegType            { return l.legType }
func (l *LiveKitPublishLeg) Context() context.Context { return l.ctx }
func (l *LiveKitPublishLeg) SampleRate() int          { return l.sampleRate }

func (l *LiveKitPublishLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *LiveKitPublishLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *LiveKitPublishLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *LiveKitPublishLeg) AppID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.appID
}

func (l *LiveKitPublishLeg) SetAppID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appID = id
}

func (l *LiveKitPublishLeg) Role() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.role
}

func (l *LiveKitPublishLeg) SetRole(r string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.role = r
}

func (l *LiveKitPublishLeg) IsMuted() bool        { return l.muted.Load() }
func (l *LiveKitPublishLeg) SetMuted(m bool)      { l.muted.Store(m) }
func (l *LiveKitPublishLeg) IsDeaf() bool         { return l.deaf.Load() }
func (l *LiveKitPublishLeg) SetDeaf(d bool)       { l.deaf.Store(d) }
func (l *LiveKitPublishLeg) AcceptDTMF() bool     { return l.acceptDTMF.Load() }
func (l *LiveKitPublishLeg) SetAcceptDTMF(a bool) { l.acceptDTMF.Store(a) }
func (l *LiveKitPublishLeg) IsHeld() bool         { return false }

func (l *LiveKitPublishLeg) SetSpeakingTap(io.Writer) {}
func (l *LiveKitPublishLeg) ClearSpeakingTap()        {}

func (l *LiveKitPublishLeg) CreatedAt() time.Time { return l.createdAt }
func (l *LiveKitPublishLeg) AnsweredAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.answeredAt
}

func (l *LiveKitPublishLeg) SIPHeaders() map[string]string { return nil }

func (l *LiveKitPublishLeg) Headers() map[string]string {
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

func (l *LiveKitPublishLeg) RTPStats() RTPStats { return RTPStats{} }

// Answer is a no-op for LK legs — connection completes during NewTransport.
func (l *LiveKitPublishLeg) Answer(_ context.Context) error { return nil }

// Hangup tears down the transport (sends Leave + closes peer connections).
// Idempotent.
func (l *LiveKitPublishLeg) Hangup(_ context.Context) error {
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

func (l *LiveKitPublishLeg) OnDTMF(_ func(rune)) {}

func (l *LiveKitPublishLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF over LiveKit not supported")
}

func (l *LiveKitPublishLeg) OnTextReceived(_ func(text string, lossMarker bool)) {}

func (l *LiveKitPublishLeg) SendText(_ context.Context, _ string) error {
	return ErrRTTNotNegotiated
}

func (l *LiveKitPublishLeg) AcceptText() bool     { return l.acceptText.Load() }
func (l *LiveKitPublishLeg) SetAcceptText(a bool) { l.acceptText.Store(a) }
func (l *LiveKitPublishLeg) RTTNegotiated() bool  { return false }

func (l *LiveKitPublishLeg) AudioReader() io.Reader {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.transport == nil {
		return emptyReader{}
	}
	return l.transport.AudioReader()
}

func (l *LiveKitPublishLeg) AudioWriter() io.Writer {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.transport == nil {
		return io.Discard
	}
	return l.transport.AudioWriter()
}
