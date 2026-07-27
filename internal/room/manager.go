package room

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/bridge"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/google/uuid"
)

type Manager struct {
	mu      sync.RWMutex
	rooms   map[string]*Room
	bridges map[string]*Bridge
	legMgr  *leg.Manager
	bus     *events.Bus
	log     *slog.Logger

	// onLegPanicTeardown, when set, is notified after a leg was torn down
	// following a mixer IO panic. hookMu guards it alone — never take it and
	// m.mu together, and never call the hook under either.
	hookMu             sync.Mutex
	onLegPanicTeardown func(l leg.Leg, roomID, reason string)
}

// legPanicReason is the disconnect reason reported for a leg torn down after
// its mixer IO loop panicked. It reaches the wire as cdr.reason on
// leg.disconnected, so it is a public value (documented in API.md).
const legPanicReason = "mixer_panic"

// SetOnLegPanicTeardown registers a callback invoked after a leg has been
// removed from its room and hung up because its mixer IO loop panicked.
// Passing nil disables it.
//
// This is NOT a general leg-termination notification: it fires on exactly one
// path, tearDownPanickedLeg, and never for Delete, an API hangup or shutdown —
// those have an API-layer caller already positioned to finish the job. The
// mixer-panic teardown is the only leg-terminal path the room layer triggers
// asynchronously by itself, which is why it needs the owner told. A future
// room-side terminal path is NOT covered by this hook.
//
// The manager holds no lock while invoking fn. fn runs on the teardown
// goroutine inside its recover(), so a panicking callback is contained.
//
// fn receives the room the leg was removed from. The removal has already
// cleared the leg's own RoomID, so the hook is the only place that still knows
// it — without it the owner cannot run the room-scoped half of the teardown.
func (m *Manager) SetOnLegPanicTeardown(fn func(l leg.Leg, roomID, reason string)) {
	m.hookMu.Lock()
	defer m.hookMu.Unlock()
	m.onLegPanicTeardown = fn
}

// legPanicTeardownHook snapshots the hook under hookMu and returns it, so the
// caller can invoke it with no lock held.
func (m *Manager) legPanicTeardownHook() func(l leg.Leg, roomID, reason string) {
	m.hookMu.Lock()
	defer m.hookMu.Unlock()
	return m.onLegPanicTeardown
}

func NewManager(legMgr *leg.Manager, bus *events.Bus, log *slog.Logger) *Manager {
	return &Manager{
		rooms:   make(map[string]*Room),
		bridges: make(map[string]*Bridge),
		legMgr:  legMgr,
		bus:     bus,
		log:     log,
	}
}

func (m *Manager) Create(id, appID string, sampleRate int) (*Room, error) {
	if id == "" {
		id = uuid.New().String()
	}

	// Built before the lock: NewRoom only allocates and the mixer starts no
	// goroutines until a participant joins, so a candidate that loses the
	// exists-check costs an allocation and nothing else. Wiring the panic hook
	// out here also keeps Mixer.hookMu out of m.mu's lock order, and means the
	// room is never reachable without its hook.
	r := NewRoom(id, appID, sampleRate, m.log)
	m.wireMixerPanicHook(r)

	m.mu.Lock()
	if _, exists := m.rooms[id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("room %s already exists", id)
	}
	m.rooms[id] = r
	m.mu.Unlock()

	// Publish with m.mu released. Bus.Publish runs every handler synchronously
	// on this goroutine and sync.RWMutex is not reentrant, so a subscriber that
	// calls back into the manager — Get, List — would block forever. The insert
	// above already happened, so the room is discoverable before the event
	// announces it. Same shape as CreateBridge/DeleteBridge.
	m.log.Info("room created", "room_id", id, "sample_rate", r.SampleRate)
	m.bus.Publish(events.RoomCreated, &events.RoomCreatedData{RoomScope: events.RoomScope{RoomID: id, AppID: appID}})
	return r, nil
}

