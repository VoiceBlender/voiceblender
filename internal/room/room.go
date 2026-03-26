package room

import (
	"log/slog"
	"sync"

	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/leg"
	"github.com/csiwek/VoiceBlender/internal/mixer"
)

type Room struct {
	ID           string
	mu           sync.RWMutex
	participants map[string]leg.Leg
	mix          *mixer.Mixer
	log          *slog.Logger
}

func NewRoom(id string, bus *events.Bus, log *slog.Logger) *Room {
	r := &Room{
		ID:           id,
		participants: make(map[string]leg.Leg),
		mix:          mixer.New(log),
		log:          log,
	}
	r.mix.OnSpeaking(func(e mixer.SpeakingEvent) {
		typ := events.SpeakingStarted
		if !e.Speaking {
			typ = events.SpeakingStopped
		}
		bus.Publish(typ, map[string]interface{}{
			"leg_id":  e.ParticipantID,
			"room_id": id,
		})
	})
	return r
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
		mixRate := mixer.SampleRate
		r.mix.AddParticipant(l.ID(),
			mixer.NewResampleReader(reader, legRate, mixRate),
			mixer.NewResampleWriter(writer, mixRate, legRate),
		)
		// Sync mute state so legs muted before room join stay muted in mixer.
		if l.IsMuted() {
			r.mix.SetParticipantMuted(l.ID(), true)
		}
	}

	// Start mixer on first participant
	if len(r.participants) == 1 {
		r.mix.Start()
	}
}

func (r *Room) RemoveLeg(legID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if l, ok := r.participants[legID]; ok {
		l.SetRoomID("")
		delete(r.participants, legID)
		r.mix.RemoveParticipant(legID)
	}

	if len(r.participants) == 0 {
		r.mix.Stop()
	}
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
	for _, l := range r.participants {
		l.SetRoomID("")
	}
	r.participants = make(map[string]leg.Leg)
}
