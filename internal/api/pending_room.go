package api

import (
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// pendingRoomJoins maps an outbound leg to the room it will join once media is
// ready. The zero value is ready to use.
type pendingRoomJoins struct {
	mu   sync.Mutex
	legs map[string]string // leg ID -> room ID
}

func (p *pendingRoomJoins) set(legID, roomID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.legs == nil {
		p.legs = make(map[string]string)
	}
	p.legs[legID] = roomID
}

func (p *pendingRoomJoins) clear(legID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.legs, legID)
}

func (p *pendingRoomJoins) legsFor(roomID string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for legID, rid := range p.legs {
		if rid == roomID {
			ids = append(ids, legID)
		}
	}
	return ids
}

// pendingRoomLegs returns the live legs still waiting to join roomID.
func (s *Server) pendingRoomLegs(roomID string) []leg.Leg {
	var legs []leg.Leg
	for _, id := range s.pendingRooms.legsFor(roomID) {
		if l, ok := s.LegMgr.Get(id); ok {
			legs = append(legs, l)
		}
	}
	return legs
}

// joinPendingRoom adds an outbound leg to the room it was created into. It
// returns false when that room no longer exists, in which case the leg has
// been hung up rather than left connected to nothing.
func (s *Server) joinPendingRoom(l leg.Leg, roomID string) bool {
	err := s.RoomMgr.AddLeg(roomID, l.ID())
	// Cleared only after the add, so a concurrent room delete always finds
	// the leg either pending or as a participant.
	s.pendingRooms.clear(l.ID())
	if err == nil {
		s.onLegJoinedRoom(roomID, l.ID())
		return true
	}
	s.Log.Warn("auto-add leg to room failed", "leg_id", l.ID(), "room_id", roomID, "error", err)
	if _, ok := s.RoomMgr.Get(roomID); ok {
		return true
	}
	s.cleanupLeg(l)
	s.publishDisconnect(l, "room_deleted")
	return false
}

const defaultRingTimeout = 60 * time.Second

// ringTimeout resolves how long an outbound leg may stay unanswered: the
// request's ring_timeout when given (0 = unbounded), else the default.
func (s *Server) ringTimeout(req CreateLegRequest) time.Duration {
	if req.RingTimeout == nil {
		return s.DefaultRingTimeout
	}
	if *req.RingTimeout <= 0 {
		return 0
	}
	return time.Duration(*req.RingTimeout) * time.Second
}