// wireMixerPanicHook makes the room's mixer report a participant whose IO
// loop panicked, so whoever owns that participant can tear it down instead of
// leaving a dead audio path wired into a live room. Must be called for every
// room the manager owns, right after NewRoom.
//
// The hook fires on the panicking mixer goroutine as its last act before that
// goroutine exits, so teardown must not run inline: RemoveLeg and DeleteBridge
// publish on the bus, and WhatsAppLeg.Hangup hands ctx straight to Bye and can
// block on a non-responsive peer. (SIPLeg.Hangup returns immediately — its BYE
// is fire-and-forget — but it is not the only implementation.) Dispatch
// asynchronously, the same way Delete fans its hangups out.
func (m *Manager) wireMixerPanicHook(r *Room) {
	roomID := r.ID
	r.Mixer().SetOnParticipantPanic(func(p *mixer.Participant, loop string) {
		go m.tearDownPanickedParticipant(roomID, p, loop)
	})
}

// tearDownPanickedParticipant routes a panicking mixer participant to the
// component that owns it.
//
// The mixer's participant map is not leg-only: synthetic bridge endpoints
// (attachBridge) and the API layer's WebSocket, agent, playback and TTS
// sources all register there too, and each class is owned — and cleaned up —
// by something different. Treating every participant as a leg would fabricate
// leg.left_room, a documented webhook, for IDs that were never legs, while
// cleaning up nothing that actually leaked.
func (m *Manager) tearDownPanickedParticipant(roomID string, p *mixer.Participant, loop string) {
	participantID := p.ID

	// This runs on its own goroutine, so a panic here — Hangup reaching into
	// a wedged SIP stack, say — would take the process down. That is the exact
	// failure this teardown exists to contain, so it must contain its own.
	defer func() {
		if rec := recover(); rec != nil {
			m.log.Error("panic tearing down panicked mixer participant",
				"room_id", roomID,
				"participant_id", participantID,
				"panic", rec,
				"stack", string(debug.Stack()),
			)
		}
	}()

	if bridgeID, ok := bridgeIDFromParticipant(participantID); ok {
		m.tearDownPanickedBridge(roomID, bridgeID, loop)
		return
	}
	if l, ok := m.legMgr.Get(participantID); ok {
		m.tearDownPanickedLeg(roomID, l, p, loop)
		return
	}

	// A ws/agent/playback/TTS source: owned by the API layer, which the room
	// package cannot call into. The mixer has already closed the participant's
	// reader and writer, which is what unblocks those owners — a closed egress
	// pipe drops a ws client's send loop, a closed playback pipe fails the
	// player's next write — so each runs its own cleanup and publishes its own
	// event. There is nothing for the room layer to remove or hang up.
	m.log.Warn("mixer participant panicked; no leg or bridge owns it",
		"room_id", roomID,
		"participant_id", participantID,
		"loop", loop,
	)
}

// tearDownPanickedLeg removes a leg whose mixer IO loop panicked from its
// room and hangs it up. Further operation of that leg is unsafe: its audio
// path is gone, so leaving it connected would strand the caller on a deaf,
// mute call with no operator signal.
//
// p is the participant instance that panicked, and teardown is elected on it,
// not on the leg's presence in roomID. This runs on its own goroutine, so the
// leg may since have moved rooms, or left and returned to roomID, and be live
// on a fresh participant; the leg would then still be a member here and a
// membership-keyed election would hang up that healthy call and report it as
// a mixer panic. RemoveLegIfParticipant resolves the identity and removes
// under the room's lock in one step, so nothing lands in between.
//
// Removing nothing means the panicked path is already detached — by a move,
// a room delete, or an API removal — and each of those owns its own teardown.
// This path then does nothing at all: no hangup, no event, no hook.
func (m *Manager) tearDownPanickedLeg(roomID string, l leg.Leg, p *mixer.Participant, loop string) {
	legID := l.ID()

	r, ok := m.Get(roomID)
	if !ok {
		m.log.Debug("room of panicked leg already deleted",
			"room_id", roomID, "leg_id", legID)
		return
	}
	if !r.RemoveLegIfParticipant(legID, p) {
		m.log.Debug("panicked leg's audio path already replaced or detached",
			"room_id", roomID, "leg_id", legID, "loop", loop)
		return
	}

	m.log.Warn("tearing down leg after mixer IO panic",
		"room_id", roomID,
		"leg_id", legID,
		"loop", loop,
	)

	// RemoveLegIfParticipant did the removal RemoveLeg would have; this is the
	// event that goes with it.
	m.bus.Publish(events.LegLeftRoom, &events.LegLeftRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: roomID, AppID: l.AppID()},
	})
	if err := l.Hangup(context.Background()); err != nil {
		m.log.Error("hanging up panicked leg",
			"room_id", roomID, "leg_id", legID, "error", err)
	}

	// Tell the owner, if one registered. RemoveLeg and Hangup above keep the
	// room layer self-sufficient with no hook installed; the owner adds the
	// teardown the room package cannot reach — the CDR, the span, the per-leg
	// webhook and the leg manager entry, plus the room-scoped cleanup that
	// hangs off roomID (the room agent and the room recording).
	if fn := m.legPanicTeardownHook(); fn != nil {
		fn(l, roomID, legPanicReason)
	}
}

