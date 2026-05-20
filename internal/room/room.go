package room

import (
	"log/slog"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/bridge"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
)

type Room struct {
	ID           string
	AppID        string
	SampleRate   int
	mu           sync.RWMutex
	participants map[string]leg.Leg
	mix          *mixer.Mixer
	log          *slog.Logger

	bridgeRefs   int  // synthetic bridge participants keeping the mixer alive
	mixerRunning bool // tracks whether r.mix is currently started

	// routing is the per-listener-role source whitelist. routing[role] is
	// the set of source roles that legs with `role` are allowed to hear.
	// nil/absent row means full mesh for that role. Guarded by r.mu.
	routing map[string]map[string]struct{}
}

func NewRoom(id, appID string, sampleRate int, log *slog.Logger) *Room {
	if sampleRate == 0 {
		sampleRate = mixer.DefaultSampleRate
	}
	return &Room{
		ID:           id,
		AppID:        appID,
		SampleRate:   sampleRate,
		participants: make(map[string]leg.Leg),
		mix:          mixer.New(log, sampleRate),
		log:          log,
	}
}

func (r *Room) Mixer() *mixer.Mixer {
	return r.mix
}

func (r *Room) AddLeg(l leg.Leg) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.addLegLocked(l)
	r.applyRoutingLocked()
	r.syncMixerLocked()
}

// AddLegWithRole adds a leg and applies role atomically before the mixer's
// next tick observes the new participant. role == "" leaves the leg unroled
// (full mesh per routing semantics).
func (r *Room) AddLegWithRole(l leg.Leg, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	l.SetRole(role)
	r.addLegLocked(l)
	r.applyRoutingLocked()
	r.syncMixerLocked()
}

func (r *Room) addLegLocked(l leg.Leg) {
	r.participants[l.ID()] = l
	l.SetRoomID(r.ID)

	reader := l.AudioReader()
	writer := l.AudioWriter()
	if reader != nil && writer != nil {
		// Wrap with rate-aware resamplers to bridge leg↔mixer rate difference.
		// When rates match (e.g. G.722 at 16kHz = mixer rate), this is a passthrough.
		legRate := l.SampleRate()
		mixRate := r.SampleRate
		r.mix.AddParticipant(l.ID(),
			mixer.NewResampleReader(reader, legRate, mixRate),
			mixer.NewResampleWriter(writer, mixRate, legRate),
		)
		// Sync mute/deaf state so legs muted/deafened before room join stay that way in mixer.
		if l.IsMuted() {
			r.mix.SetParticipantMuted(l.ID(), true)
		}
		if l.IsDeaf() {
			r.mix.SetParticipantDeaf(l.ID(), true)
		}
	}
}

// DetachLeg removes a leg from the room and returns it.
func (r *Room) DetachLeg(legID string) (leg.Leg, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	l, ok := r.participants[legID]
	if !ok {
		return nil, false
	}
	l.SetRoomID("")
	delete(r.participants, legID)
	r.mix.RemoveParticipant(legID)

	r.applyRoutingLocked()
	r.syncMixerLocked()
	return l, true
}

func (r *Room) RemoveLeg(legID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if l, ok := r.participants[legID]; ok {
		l.SetRoomID("")
		delete(r.participants, legID)
		r.mix.RemoveParticipant(legID)
	}

	r.applyRoutingLocked()
	r.syncMixerLocked()
}

func (r *Room) Participants() []leg.Leg {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]leg.Leg, 0, len(r.participants))
	for _, l := range r.participants {
		out = append(out, l)
	}
	return out
}

func (r *Room) ParticipantCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.participants)
}

func (r *Room) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mix.Stop()
	r.mixerRunning = false
	for _, l := range r.participants {
		l.SetRoomID("")
	}
	r.participants = make(map[string]leg.Leg)
}

// mixerShouldRun reports whether the mixer has any reason to keep ticking:
// at least one leg, or at least one attached bridge. Caller must hold r.mu.
func (r *Room) mixerShouldRun() bool {
	return len(r.participants) > 0 || r.bridgeRefs > 0
}

// syncMixerLocked starts or stops the mixer to match mixerShouldRun.
// Caller must hold r.mu.
func (r *Room) syncMixerLocked() {
	switch {
	case !r.mixerRunning && r.mixerShouldRun():
		r.mix.Start()
		r.mixerRunning = true
	case r.mixerRunning && !r.mixerShouldRun():
		r.mix.Stop()
		r.mixerRunning = false
	}
}

