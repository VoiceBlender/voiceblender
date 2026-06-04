package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/lkmedia"
	"github.com/go-chi/chi/v5"
	"github.com/livekit/protocol/livekit"
)

// createLiveKitRoomLeg handles POST /v1/legs with type=livekit_room. It
// resolves the JWT (either passed by the caller or minted from
// LIVEKIT_API_KEY/SECRET when LIVEKIT_TOKEN_SIGNING_ENABLED=true), opens
// the LK transport, registers the leg, and spawns a watcher that publishes
// leg.disconnected when the LK session ends.
func (s *Server) createLiveKitRoomLeg(w http.ResponseWriter, r *http.Request, req CreateLegRequest) {
	if !s.Config.LiveKitEnabled {
		writeError(w, http.StatusServiceUnavailable, "LiveKit is not enabled (set LIVEKIT_ENABLED=true)")
		return
	}
	if req.LiveKit == nil {
		writeError(w, http.StatusBadRequest, "type=livekit_room requires `livekit` parameters")
		return
	}
	p := req.LiveKit

	url := p.URL
	if url == "" {
		url = s.Config.LiveKitURL
	}
	if url == "" {
		writeError(w, http.StatusBadRequest, "LiveKit URL is required (set LIVEKIT_URL or pass livekit.url)")
		return
	}

	token, err := s.resolveLiveKitToken(p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	bitrate := p.OpusBitrate
	if bitrate == 0 {
		bitrate = s.Config.LiveKitOpusBitrate
	}
	cfg := lkmedia.Config{
		OpusBitrate: bitrate,
		Log:         s.Log,
	}
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.RoomID != "" {
		if _, ok := s.RoomMgr.Get(req.RoomID); !ok {
			if _, err := s.RoomMgr.Create(req.RoomID, req.AppID, s.Config.DefaultSampleRate); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("create room: %v", err))
				return
			}
		}
	}

	headers := captureCustomHeaders(r.Header)

	// Connect synchronously: errors here surface as a clean HTTP response
	// (no events emitted, no leg registered).
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tr, err := lkmedia.NewTransport(ctx, cfg,
		lkmedia.SignalConfig{URL: url, Token: token, Log: s.Log},
		lkmedia.PeerConfig{
			RTPPortMin: s.Config.RTPPortMin,
			RTPPortMax: s.Config.RTPPortMax,
		},
		lkmedia.Callbacks{},
	)
	if err != nil {
		s.Log.Warn("livekit transport connect failed", "error", err)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("livekit connect: %v", err))
		return
	}

	l := leg.NewLiveKitLeg(tr, headers, cfg.SampleRate, s.Log)
	if req.AppID != "" {
		l.SetAppID(req.AppID)
	}
	// Wire callbacks to bridge LK observability into the event bus, scoped
	// to this leg. Done after the leg has an ID we can scope events to.
	wireLiveKitCallbacks(tr, l, s.Bus)

	s.LegMgr.Add(l)
	if req.WebhookURL != "" {
		s.Webhooks.SetLegWebhook(l.ID(), req.WebhookURL, req.WebhookSecret)
	}

	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  string(l.Type()),
	})

	if req.RoomID != "" {
		if err := s.RoomMgr.AddLeg(req.RoomID, l.ID()); err != nil {
			s.Log.Warn("auto-add livekit leg to room failed", "leg_id", l.ID(), "room_id", req.RoomID, "error", err)
		} else {
			s.onLegJoinedRoom(req.RoomID, l.ID())
		}
	}

	// Watcher: when the transport closes (server-side leave, signaling
	// failure, etc.) we publish leg.disconnected once.
	go s.watchLiveKitTransport(l, tr)

	writeJSON(w, http.StatusCreated, toLegView(l))
}