// tearDownPanickedBridge tears down the whole bridge behind a panicking
// synthetic bridge participant.
//
// Letting the mixer drop the participant is not enough: only detachBridge
// decrements the room's bridgeRefs, so a dead endpoint would keep
// mixerShouldRun() true and this room's mixer ticking forever behind an
// endpoint nobody reads, and the peer room would keep pushing audio into a
// conduit with no far side. DeleteBridge does the whole job — deregister,
// detach from both rooms, close both endpoints, publish room.unbridged — and
// its registry delete is the exactly-once gate for the case where both
// endpoints panic at once.
func (m *Manager) tearDownPanickedBridge(roomID, bridgeID, loop string) {
	m.log.Warn("tearing down bridge after mixer IO panic",
		"room_id", roomID,
		"bridge_id", bridgeID,
		"loop", loop,
	)

	err := m.DeleteBridge(bridgeID)
	switch {
	case err == nil:
	case errors.Is(err, ErrBridgeNotFound):
		// The peer endpoint's panic, or a concurrent delete, got there first.
		m.log.Debug("panicked bridge already torn down",
			"room_id", roomID, "bridge_id", bridgeID)
	default:
		m.log.Error("deleting panicked bridge",
			"room_id", roomID, "bridge_id", bridgeID, "error", err)
	}
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
	torn := m.collectBridgesForRoomLocked(id)
	appID := r.AppID
	m.mu.Unlock()

	// Tear down bridges referencing this room before hanging up legs, so the
	// bridge readLoop/writeLoop exit via endpoint Close rather than racing
	// the mixer Stop() inside r.Close().
	for _, t := range torn {
		m.teardownBridge(t.br, t.roomA, t.roomB)
		m.bus.Publish(events.RoomUnbridged, &events.RoomUnbridgedData{
			BridgeScope: events.BridgeScope{BridgeID: t.br.ID, RoomAID: t.br.RoomAID, RoomBID: t.br.RoomBID, AppID: appID},
			Reason:      "room_deleted",
		})
	}

	// Hangup all participants concurrently.
	// Bye() blocks waiting for a SIP response, so sequential hangups
	// would stall on the first leg and never reach the rest.
	var wg sync.WaitGroup
	for _, l := range r.Participants() {
		wg.Add(1)
		go func(l leg.Leg) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					m.log.Error("panic hanging up leg during room delete",
						"leg_id", l.ID(),
						"panic", rec,
						"stack", string(debug.Stack()),
					)
				}
			}()
			l.Hangup(context.Background())
		}(l)
	}
	wg.Wait()
	r.Close()
	m.bus.Publish(events.RoomDeleted, &events.RoomDeletedData{RoomScope: events.RoomScope{RoomID: id, AppID: r.AppID}})
	return nil
}

func (m *Manager) AddLeg(roomID, legID string) error {
	return m.addLeg(roomID, legID, nil)
}

// AddLegWithRole behaves like AddLeg but additionally sets the leg's
// routing role atomically so the room's routing matrix takes effect before
// the first mix tick that includes this leg.
func (m *Manager) AddLegWithRole(roomID, legID, role string) error {
	return m.addLeg(roomID, legID, &role)
}