// attachBridge wires a synthetic bridge participant into this room's mixer
// and keeps the mixer alive while the bridge exists.
func (r *Room) attachBridge(participantID string, ep *bridge.Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mix.AddParticipant(participantID, ep, ep)
	r.bridgeRefs++
	r.syncMixerLocked()
}

// detachBridge removes a synthetic bridge participant. Safe to call for a
// participant that is no longer present (e.g. the room is being deleted).
func (r *Room) detachBridge(participantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mix.RemoveParticipant(participantID)
	if r.bridgeRefs > 0 {
		r.bridgeRefs--
	}
	r.syncMixerLocked()
}

// setBridgeDirection controls whether this room's bridge participant emits
// audio toward the peer room. send == false makes the participant deaf so it
// produces no mixed-minus-self output (that direction is off).
func (r *Room) setBridgeDirection(participantID string, send bool) {
	r.mix.SetParticipantDeaf(participantID, !send)
}

// --- Audio routing matrix ---

// SetRoutingMatrix replaces the full routing matrix and recomputes every
// leg's allow-set. Matrix is map[listener_role][]source_role.
func (r *Room) SetRoutingMatrix(matrix map[string][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routing = normalizeMatrix(matrix)
	r.applyRoutingLocked()
}

// UpdateRoutingRow replaces matrix[listenerRole]. Passing a nil sources
// slice clears the row (full mesh for that role).
func (r *Room) UpdateRoutingRow(listenerRole string, sources []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routing == nil {
		r.routing = make(map[string]map[string]struct{})
	}
	if sources == nil {
		delete(r.routing, listenerRole)
	} else {
		set := make(map[string]struct{}, len(sources))
		for _, s := range sources {
			set[s] = struct{}{}
		}
		r.routing[listenerRole] = set
	}
	r.applyRoutingLocked()
}

// RoutingMatrix returns a snapshot of the matrix as map[role][]source_roles.
func (r *Room) RoutingMatrix() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string][]string, len(r.routing))
	for role, sources := range r.routing {
		list := make([]string, 0, len(sources))
		for s := range sources {
			list = append(list, s)
		}
		out[role] = list
	}
	return out
}

// SetLegRole changes a leg's routing role and recomputes the matrix-derived
// allow-sets. Returns (oldRole, true) if the leg is in this room and the
// role was applied, or ("", false) if the leg is not present.
func (r *Room) SetLegRole(legID, role string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.participants[legID]
	if !ok {
		return "", false
	}
	old := l.Role()
	l.SetRole(role)
	r.applyRoutingLocked()
	return old, true
}

// normalizeMatrix converts the API shape into the internal set-of-sets shape.
func normalizeMatrix(m map[string][]string) map[string]map[string]struct{} {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]map[string]struct{}, len(m))
	for role, sources := range m {
		set := make(map[string]struct{}, len(sources))
		for _, s := range sources {
			set[s] = struct{}{}
		}
		out[role] = set
	}
	return out
}

// applyRoutingLocked recomputes every participant's flat allow-set from the
// current routing matrix + leg roles, then pushes them into the mixer in a
// single batch so mixTick observes an atomic update. Caller must hold r.mu.
//
// Semantics (from the plan):
//
//	if R_L == ""           → nil (full mesh)
//	else if M[R_L] is unset → nil (no row: full mesh)
//	else                    → { S.ID | S != L, S.role != "", S.role ∈ M[R_L] }
//
// A leg whose listener row is set never hears legs without a role
// (matrix-routed listener only hears roled sources).
func (r *Room) applyRoutingLocked() {
	if len(r.participants) == 0 {
		return
	}
	updates := make(map[string]map[string]struct{}, len(r.participants))
	for listenerID, listener := range r.participants {
		role := listener.Role()
		if role == "" {
			updates[listenerID] = nil
			continue
		}
		allowedRoles, ok := r.routing[role]
		if !ok {
			updates[listenerID] = nil
			continue
		}
		hears := make(map[string]struct{})
		for sourceID, source := range r.participants {
			if sourceID == listenerID {
				continue
			}
			srcRole := source.Role()
			if srcRole == "" {
				continue
			}
			if _, ok := allowedRoles[srcRole]; ok {
				hears[sourceID] = struct{}{}
			}
		}
		updates[listenerID] = hears
	}
	r.mix.ApplyHearsBatch(updates)
}
