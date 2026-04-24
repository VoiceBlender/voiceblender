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
	"github.com/pion/webrtc/v4"
)

// WebRTCLeg wraps a pion PeerConnection (via PCMedia) as a Leg.
type WebRTCLeg struct {
	id    string
	state LegState
	mu    sync.RWMutex

	media *PCMedia

	roomID     string
	appID      string
	muted      atomic.Bool
	deaf       atomic.Bool
	acceptDTMF atomic.Bool
	createdAt  time.Time

	onDTMF func(digit rune)
	log    *slog.Logger
}

// NewWebRTCLeg wraps a PCMedia and starts its outbound write loop.
func NewWebRTCLeg(media *PCMedia, log *slog.Logger) *WebRTCLeg {
	l := &WebRTCLeg{
		id:        uuid.New().String(),
		state:     StateConnected,
		createdAt: time.Now(),
		media:     media,
		log:       log,
	}
	l.acceptDTMF.Store(true)
	media.Start()
	return l
}

// Media returns the underlying PCMedia (for SDP negotiation + ICE trickle).
func (l *WebRTCLeg) Media() *PCMedia { return l.media }

func (l *WebRTCLeg) ID() string      { return l.id }
func (l *WebRTCLeg) Type() LegType   { return TypeWebRTC }
func (l *WebRTCLeg) SampleRate() int { return l.media.SampleRate() }

func (l *WebRTCLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *WebRTCLeg) Context() context.Context { return l.media.Context() }

func (l *WebRTCLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *WebRTCLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *WebRTCLeg) AppID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.appID
}

func (l *WebRTCLeg) SetAppID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appID = id
}

func (l *WebRTCLeg) IsMuted() bool              { return l.muted.Load() }
func (l *WebRTCLeg) SetMuted(m bool)            { l.muted.Store(m) }
func (l *WebRTCLeg) IsDeaf() bool               { return l.deaf.Load() }
func (l *WebRTCLeg) SetDeaf(d bool)             { l.deaf.Store(d) }
func (l *WebRTCLeg) AcceptDTMF() bool           { return l.acceptDTMF.Load() }
func (l *WebRTCLeg) SetAcceptDTMF(a bool)       { l.acceptDTMF.Store(a) }
func (l *WebRTCLeg) SetSpeakingTap(w io.Writer) { l.media.SetSpeakingTap(w) }
func (l *WebRTCLeg) ClearSpeakingTap()          { l.media.ClearSpeakingTap() }
func (l *WebRTCLeg) IsHeld() bool               { return false }

func (l *WebRTCLeg) CreatedAt() time.Time          { return l.createdAt }
func (l *WebRTCLeg) AnsweredAt() time.Time         { return l.createdAt }
func (l *WebRTCLeg) SIPHeaders() map[string]string { return nil }
func (l *WebRTCLeg) RTPStats() RTPStats            { return RTPStats{} }

func (l *WebRTCLeg) Answer(_ context.Context) error {
	return fmt.Errorf("webrtc legs do not need explicit answer")
}

func (l *WebRTCLeg) Hangup(_ context.Context) error {
	l.mu.Lock()
	l.state = StateHungUp
	l.mu.Unlock()
	return l.media.Close()
}

func (l *WebRTCLeg) OnDTMF(f func(digit rune)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onDTMF = f
}

func (l *WebRTCLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF send over WebRTC not yet implemented")
}

// AddICECandidate adds a remote ICE candidate.
func (l *WebRTCLeg) AddICECandidate(c webrtc.ICECandidateInit) error {
	return l.media.AddICECandidate(c)
}

// DrainCandidates returns buffered local ICE candidates and whether gathering is done.
func (l *WebRTCLeg) DrainCandidates() ([]webrtc.ICECandidateInit, bool) {
	return l.media.DrainLocalCandidates()
}

func (l *WebRTCLeg) AudioReader() io.Reader { return l.media.AudioReader() }
func (l *WebRTCLeg) AudioWriter() io.Writer { return l.media.AudioWriter() }
