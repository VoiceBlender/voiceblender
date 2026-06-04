package leg

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"time"
)

type LegType string

const (
	TypeSIPInbound        LegType = "sip_inbound"
	TypeSIPOutbound       LegType = "sip_outbound"
	TypeWebRTC            LegType = "webrtc"
	TypeWhatsAppInbound   LegType = "whatsapp_in"
	TypeWhatsAppOutbound  LegType = "whatsapp_out"
	TypeWebSocketInbound  LegType = "websocket_in"
	TypeWebSocketOutbound LegType = "websocket_out"
	TypeMoQInbound        LegType = "moq_in"
	TypeLiveKitRoom       LegType = "livekit_room"
)

type LegState string

const (
	StateRinging    LegState = "ringing"
	StateEarlyMedia LegState = "early_media"
	StateConnected  LegState = "connected"
	StateHeld       LegState = "held"
	StateHungUp     LegState = "hung_up"
)

// RTPStats holds inbound RTP stream quality metrics.
type RTPStats struct {
	PacketsReceived uint32
	PacketsLost     uint32
	JitterMs        float64
	MOSScore        float64 // 1.0–5.0; 0 if insufficient data (< 2 packets)
}

// calculateMOS estimates the Mean Opinion Score (1.0–5.0) using a simplified
// E-model (ITU-T G.107) from packet loss rate (0–1) and jitter in milliseconds.
func calculateMOS(lossRate, jitterMs float64) float64 {
	effectiveLatency := jitterMs*2 + 10
	rFactor := 93.2 - effectiveLatency/40
	if effectiveLatency >= 160 {
		rFactor -= 10
	}
	rFactor -= lossRate * 100 * 2.5
	if rFactor < 0 {
		rFactor = 0
	} else if rFactor > 100 {
		rFactor = 100
	}
	mos := 1 + 0.035*rFactor + 7e-6*rFactor*(rFactor-60)*(100-rFactor)
	return math.Round(math.Max(1.0, math.Min(5.0, mos))*100) / 100
}

type Leg interface {
	ID() string
	Type() LegType
	State() LegState
	SampleRate() int
	AudioReader() io.Reader
	AudioWriter() io.Writer
	OnDTMF(func(digit rune))
	SendDTMF(ctx context.Context, digits string) error
	AcceptDTMF() bool
	SetAcceptDTMF(accept bool)
	OnTextReceived(func(text string, lossMarker bool))
	SendText(ctx context.Context, text string) error
	AcceptText() bool
	SetAcceptText(accept bool)
	RTTNegotiated() bool
	Hangup(ctx context.Context) error
	Answer(ctx context.Context) error
	Context() context.Context
	RoomID() string
	SetRoomID(id string)
	AppID() string
	SetAppID(id string)
	IsMuted() bool
	SetMuted(muted bool)
	IsDeaf() bool
	SetDeaf(deaf bool)
	// Role returns the routing role used by the room's audio routing matrix.
	// Empty string means unroled (full mesh).
	Role() string
	SetRole(role string)
	SetSpeakingTap(w io.Writer)
	ClearSpeakingTap()
	IsHeld() bool
	CreatedAt() time.Time
	AnsweredAt() time.Time
	SIPHeaders() map[string]string

	// Headers returns custom protocol headers exposed by the leg's
	// transport (X-/P- headers from a SIP INVITE or from a WebSocket HTTP
	// upgrade, caller-supplied headers on an outbound WebSocket dial,
	// etc.). Returns nil for leg types with no header concept.
	Headers() map[string]string
	RTPStats() RTPStats

	// ClaimDisconnect returns true exactly once per leg. Termination paths
	// (API hangup, remote BYE, RTP timeout, session expiry, etc.) call this
	// before publishing leg.disconnected so concurrent paths cannot emit
	// duplicate events.
	ClaimDisconnect() bool
}

// ErrRTTNotNegotiated is returned by SendText when RTT was not agreed in the
// SDP offer/answer exchange (or the leg type does not support RTT).
var ErrRTTNotNegotiated = errors.New("RTT not negotiated for this leg")

type Manager struct {
	mu   sync.RWMutex
	legs map[string]Leg
}

func NewManager() *Manager {
	return &Manager{
		legs: make(map[string]Leg),
	}
}

func (m *Manager) Add(l Leg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.legs[l.ID()] = l
}

func (m *Manager) Get(id string) (Leg, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.legs[id]
	return l, ok
}

// Remove deletes the leg from the manager and returns it together with a
// boolean indicating whether this call performed the removal. The boolean is
// the single-flight signal: termination paths use it to ensure only one of
// several racing callers (API DELETE × N, remote BYE, RTP timeout, session
// expiry) actually drives leg shutdown. Callers that get false should treat
// the leg as already being torn down by someone else.
func (m *Manager) Remove(id string) (Leg, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.legs[id]
	if !ok {
		return nil, false
	}
	delete(m.legs, id)
	return l, true
}

func (m *Manager) List() []Leg {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Leg, 0, len(m.legs))
	for _, l := range m.legs {
		out = append(out, l)
	}
	return out
}

func (m *Manager) All() map[string]Leg {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make(map[string]Leg, len(m.legs))
	for k, v := range m.legs {
		cp[k] = v
	}
	return cp
}

// FindSIPByCallID returns the SIPLeg whose dialog Call-ID matches the given
// value, or nil if none. Used to route in-dialog requests (e.g. REFER,
// NOTIFY) back to the leg that owns the dialog.
func (m *Manager) FindSIPByCallID(callID string) *SIPLeg {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, l := range m.legs {
		s, ok := l.(*SIPLeg)
		if !ok {
			continue
		}
		if s.CallID() == callID {
			return s
		}
	}
	return nil
}
