package leg

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// LiveKitParticipantLeg represents a single remote LiveKit participant —
// auto-created when the umbrella LiveKitPublishLeg's transport receives an
// inbound audio track for that participant. From VoiceBlender's room
// perspective the participant is a normal leg whose AudioReader is the
// decoded PCM of the LK participant's published audio. The AudioWriter is
// io.Discard — the LK side already mixes outbound audio for the
// participant, so we have nothing meaningful to send back to them at the
// leg level.
//
// State machine: created in StateConnected. No ringing, no early-media.
// DTMF and RTT are not negotiated over LiveKit, so SendDTMF/SendText
// return errors. Hangup tears down the leg locally; the API layer is
// responsible for asking the LK server to unsubscribe via the umbrella
// transport (so we stop receiving the participant's RTP).
type LiveKitParticipantLeg struct {
	id      string
	legType LegType
	state   LegState
	mu      sync.RWMutex

	identity string
	trackSID string
	headers  map[string]string

	pcmReader  io.Reader
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

// Ensure *LiveKitParticipantLeg satisfies the Leg interface at compile time.
var _ Leg = (*LiveKitParticipantLeg)(nil)

// NewLiveKitParticipantLeg builds a participant leg already bound to its
// decoded-PCM source. The reader is owned by the caller (typically the
// umbrella transport's per-track decoder goroutine); the leg only reads
// from it.
//
// `identity` is the LK participant identity from JoinResponse /
// ParticipantUpdate. `trackSID` is the LK track ID of the audio track.
// Both are surfaced via Headers().
//
// The leg starts in StateConnected; the API layer is responsible for
// adding it to the LegMgr, emitting `leg.connected`, and adding it to the
// VoiceBlender room.
func NewLiveKitParticipantLeg(identity, trackSID string, pcmReader io.Reader, sampleRate int, log *slog.Logger) *LiveKitParticipantLeg {
	ctx, cancel := context.WithCancel(context.Background())
	if log == nil {
		log = slog.Default()
	}
	headers := map[string]string{}
	if identity != "" {
		headers["livekit_identity"] = identity
	}
	if trackSID != "" {
		headers["livekit_track_sid"] = trackSID
	}
	if len(headers) == 0 {
		headers = nil
	}
	now := time.Now()
	return &LiveKitParticipantLeg{
		id:         uuid.New().String(),
		legType:    TypeLiveKitParticipant,
		state:      StateConnected,
		identity:   identity,
		trackSID:   trackSID,
		headers:    headers,
		pcmReader:  pcmReader,
		sampleRate: sampleRate,
		createdAt:  now,
		answeredAt: now,
		ctx:        ctx,
		cancel:     cancel,
		log:        log,
	}
}

// Identity returns the LK participant identity from JoinResponse.
func (l *LiveKitParticipantLeg) Identity() string { return l.identity }

// TrackSID returns the LK audio track ID this leg is bound to.
func (l *LiveKitParticipantLeg) TrackSID() string { return l.trackSID }

func (l *LiveKitParticipantLeg) ClaimDisconnect() bool {
	return l.disconnectDone.CompareAndSwap(false, true)
}

func (l *LiveKitParticipantLeg) ID() string               { return l.id }
func (l *LiveKitParticipantLeg) Type() LegType            { return l.legType }
func (l *LiveKitParticipantLeg) Context() context.Context { return l.ctx }
func (l *LiveKitParticipantLeg) SampleRate() int          { return l.sampleRate }

func (l *LiveKitParticipantLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *LiveKitParticipantLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *LiveKitParticipantLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *LiveKitParticipantLeg) AppID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.appID
}

func (l *LiveKitParticipantLeg) SetAppID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appID = id
}

func (l *LiveKitParticipantLeg) Role() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.role
}

func (l *LiveKitParticipantLeg) SetRole(r string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.role = r
}

func (l *LiveKitParticipantLeg) IsMuted() bool        { return l.muted.Load() }
func (l *LiveKitParticipantLeg) SetMuted(m bool)      { l.muted.Store(m) }
func (l *LiveKitParticipantLeg) IsDeaf() bool         { return l.deaf.Load() }
func (l *LiveKitParticipantLeg) SetDeaf(d bool)       { l.deaf.Store(d) }
func (l *LiveKitParticipantLeg) AcceptDTMF() bool     { return l.acceptDTMF.Load() }
func (l *LiveKitParticipantLeg) SetAcceptDTMF(a bool) { l.acceptDTMF.Store(a) }
func (l *LiveKitParticipantLeg) IsHeld() bool         { return false }

func (l *LiveKitParticipantLeg) SetSpeakingTap(io.Writer) {}
func (l *LiveKitParticipantLeg) ClearSpeakingTap()        {}

func (l *LiveKitParticipantLeg) CreatedAt() time.Time { return l.createdAt }
func (l *LiveKitParticipantLeg) AnsweredAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.answeredAt
}

func (l *LiveKitParticipantLeg) SIPHeaders() map[string]string { return nil }

func (l *LiveKitParticipantLeg) Headers() map[string]string {
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

func (l *LiveKitParticipantLeg) RTPStats() RTPStats { return RTPStats{} }

// Answer is a no-op for LK participant legs — connection completes when
// the leg is constructed.
func (l *LiveKitParticipantLeg) Answer(_ context.Context) error { return nil }

// Hangup transitions to StateHungUp and cancels the leg's context. The
// API layer is responsible for telling the umbrella transport to drop
// the LK subscription (so the server stops sending us this participant's
// RTP). Idempotent.
func (l *LiveKitParticipantLeg) Hangup(_ context.Context) error {
	l.mu.Lock()
	if l.state == StateHungUp {
		l.mu.Unlock()
		return nil
	}
	l.state = StateHungUp
	l.mu.Unlock()
	l.cancel()
	return nil
}

func (l *LiveKitParticipantLeg) OnDTMF(_ func(rune)) {}

func (l *LiveKitParticipantLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF over LiveKit not supported")
}

func (l *LiveKitParticipantLeg) OnTextReceived(_ func(text string, lossMarker bool)) {}

func (l *LiveKitParticipantLeg) SendText(_ context.Context, _ string) error {
	return ErrRTTNotNegotiated
}

func (l *LiveKitParticipantLeg) AcceptText() bool     { return l.acceptText.Load() }
func (l *LiveKitParticipantLeg) SetAcceptText(a bool) { l.acceptText.Store(a) }
func (l *LiveKitParticipantLeg) RTTNegotiated() bool  { return false }

// AudioReader yields the decoded PCM of the LK participant's audio
// track. Driven by the umbrella transport's per-track decoder goroutine.
func (l *LiveKitParticipantLeg) AudioReader() io.Reader {
	if l.pcmReader == nil {
		return emptyReader{}
	}
	return l.pcmReader
}

// AudioWriter is io.Discard: the LK side already mixes outbound audio
// for this participant, so anything VB tried to write would not reach
// them anyway. Reads from the VB room mixer's mixed-minus-self output
// land here and are dropped.
func (l *LiveKitParticipantLeg) AudioWriter() io.Writer { return io.Discard }