// resolveLiveKitToken returns the JWT to use for this leg-create request.
// Caller-supplied Token wins; otherwise mint one if signing is enabled.
func (s *Server) resolveLiveKitToken(p *LiveKitParams) (string, error) {
	if p.Token != "" {
		return p.Token, nil
	}
	if !s.Config.LiveKitTokenSigningEnabled {
		return "", errors.New("livekit.token is required (server token minting is disabled — set LIVEKIT_TOKEN_SIGNING_ENABLED=true to mint from {room,identity})")
	}
	if s.Config.LiveKitAPIKey == "" || s.Config.LiveKitAPISecret == "" {
		return "", errors.New("LIVEKIT_TOKEN_SIGNING_ENABLED=true but LIVEKIT_API_KEY/LIVEKIT_API_SECRET are not configured")
	}
	if p.Room == "" || p.Identity == "" {
		return "", errors.New("livekit.room and livekit.identity are required when minting")
	}

	ttl := s.Config.LiveKitDefaultTokenTTL
	if p.TokenTTL != "" {
		d, err := time.ParseDuration(p.TokenTTL)
		if err != nil {
			return "", fmt.Errorf("livekit.token_ttl: %w", err)
		}
		ttl = d
	}

	canPub, canSub, canData, admin := true, true, false, false
	if perm := p.Permissions; perm != nil {
		if perm.CanPublish != nil {
			canPub = *perm.CanPublish
		}
		if perm.CanSubscribe != nil {
			canSub = *perm.CanSubscribe
		}
		if perm.CanPublishData != nil {
			canData = *perm.CanPublishData
		}
		if perm.RoomAdmin != nil {
			admin = *perm.RoomAdmin
		}
	}

	return lkmedia.MintJoinToken(s.Config.LiveKitAPIKey, s.Config.LiveKitAPISecret, lkmedia.JoinClaims{
		Identity:       p.Identity,
		Name:           p.ParticipantName,
		Room:           p.Room,
		TTL:            ttl,
		CanPublish:     canPub,
		CanSubscribe:   canSub,
		CanPublishData: canData,
		RoomAdmin:      admin,
	})
}

// watchLiveKitTransport blocks on the transport's Done channel and, when
// it closes, publishes leg.disconnected (single-flight via the leg's
// ClaimDisconnect) and runs cleanup.
func (s *Server) watchLiveKitTransport(l *leg.LiveKitLeg, tr *lkmedia.Transport) {
	<-tr.Done()
	reason := tr.CloseReason()
	if reason == "" {
		reason = "livekit_disconnected"
	}
	if !l.ClaimDisconnect() {
		return
	}
	s.cleanupLeg(l)
	s.publishDisconnect(l, reason)
}

// listLiveKitParticipants handles GET /v1/legs/{id}/livekit/participants.
// Returns the current snapshot of LK participants the leg is connected to.
func (s *Server) listLiveKitParticipants(w http.ResponseWriter, r *http.Request) {
	l, tr, ok := s.lookupLiveKitLeg(w, r)
	if !ok {
		return
	}
	parts := tr.Participants()
	out := make([]liveKitParticipantView, 0, len(parts))
	for _, p := range parts {
		out = append(out, toLiveKitParticipantView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"leg_id":       l.ID(),
		"participants": out,
	})
}

// muteLiveKitParticipant handles POST /v1/legs/{id}/livekit/participants/{identity}/mute.
// Sends a server-side MuteTrack signal for the participant's audio
// track. Requires the leg's JWT to carry roomAdmin=true.
func (s *Server) muteLiveKitParticipant(w http.ResponseWriter, r *http.Request) {
	_, tr, ok := s.lookupLiveKitLeg(w, r)
	if !ok {
		return
	}
	identity := chi.URLParam(r, "identity")
	if identity == "" {
		writeError(w, http.StatusBadRequest, "identity is required")
		return
	}
	if err := tr.MuteRemoteTrackByIdentity(identity, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "mute_requested"})
}

// lookupLiveKitLeg fetches the leg by URL param, narrows it to
// LiveKitLeg, and returns both the leg and its transport. Writes the
// appropriate error response and returns ok=false on any failure.
func (s *Server) lookupLiveKitLeg(w http.ResponseWriter, r *http.Request) (*leg.LiveKitLeg, *lkmedia.Transport, bool) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return nil, nil, false
	}
	lkl, ok := l.(*leg.LiveKitLeg)
	if !ok {
		writeError(w, http.StatusBadRequest, "leg is not a livekit_room leg")
		return nil, nil, false
	}
	tr := lkl.Transport()
	if tr == nil {
		writeError(w, http.StatusInternalServerError, "leg has no transport")
		return nil, nil, false
	}
	return lkl, tr, true
}

// liveKitParticipantView is the JSON shape returned by GET
// .../livekit/participants. Subset of the LK ParticipantInfo proto
// surfaced for observability.
type liveKitParticipantView struct {
	Identity string                       `json:"identity"`
	Name     string                       `json:"name,omitempty"`
	SID      string                       `json:"sid,omitempty"`
	State    string                       `json:"state,omitempty"`
	Tracks   []liveKitParticipantTrackRef `json:"tracks,omitempty"`
}

