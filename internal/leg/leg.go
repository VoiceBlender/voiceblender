package leg

import (
	"context"
	"io"
	"math"
	"sync"
	"time"
)

type LegType string

const (
	TypeSIPInbound  LegType = "sip_inbound"
	TypeSIPOutbound LegType = "sip_outbound"
	TypeWebRTC      LegType = "webrtc"
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
	Hangup(ctx context.Context) error
	Answer(ctx context.Context) error
	Context() context.Context
	RoomID() string
	SetRoomID(id string)
	IsMuted() bool
	SetMuted(muted bool)
	IsDeaf() bool
	SetDeaf(deaf bool)
	SetSpeakingTap(w io.Writer)
	ClearSpeakingTap()
	IsHeld() bool
	CreatedAt() time.Time
	AnsweredAt() time.Time
	SIPHeaders() map[string]string
	RTPStats() RTPStats
}

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

func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.legs, id)
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
