package room

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/google/uuid"
)

type Manager struct {
	mu     sync.RWMutex
	rooms  map[string]*Room
	legMgr *leg.Manager
	bus    *events.Bus
	log    *slog.Logger
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

	r := NewRoom(id, m.log)
	m.rooms[id] = r
	m.bus.Publish(events.RoomCreated, &events.RoomCreatedData{RoomScope: events.RoomScope{RoomID: id}})
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
	m.bus.Publish(events.RoomDeleted, &events.RoomDeletedData{RoomScope: events.RoomScope{RoomID: id}})
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
	m.bus.Publish(events.LegJoinedRoom, &events.LegJoinedRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: roomID},
	})
	return nil
}

func (m *Manager) MoveLeg(fromRoomID, toRoomID, legID string) error {
	fromRoom, ok := m.Get(fromRoomID)
	if !ok {
		return fmt.Errorf("room %s not found", fromRoomID)
	}

	// Get or create target room.
	m.mu.Lock()
	toRoom, ok := m.rooms[toRoomID]
	if !ok {
		toRoom = NewRoom(toRoomID, m.log)
		m.rooms[toRoomID] = toRoom
		m.bus.Publish(events.RoomCreated, &events.RoomCreatedData{RoomScope: events.RoomScope{RoomID: toRoomID}})
	}
	m.mu.Unlock()

	l, ok := fromRoom.DetachLeg(legID)
	if !ok {
		return fmt.Errorf("leg %s not found in room %s", legID, fromRoomID)
	}
	toRoom.AddLeg(l)

	m.bus.Publish(events.LegLeftRoom, &events.LegLeftRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: fromRoomID},
	})
	m.bus.Publish(events.LegJoinedRoom, &events.LegJoinedRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: toRoomID},
	})
	return nil
}

// FindLegRoom returns the room ID that contains the given leg, if any.
func (m *Manager) FindLegRoom(legID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.rooms {
		for _, p := range r.Participants() {
			if p.ID() == legID {
				return r.ID, true
			}
		}
	}
	return "", false
}

func (m *Manager) RemoveLeg(roomID, legID string) error {
	r, ok := m.Get(roomID)
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}

	r.RemoveLeg(legID)
	m.bus.Publish(events.LegLeftRoom, &events.LegLeftRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: roomID},
	})
	return nil
}
