package api

import (
	"io"
	"log/slog"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/speaking"
	"github.com/VoiceBlender/voiceblender/internal/turndetector"
)

// newTurnProvider constructs the appropriate Provider based on the configured transport.
func (s *Server) newTurnProvider() turndetector.Provider {
	opts := turndetector.Options{
		ServiceURL:      s.Config.SmartTurnURL,
		VAD:             s.Config.SmartTurnVAD,
		Threshold:       s.Config.SmartTurnThreshold,
		PauseDurationMs: s.Config.SmartTurnPauseDurationMs,
		Adaptive:        s.Config.SmartTurnAdaptive,
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
	transport := s.Config.SmartTurnTransport
	provider := s.newTurnProvider()

	scope := events.LegRoomScope{LegID: l.ID(), RoomID: l.RoomID(), AppID: l.AppID()}

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
			slog.String("transport", transport),
		)
		s.Bus.Publish(typ, &events.TurnDetectionData{
			LegRoomScope: scope,
			Transport:    transport,
			Probability:  ev.Probability,
			Threshold:    ev.ThresholdUsed,
			ProcessingMs: ev.ProcessingMs,
		})
	}

	if err := provider.Start(l.Context(), onEvent); err != nil {
		s.Log.Error("SmartTurn provider failed to start", "err", err, "leg_id", l.ID())
		return
	}

	// Wrap provider with a resampler to guarantee 16kHz PCM reaching SmartTurn.
	// NewResampleWriter is an automatic no-op passthrough when l.SampleRate() == 16000.
	turnWriter := mixer.NewResampleWriter(provider, l.SampleRate(), mixer.DefaultSampleRate)

	// Plug the provider into the leg's speaking tap.
	// If a speaking.Detector is already wired, combine them with io.MultiWriter
	// so both detectors receive the same PCM stream.
	s.speakMu.Lock()
	existingDet, hasDet := s.speakDets[l.ID()]
	s.speakMu.Unlock()

	if hasDet {
		l.SetSpeakingTap(io.MultiWriter(existingDet, turnWriter))
	} else {
		l.SetSpeakingTap(turnWriter)
	}

	// HTTP mode: drive evaluation from speaking.stopped events.
	// We wrap the existing speaking detector's event callback to also call
	// NotifyPause so the HTTPProvider sends the buffered audio when speech ends.
	if transport == "http" {
		if hp, ok := provider.(*turndetector.HTTPProvider); ok {
			s.speakMu.Lock()
			if hasDet {
				// Replace the existing detector with one whose callback also notifies the HTTP provider.
				legID := l.ID()
				sampleRate := l.SampleRate()
				det := speaking.New(legID, sampleRate, l.IsMuted, func(e speaking.Event) {
					typ := events.SpeakingStarted
					if !e.Speaking {
						typ = events.SpeakingStopped
						hp.NotifyPause(l.Context())
					}
					s.Bus.Publish(typ, &events.SpeakingData{
						LegRoomScope: events.LegRoomScope{LegID: e.LegID, RoomID: l.RoomID(), AppID: l.AppID()},
					})
				})
				// Swap the old detector for the new combined one.
				if old, ok := s.speakDets[legID]; ok {
					old.Stop()
				}
				s.speakDets[legID] = det
				s.speakMu.Unlock()

				l.SetSpeakingTap(io.MultiWriter(det, turnWriter))
				det.Start()
			} else {
				s.speakMu.Unlock()
				// No existing speaking detector; create one solely for turn detection signals.
				legID := l.ID()
				sampleRate := l.SampleRate()
				det := speaking.New(legID, sampleRate, l.IsMuted, func(e speaking.Event) {
					if !e.Speaking {
						hp.NotifyPause(l.Context())
					}
				})
				s.speakMu.Lock()
				s.speakDets[legID] = det
				s.speakMu.Unlock()

				l.SetSpeakingTap(io.MultiWriter(det, turnWriter))
				det.Start()
			}
		}
	}

	s.turnDetMu.Lock()
	s.turnDets[l.ID()] = provider
	s.turnDetMu.Unlock()

	s.Log.Info("SmartTurn session started", "leg_id", l.ID(), "transport", transport)
}

// stopTurnDetector stops and removes the SmartTurn session for a leg.
func (s *Server) stopTurnDetector(legID string) {
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

