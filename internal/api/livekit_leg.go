package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/lkmedia"
)

// Role labels used by the LiveKit integration. The publish leg only hears
// VB legs whose role is NOT roleLiveKitListen — that's how we keep LK
// participants' audio from being looped back to LiveKit.
const (
	roleLiveKitPublish = "livekit_publish"
	roleLiveKitListen  = "livekit_listen"
)

// createLiveKitRoomLeg handles POST /v1/legs with type=livekit_room. It
// opens the LK signaling + WebRTC connection, registers a publish leg
// (the outbound audio direction), and wires callbacks so each remote LK
// participant becomes its own VB LiveKitParticipantLeg in the same VB
// room. Model B: the VB room mixer drives audio for everyone; there is no
// bespoke sum-mixer.
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

	// NewTransport is synchronous: any failure here returns a clean HTTP
	// response with no events emitted and no leg registered. We pass
	// empty callbacks first; SetCallbacks runs once we have the publish
	// leg to close over. The brief window where OnTrack may fire before
	// callbacks are set is handled by Transport.fireRemoteAudioTrack —
	// it drains the pcm reader rather than blocking.
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

	publishLeg := leg.NewLiveKitPublishLeg(tr, headers, cfg.SampleRate, s.Log)
	publishLeg.SetRole(roleLiveKitPublish)
	if req.AppID != "" {
		publishLeg.SetAppID(req.AppID)
	}

	conn := &liveKitConn{
		server:     s,
		publishLeg: publishLeg,
		transport:  tr,
		tracks:     map[string]string{},
	}
	tr.SetCallbacks(lkmedia.Callbacks{
		OnRemoteAudioTrack:      conn.handleRemoteAudioTrack,
		OnRemoteAudioTrackEnded: conn.handleRemoteAudioTrackEnded,
	})

	s.LegMgr.Add(publishLeg)
	if req.WebhookURL != "" {
		s.Webhooks.SetLegWebhook(publishLeg.ID(), req.WebhookURL, req.WebhookSecret)
	}

	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: publishLeg.ID(), AppID: publishLeg.AppID()},
		LegType:  string(publishLeg.Type()),
	})

	if req.RoomID != "" {
		if err := s.RoomMgr.AddLeg(req.RoomID, publishLeg.ID()); err != nil {
			s.Log.Warn("auto-add livekit publish leg to room failed",
				"leg_id", publishLeg.ID(), "room_id", req.RoomID, "error", err)
		} else {
			s.onLegJoinedRoom(req.RoomID, publishLeg.ID())
		}
	}

	// Bus subscription that recomputes the publish leg's Hears whitelist
	// whenever a leg joins or leaves its room. Prevents audio feedback by
	// excluding any leg whose role is roleLiveKitListen.
	unsubHears := s.subscribeLKPublishHears(publishLeg)
	s.recomputeLKPublishHears(publishLeg)

	go conn.watch(unsubHears)

	writeJSON(w, http.StatusCreated, toLegView(publishLeg))
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

// liveKitConn owns one umbrella LiveKit connection: the publish leg, the
// transport, and the live trackSID→participant-leg-ID index. Created in
// createLiveKitRoomLeg; lives as long as the transport.
type liveKitConn struct {
	server     *Server
	publishLeg *leg.LiveKitPublishLeg
	transport  *lkmedia.Transport

	tracksMu sync.Mutex
	tracks   map[string]string // trackSID → participant leg ID
}

// handleRemoteAudioTrack auto-creates a LiveKitParticipantLeg when the
// transport surfaces a subscribed remote audio track. Adds it to the
// publish leg's VB room so the VB room mixer wires it in automatically.
func (c *liveKitConn) handleRemoteAudioTrack(identity, _, trackSID string, pcm io.Reader) {
	// Defensive: if a track arrives with no identity (e.g. ParticipantUpdate
	// raced behind OnTrack and we somehow still made it here) just drain.
	if identity == "" {
		go io.Copy(io.Discard, pcm)
		return
	}

	pl := leg.NewLiveKitParticipantLeg(identity, trackSID, pcm, c.publishLeg.SampleRate(), c.server.Log)
	pl.SetRole(roleLiveKitListen)
	if appID := c.publishLeg.AppID(); appID != "" {
		pl.SetAppID(appID)
	}

	c.server.LegMgr.Add(pl)
	c.server.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: pl.ID(), AppID: pl.AppID()},
		LegType:  string(pl.Type()),
	})

	c.tracksMu.Lock()
	c.tracks[trackSID] = pl.ID()
	c.tracksMu.Unlock()

	if roomID := c.publishLeg.RoomID(); roomID != "" {
		if err := c.server.RoomMgr.AddLeg(roomID, pl.ID()); err != nil {
			c.server.Log.Warn("auto-add LK participant leg to room failed",
				"leg_id", pl.ID(), "room_id", roomID, "error", err)
		} else {
			c.server.onLegJoinedRoom(roomID, pl.ID())
		}
	}
}