func (m *Manager) addLeg(roomID, legID string, role *string) error {
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

	if role != nil {
		r.AddLegWithRole(l, *role)
	} else {
		r.AddLeg(l)
	}
	m.bus.Publish(events.LegJoinedRoom, &events.LegJoinedRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: roomID, AppID: l.AppID()},
	})
	if role != nil {
		m.bus.Publish(events.RoomRoutingChanged, &events.RoomRoutingChangedData{
			RoomScope: events.RoomScope{RoomID: roomID, AppID: r.AppID},
			Matrix:    r.RoutingMatrix(),
			Reason:    "leg_joined",
		})
	}
	return nil
}

// SetRoomRouting replaces the room's routing matrix and emits the
// room.routing_changed event.
func (m *Manager) SetRoomRouting(roomID string, matrix map[string][]string) error {
	r, ok := m.Get(roomID)
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	r.SetRoutingMatrix(matrix)
	m.bus.Publish(events.RoomRoutingChanged, &events.RoomRoutingChangedData{
		RoomScope: events.RoomScope{RoomID: roomID, AppID: r.AppID},
		Matrix:    r.RoutingMatrix(),
		Reason:    "set",
	})
	return nil
}

// UpdateRoomRoutingRow replaces a single listener-role row. sources == nil
// clears the row (full mesh for that role).
func (m *Manager) UpdateRoomRoutingRow(roomID, listenerRole string, sources []string) error {
	r, ok := m.Get(roomID)
	if !ok {
		return fmt.Errorf("room %s not found", roomID)
	}
	r.UpdateRoutingRow(listenerRole, sources)
	m.bus.Publish(events.RoomRoutingChanged, &events.RoomRoutingChangedData{
		RoomScope: events.RoomScope{RoomID: roomID, AppID: r.AppID},
		Matrix:    r.RoutingMatrix(),
		Reason:    "update",
	})
	return nil
}

// GetRoomRouting returns a snapshot of the room's routing matrix.
func (m *Manager) GetRoomRouting(roomID string) (map[string][]string, error) {
	r, ok := m.Get(roomID)
	if !ok {
		return nil, fmt.Errorf("room %s not found", roomID)
	}
	return r.RoutingMatrix(), nil
}

// SetLegRole changes a leg's routing role. If the leg is in a room, the
// room's routing-derived allow-sets are recomputed and a routing_changed
// event is emitted alongside leg.role_changed.
func (m *Manager) SetLegRole(legID, role string) error {
	l, ok := m.legMgr.Get(legID)
	if !ok {
		return fmt.Errorf("leg %s not found", legID)
	}
	oldRole := l.Role()
	if oldRole == role {
		return nil
	}
	roomID := l.RoomID()
	if roomID == "" {
		l.SetRole(role)
		m.bus.Publish(events.LegRoleChanged, &events.LegRoleChangedData{
			LegRoomScope: events.LegRoomScope{LegID: legID, AppID: l.AppID()},
			OldRole:      oldRole,
			NewRole:      role,
		})
		return nil
	}
	r, ok := m.Get(roomID)
	if !ok {
		l.SetRole(role)
		m.bus.Publish(events.LegRoleChanged, &events.LegRoleChangedData{
			LegRoomScope: events.LegRoomScope{LegID: legID, AppID: l.AppID()},
			OldRole:      oldRole,
			NewRole:      role,
		})
		return nil
	}
	if _, found := r.SetLegRole(legID, role); !found {
		return fmt.Errorf("leg %s not found in room %s", legID, roomID)
	}
	m.bus.Publish(events.LegRoleChanged, &events.LegRoleChangedData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: roomID, AppID: l.AppID()},
		OldRole:      oldRole,
		NewRole:      role,
	})
	m.bus.Publish(events.RoomRoutingChanged, &events.RoomRoutingChangedData{
		RoomScope: events.RoomScope{RoomID: roomID, AppID: r.AppID},
		Matrix:    r.RoutingMatrix(),
		Reason:    "leg_role_changed",
	})
	return nil
}

