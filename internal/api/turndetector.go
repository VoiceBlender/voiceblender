package api

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/turndetector"
)

// newTurnProvider constructs the appropriate Provider based on the configured transport.
func (s *Server) newTurnProvider(onError func(reason, msg string, fatal bool)) turndetector.Provider {
	opts := turndetector.Options{
		ServiceURL:      s.Config.SmartTurnURL,
		VAD:             s.Config.SmartTurnVAD,
		Threshold:       s.Config.SmartTurnThreshold,
		PauseDurationMs: s.Config.SmartTurnPauseDurationMs,
		Adaptive:        s.Config.SmartTurnAdaptive,
		OnError:         onError,
	}
	var p turndetector.Provider
	if s.Config.SmartTurnTransport == "http" {
		hp := turndetector.NewHTTP(s.Log)
		hp.SetOptions(opts)
		p = hp
	} else {
		wp := turndetector.NewWS(s.Log)
		wp.SetOptions(opts)
		p = wp
	}
	return p
}

// maybeStartTurnDetector attaches a SmartTurn session only when turn detection
// is enabled for this leg (per-call override or server-wide default) and a
// SmartTurn URL is configured.
func (s *Server) maybeStartTurnDetector(l leg.Leg, override *bool) {
	if s.Config.SmartTurnURL == "" {
		return
	}
	if !resolveSpeechDetection(override, s.Config.SmartTurnEnabled) {
		return
	}
	s.startTurnDetector(l)
}

// startTurnDetector creates, wires, and starts a SmartTurn session for a connected leg.
func (s *Server) startTurnDetector(l leg.Leg) {
	// M3: guard against leg types whose SetSpeakingTap is a no-op. Starting
	// a provider for these legs wastes a WS connection and never receives audio.
	switch l.(type) {
	case *leg.WebSocketLeg, *leg.MoQLeg, *leg.LiveKitPublishLeg, *leg.LiveKitParticipantLeg:
		s.Log.Warn("SmartTurn: leg type does not support speaking tap, skipping",
			"leg_id", l.ID(), "type", fmt.Sprintf("%T", l))
		return
	}

	transport := s.Config.SmartTurnTransport
	scope := events.LegRoomScope{LegID: l.ID(), RoomID: l.RoomID(), AppID: l.AppID()}

	onError := func(reason, msg string, fatal bool) {
		s.Log.Warn("smartturn error",
			slog.String("reason", reason),
			slog.String("message", msg),
			slog.Bool("fatal", fatal),
			slog.String("leg_id", l.ID()),
			slog.String("transport", transport),
		)
		s.Bus.Publish(events.TurnError, &events.TurnErrorData{
			LegRoomScope: scope,
			Transport:    transport,
			Reason:       reason,
			Fatal:        fatal,
			Message:      msg,
		})
	}

	provider := s.newTurnProvider(onError)

	// onEvent translates a SmartTurn decision into a VoiceBlender bus event.
	onEvent := func(ev turndetector.Event) {
		typ := events.TurnIncomplete
		if ev.Complete {
			typ = events.TurnComplete
		}
		s.Log.Info("smartturn turn decision",
			slog.String("action", string(typ)),
			slog.Float64("probability", ev.Probability),
			slog.Float64("threshold", ev.ThresholdUsed),
			slog.Int64("processing_ms", ev.ProcessingMs),
			slog.Int64("roundtrip_ms", ev.RoundtripMs),
			slog.String("transport", transport),
		)
		s.Bus.Publish(typ, &events.TurnDetectionData{
			LegRoomScope: scope,
			Transport:    transport,
			Probability:  ev.Probability,
			Threshold:    ev.ThresholdUsed,
			ProcessingMs: ev.ProcessingMs,
			RoundtripMs:  ev.RoundtripMs,
		})
	}

	if err := provider.Start(l.Context(), onEvent); err != nil {
		s.Log.Error("SmartTurn provider failed to start", "err", err, "leg_id", l.ID())
		s.Bus.Publish(events.TurnError, &events.TurnErrorData{
			LegRoomScope: scope,
			Transport:    transport,
			Reason:       "start_failed",
			Fatal:        true,
			Message:      err.Error(),
		})
		return
	}

	// Wrap provider with a resampler to guarantee 16kHz PCM reaching SmartTurn.
	// NewResampleWriter is an automatic no-op passthrough when l.SampleRate() == 16000.
	turnWriter := mixer.NewResampleWriter(provider, l.SampleRate(), mixer.DefaultSampleRate)

	// M2: single lock hold to read-modify-write speakDets, avoiding TOCTOU.
	// SetSpeakingTap is called exactly once after determining the final writer.
	s.speakMu.Lock()
	existingDet, hasDet := s.speakDets[l.ID()]
	s.speakMu.Unlock()

	if hasDet {
		l.SetSpeakingTap(io.MultiWriter(existingDet, turnWriter))
	} else {
		l.SetSpeakingTap(turnWriter)
	}

	// M1: HTTP mode — hook into speaking.stopped bus events to trigger evaluation.
	// This replaces the previous approach of building a second speaking.Detector
	// (which duplicated callback logic and broke speaking event publishing).
	// Bus.Publish is synchronous but NotifyPause only holds its mutex briefly
	// before spawning a goroutine, so running inline from the bus handler is safe.
	if transport == "http" {
		if hp, ok := provider.(*turndetector.HTTPProvider); ok {
			legID := l.ID()

			// If no speaking detector exists, create one first so speaking events
			// are published correctly. The bus subscriber below will then fire on
			// its speaking.stopped events.
			if !hasDet {
				s.startSpeakingDetector(l)
			}

			unsub := s.Bus.Subscribe(func(e events.Event) {
				if e.Type != events.SpeakingStopped {
					return
				}
				if e.Data == nil || e.Data.GetLegID() != legID {
					return
				}
				hp.NotifyPause(l.Context())
			})

			s.turnUnsubsMu.Lock()
			s.turnUnsubs[legID] = unsub
			s.turnUnsubsMu.Unlock()
		}
	}

	s.turnDetMu.Lock()
	s.turnDets[l.ID()] = provider
	s.turnDetMu.Unlock()

	s.Log.Info("SmartTurn session started", "leg_id", l.ID(), "transport", transport)
}