type liveKitParticipantTrackRef struct {
	SID   string `json:"sid"`
	Name  string `json:"name,omitempty"`
	Kind  string `json:"kind"` // "audio" | "video"
	Muted bool   `json:"muted,omitempty"`
}

func toLiveKitParticipantView(p *livekit.ParticipantInfo) liveKitParticipantView {
	out := liveKitParticipantView{
		Identity: p.GetIdentity(),
		Name:     p.GetName(),
		SID:      p.GetSid(),
		State:    p.GetState().String(),
	}
	for _, t := range p.GetTracks() {
		out.Tracks = append(out.Tracks, liveKitParticipantTrackRef{
			SID:   t.GetSid(),
			Name:  t.GetName(),
			Kind:  t.GetType().String(),
			Muted: t.GetMuted(),
		})
	}
	return out
}

// wireLiveKitCallbacks attaches lkmedia.Transport observability callbacks
// to the event bus, scoped to the given leg's ID. Diffs participant
// updates against the prior snapshot to emit synthetic joined/left
// events (LiveKit's ParticipantUpdate carries snapshots, not deltas).
func wireLiveKitCallbacks(tr *lkmedia.Transport, l *leg.LiveKitLeg, bus eventPublisher) {
	prevParticipants := map[string]struct{}{}
	prevSpeakers := map[string]struct{}{}

	cb := lkmedia.Callbacks{
		OnParticipantsUpdated: func(updates []*livekit.ParticipantInfo) {
			scope := events.LegScope{LegID: l.ID(), AppID: l.AppID()}
			for _, p := range updates {
				if p == nil || p.GetIdentity() == "" {
					continue
				}
				id := p.GetIdentity()
				if p.GetState() == livekit.ParticipantInfo_DISCONNECTED {
					if _, was := prevParticipants[id]; was {
						bus.Publish(events.LiveKitParticipantLeft, &events.LiveKitParticipantLeftData{
							LegScope: scope, Identity: id, SID: p.GetSid(),
						})
					}
					delete(prevParticipants, id)
					continue
				}
				if _, was := prevParticipants[id]; !was {
					bus.Publish(events.LiveKitParticipantJoined, &events.LiveKitParticipantJoinedData{
						LegScope: scope, Identity: id, Name: p.GetName(), SID: p.GetSid(),
					})
					prevParticipants[id] = struct{}{}
				}
			}
		},
		OnSpeakersChanged: func(speakers []*livekit.SpeakerInfo) {
			scope := events.LegScope{LegID: l.ID(), AppID: l.AppID()}
			now := map[string]float64{}
			for _, sp := range speakers {
				if sp.GetActive() {
					now[sp.GetSid()] = float64(sp.GetLevel())
				}
			}
			for sid, lvl := range now {
				if _, was := prevSpeakers[sid]; !was {
					bus.Publish(events.LiveKitParticipantSpeakingStarted, &events.LiveKitParticipantSpeakingData{
						LegScope: scope, SID: sid, Level: lvl,
					})
				}
			}
			for sid := range prevSpeakers {
				if _, still := now[sid]; !still {
					bus.Publish(events.LiveKitParticipantSpeakingStopped, &events.LiveKitParticipantSpeakingData{
						LegScope: scope, SID: sid,
					})
				}
			}
			prevSpeakers = map[string]struct{}{}
			for sid := range now {
				prevSpeakers[sid] = struct{}{}
			}
		},
		OnTrackPublishedRemote: func(participantSID string, ti *livekit.TrackInfo) {
			bus.Publish(events.LiveKitTrackPublished, &events.LiveKitTrackPublishedData{
				LegScope:       events.LegScope{LegID: l.ID(), AppID: l.AppID()},
				ParticipantSID: participantSID,
				TrackSID:       ti.GetSid(),
				TrackName:      ti.GetName(),
				TrackKind:      ti.GetType().String(),
			})
		},
		OnTrackUnpublishedSID: func(trackSID string) {
			bus.Publish(events.LiveKitTrackUnpublished, &events.LiveKitTrackUnpublishedData{
				LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
				TrackSID: trackSID,
			})
		},
	}
	tr.SetCallbacks(cb)
}

// eventPublisher narrows the bus to the one method this file uses, which
// also makes it trivial to substitute in tests.
type eventPublisher interface {
	Publish(events.EventType, events.EventData)
}