func (m *Manager) MoveLeg(fromRoomID, toRoomID, legID string) error {
	fromRoom, ok := m.Get(fromRoomID)
	if !ok {
		return fmt.Errorf("room %s not found", fromRoomID)
	}

	// Get or create target room. Moving into a room that already exists is the
	// common path, so take the read fast path first and only allocate on a
	// miss; a candidate that loses the race below is safe to drop because
	// nothing has been started on it.
	toRoom, ok := m.Get(toRoomID)
	created := false
	if !ok {
		candidate := NewRoom(toRoomID, "", fromRoom.SampleRate, m.log)
		m.wireMixerPanicHook(candidate)

		m.mu.Lock()
		// Re-check under the lock: without it, two concurrent moves into the
		// same absent room would both insert and both publish room.created.
		if toRoom, ok = m.rooms[toRoomID]; !ok {
			toRoom = candidate
			m.rooms[toRoomID] = candidate
			created = true
		}
		m.mu.Unlock()
	}
	// Publish with m.mu released, for the reason Create documents. Only the
	// goroutine that actually inserted publishes, which keeps room.created
	// exactly-once.
	if created {
		m.bus.Publish(events.RoomCreated, &events.RoomCreatedData{RoomScope: events.RoomScope{RoomID: toRoomID, AppID: toRoom.AppID}})
	}

	l, ok := fromRoom.DetachLeg(legID)
	if !ok {
		return fmt.Errorf("leg %s not found in room %s", legID, fromRoomID)
	}
	toRoom.AddLeg(l)

	m.bus.Publish(events.LegLeftRoom, &events.LegLeftRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: fromRoomID, AppID: l.AppID()},
	})
	m.bus.Publish(events.LegJoinedRoom, &events.LegJoinedRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: toRoomID, AppID: l.AppID()},
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
	legAppID := ""
	if l, ok := m.legMgr.Get(legID); ok {
		legAppID = l.AppID()
	}
	m.bus.Publish(events.LegLeftRoom, &events.LegLeftRoomData{
		LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: roomID, AppID: legAppID},
	})
	return nil
}

// --- Bridges ---

type bridgeTeardown struct {
	br           *Bridge
	roomA, roomB *Room
}

// collectBridgesForRoomLocked removes every bridge referencing roomID from
// the registry and returns them with their (still-registered) room pointers.
// Caller must hold m.mu.
func (m *Manager) collectBridgesForRoomLocked(roomID string) []bridgeTeardown {
	var torn []bridgeTeardown
	for bid, br := range m.bridges {
		if br.RoomAID == roomID || br.RoomBID == roomID {
			delete(m.bridges, bid)
			torn = append(torn, bridgeTeardown{br: br, roomA: m.rooms[br.RoomAID], roomB: m.rooms[br.RoomBID]})
		}
	}
	return torn
}

// teardownBridge detaches the bridge participant from both mixers (skipping a
// nil room, e.g. one being deleted) and closes the conduit. Must be called
// without m.mu held — detachBridge takes the room lock.
func (m *Manager) teardownBridge(br *Bridge, roomA, roomB *Room) {
	if roomA != nil {
		roomA.detachBridge(br.pid)
	}
	if roomB != nil {
		roomB.detachBridge(br.pid)
	}
	br.epA.Close()
	br.epB.Close()
}