// handleRemoteAudioTrackEnded tears down the participant leg matching a
// trackSID. Idempotent.
func (c *liveKitConn) handleRemoteAudioTrackEnded(trackSID string) {
	c.tracksMu.Lock()
	legID, ok := c.tracks[trackSID]
	delete(c.tracks, trackSID)
	c.tracksMu.Unlock()
	if !ok {
		return
	}
	c.disconnectParticipantLeg(legID, "livekit_participant_left")
}

func (c *liveKitConn) disconnectParticipantLeg(legID, reason string) {
	l, ok := c.server.LegMgr.Get(legID)
	if !ok {
		return
	}
	pl, ok := l.(*leg.LiveKitParticipantLeg)
	if !ok {
		return
	}
	// publishDisconnect runs ClaimDisconnect internally; calling it here
	// would double-claim and silently swallow the event.
	c.server.cleanupLeg(pl)
	c.server.publishDisconnect(pl, reason)
}

// cleanupAllParticipants is called when the umbrella tears down to ensure
// no participant leg is leaked.
func (c *liveKitConn) cleanupAllParticipants(reason string) {
	c.tracksMu.Lock()
	legIDs := make([]string, 0, len(c.tracks))
	for _, id := range c.tracks {
		legIDs = append(legIDs, id)
	}
	c.tracks = map[string]string{}
	c.tracksMu.Unlock()
	for _, id := range legIDs {
		c.disconnectParticipantLeg(id, reason)
	}
}

// watch blocks on the transport's Done channel, then drives the umbrella
// shutdown: participant legs first, then the publish leg.
func (c *liveKitConn) watch(unsubHears func()) {
	<-c.transport.Done()
	if unsubHears != nil {
		unsubHears()
	}
	reason := c.transport.CloseReason()
	if reason == "" {
		reason = "livekit_disconnected"
	}
	c.cleanupAllParticipants(reason)
	c.server.cleanupLeg(c.publishLeg)
	c.server.publishDisconnect(c.publishLeg, reason)
}

// subscribeLKPublishHears wires a bus subscription that recomputes the
// publish leg's Hears whitelist on every leg join/leave in its room.
// Returns the unsubscribe function.
func (s *Server) subscribeLKPublishHears(publishLeg *leg.LiveKitPublishLeg) func() {
	return s.Bus.Subscribe(func(ev events.Event) {
		if ev.Type != events.LegJoinedRoom && ev.Type != events.LegLeftRoom {
			return
		}
		if ev.Data == nil {
			return
		}
		// Only react to changes in the publish leg's room.
		if ev.Data.GetRoomID() != publishLeg.RoomID() {
			return
		}
		s.recomputeLKPublishHears(publishLeg)
	})
}

// recomputeLKPublishHears rewrites the publish leg's Hears whitelist to
// include every leg in its room EXCEPT participant legs (role =
// roleLiveKitListen) and the publish leg itself. This is what keeps LK
// participants' audio from being re-published to LiveKit (audio feedback).
func (s *Server) recomputeLKPublishHears(publishLeg *leg.LiveKitPublishLeg) {
	roomID := publishLeg.RoomID()
	if roomID == "" {
		return
	}
	room, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}
	hears := map[string]struct{}{}
	for _, l := range room.Participants() {
		if l.ID() == publishLeg.ID() {
			continue
		}
		if l.Role() == roleLiveKitListen {
			continue
		}
		hears[l.ID()] = struct{}{}
	}
	room.Mixer().SetParticipantHears(publishLeg.ID(), hears)
}
