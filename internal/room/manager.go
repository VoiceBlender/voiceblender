package room

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/leg"
	"github.com/google/uuid"
)

type Manager struct {
	mu       sync.RWMutex
	rooms    map[string]*Room
	legMgr   *leg.Manager
	bus      *events.Bus
	log      *slog.Logger
}

func NewManager(legMgr *leg.Manager, bus *events.Bus, log *slog.Logger) *Manager {
	return &Manager{
		rooms:  make(map[string]*Room),
		legMgr: legMgr,
		bus:    bus,
		log:    log,
	}
}

func (m *Manager) Create(id string) (*Room, error) {
	if id == "" {
		id = uuid.New().String()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.rooms[id]; exists {
		return nil, fmt.Errorf("room %s already exists", id)
	}

	r := NewRoom(id, m.bus, m.log)
	m.rooms[id] = r
	m.bus.Publish(events.RoomCreated, map[string]interface{}{"room_id": id})
	return r, nil
}

func (m *Manager) Get(id string) (*Room, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.rooms[id]
	return r, ok
}

func (m *Manager) List() []*Room {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		out = append(out, r)
	}
	return out
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	r, ok := m.rooms[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("room %s not found", id)
	}
	delete(m.rooms, id)
	m.mu.Unlock()

	// Hangup all participants concurrently.
	// Bye() blocks waiting for a SIP response, so sequential hangups
	// would stall on the first leg and never reach the rest.
	var wg sync.WaitGroup
	for _, l := range r.Participants() {
		wg.Add(1)
		go func(l leg.Leg) {
			defer wg.Done()
			l.Hangup(context.Background())
		}(l)
	}
	wg.Wait()
	r.Close()
	m.bus.Publish(events.RoomDeleted, map[string]interface{}{"room_id": id})
	return nil
}

func (m *Manager) AddLeg(roomID, legID string) error {
	r, ok := m.Get(roomID)
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}

	l, ok := m.legMgr.Get(legID)
	if !ok {
		return fmt.Errorf("leg %s not found", legID)
	}

	if l.State() != leg.StateConnected && l.State() != leg.StateEarlyMedia {
		return fmt.Errorf("leg %s is not connected (state: %s)", legID, l.State())
	}

	r.AddLeg(l)
	m.bus.Publish(events.LegJoinedRoom, map[string]interface{}{
		"leg_id":  legID,
		"room_id": roomID,
	})
	return nil
}

func (m *Manager) RemoveLeg(roomID, legID string) error {
	r, ok := m.Get(roomID)
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}

	r.RemoveLeg(legID)
	m.bus.Publish(events.LegLeftRoom, map[string]interface{}{
		"leg_id":  legID,
		"room_id": roomID,
	})
	return nil
}