// stopTurnDetector stops and removes the SmartTurn session for a leg.
func (s *Server) stopTurnDetector(legID string) {
	// Cancel the HTTP-mode bus subscriber first, if any.
	s.turnUnsubsMu.Lock()
	unsub, hasUnsub := s.turnUnsubs[legID]
	if hasUnsub {
		delete(s.turnUnsubs, legID)
	}
	s.turnUnsubsMu.Unlock()
	if hasUnsub {
		unsub()
	}

	s.turnDetMu.Lock()
	p, ok := s.turnDets[legID]
	if ok {
		delete(s.turnDets, legID)
	}
	s.turnDetMu.Unlock()
	if ok {
		p.Stop()
	}
}

// HasTurnDetector reports whether a SmartTurn session is active for the given leg.
// Primarily for tests.
func (s *Server) HasTurnDetector(legID string) bool {
	s.turnDetMu.Lock()
	defer s.turnDetMu.Unlock()
	_, ok := s.turnDets[legID]
	return ok
}

// GetTurnProvider returns the active TurnProvider for the given leg.
// Primarily for tests.
func (s *Server) GetTurnProvider(legID string) (turndetector.Provider, bool) {
	s.turnDetMu.Lock()
	defer s.turnDetMu.Unlock()
	p, ok := s.turnDets[legID]
	return p, ok
}

// setTurnOverride stashes a per-leg turn_detection override to be applied
// when the leg transitions to connected. Mirrors setSpeechOverride.
func (s *Server) setTurnOverride(legID string, override *bool) {
	s.turnOverrideMu.Lock()
	s.turnOverride[legID] = override
	s.turnOverrideMu.Unlock()
}

// takeTurnOverride retrieves and removes the stored per-leg turn_detection
// override. Mirrors takeSpeechOverride.
func (s *Server) takeTurnOverride(legID string) *bool {
	s.turnOverrideMu.Lock()
	defer s.turnOverrideMu.Unlock()
	ov, ok := s.turnOverride[legID]
	if ok {
		delete(s.turnOverride, legID)
	}
	return ov
}
