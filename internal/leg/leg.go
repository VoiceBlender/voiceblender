package leg

import (
	"context"
	"io"
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
	IsHeld() bool
	CreatedAt() time.Time
	AnsweredAt() time.Time
	SIPHeaders() map[string]string
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