// CreateBridge joins roomAID and roomBID so audio flows between their mixers
// per dir. Both rooms must exist and share a sample rate; a room cannot be
// bridged to itself or to a room it is already bridged with.
func (m *Manager) CreateBridge(id, roomAID, roomBID string, dir Direction) (*Bridge, error) {
	if !dir.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrBridgeDirection, dir)
	}
	if roomAID == roomBID {
		return nil, ErrBridgeSelf
	}
	if id == "" {
		id = uuid.New().String()
	}

	m.mu.Lock()
	roomA, okA := m.rooms[roomAID]
	if !okA {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrBridgeRoomMissing, roomAID)
	}
	roomB, okB := m.rooms[roomBID]
	if !okB {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrBridgeRoomMissing, roomBID)
	}
	if roomA.SampleRate != roomB.SampleRate {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: room %s is %dHz, room %s is %dHz",
			ErrBridgeSampleRate, roomAID, roomA.SampleRate, roomBID, roomB.SampleRate)
	}
	if _, exists := m.bridges[id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: bridge id %s already exists", ErrBridgeExists, id)
	}
	for _, b := range m.bridges {
		if (b.RoomAID == roomAID && b.RoomBID == roomBID) ||
			(b.RoomAID == roomBID && b.RoomBID == roomAID) {
			m.mu.Unlock()
			return nil, ErrBridgeExists
		}
	}

	epA, epB := bridge.NewPair(bridge.DefaultBufFrames)
	pid := bridgeParticipantID(id)
	br := &Bridge{ID: id, RoomAID: roomAID, RoomBID: roomBID, Direction: dir, epA: epA, epB: epB, pid: pid}
	m.bridges[id] = br
	appID := roomA.AppID
	m.mu.Unlock()

	aSends, bSends := dir.flags()
	roomA.attachBridge(pid, epA)
	roomB.attachBridge(pid, epB)
	roomA.setBridgeDirection(pid, aSends)
	roomB.setBridgeDirection(pid, bSends)

	m.log.Info("bridge created", "bridge_id", id, "room_a", roomAID, "room_b", roomBID, "direction", dir)
	m.bus.Publish(events.RoomBridged, &events.RoomBridgedData{
		BridgeScope: events.BridgeScope{BridgeID: id, RoomAID: roomAID, RoomBID: roomBID, AppID: appID},
		Direction:   string(dir),
	})
	return br, nil
}

func (m *Manager) GetBridge(id string) (*Bridge, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.bridges[id]
	return b, ok
}

func (m *Manager) ListBridges() []*Bridge {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Bridge, 0, len(m.bridges))
	for _, b := range m.bridges {
		out = append(out, b)
	}
	return out
}

// ListBridgesForRoom returns every bridge that has roomID as an endpoint.
func (m *Manager) ListBridgesForRoom(roomID string) []*Bridge {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Bridge, 0)
	for _, b := range m.bridges {
		if b.RoomAID == roomID || b.RoomBID == roomID {
			out = append(out, b)
		}
	}
	return out
}

// SetBridgeDirection changes a bridge's audio flow live, without
// interrupting audio or churning participants.
func (m *Manager) SetBridgeDirection(id string, dir Direction) error {
	if !dir.Valid() {
		return fmt.Errorf("%w: %q", ErrBridgeDirection, dir)
	}
	m.mu.Lock()
	br, ok := m.bridges[id]
	if !ok {
		m.mu.Unlock()
		return ErrBridgeNotFound
	}
	roomA := m.rooms[br.RoomAID]
	roomB := m.rooms[br.RoomBID]
	br.Direction = dir
	appID := ""
	if roomA != nil {
		appID = roomA.AppID
	}
	m.mu.Unlock()

	aSends, bSends := dir.flags()
	if roomA != nil {
		roomA.setBridgeDirection(br.pid, aSends)
	}
	if roomB != nil {
		roomB.setBridgeDirection(br.pid, bSends)
	}

	m.bus.Publish(events.RoomBridgeUpdated, &events.RoomBridgeUpdatedData{
		BridgeScope: events.BridgeScope{BridgeID: id, RoomAID: br.RoomAID, RoomBID: br.RoomBID, AppID: appID},
		Direction:   string(dir),
	})
	return nil
}

func (m *Manager) DeleteBridge(id string) error {
	m.mu.Lock()
	br, ok := m.bridges[id]
	if !ok {
		m.mu.Unlock()
		return ErrBridgeNotFound
	}
	delete(m.bridges, id)
	roomA := m.rooms[br.RoomAID]
	roomB := m.rooms[br.RoomBID]
	appID := ""
	if roomA != nil {
		appID = roomA.AppID
	}
	m.mu.Unlock()

	m.teardownBridge(br, roomA, roomB)
	m.bus.Publish(events.RoomUnbridged, &events.RoomUnbridgedData{
		BridgeScope: events.BridgeScope{BridgeID: id, RoomAID: br.RoomAID, RoomBID: br.RoomBID, AppID: appID},
	})
	return nil
}
