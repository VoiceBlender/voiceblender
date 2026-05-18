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

	r.syncMixerLocked()
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
