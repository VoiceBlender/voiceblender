package api

import (
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/go-chi/chi/v5"
	"github.com/pion/webrtc/v4"
)

func (s *Server) webrtcOffer(w http.ResponseWriter, r *http.Request) {
	var req WebRTCOfferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Configure ICE servers
	iceServers := make([]webrtc.ICEServer, 0, len(s.Config.ICEServers))
	for _, url := range s.Config.ICEServers {
		if url != "" {
			iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{url}})
		}
	}

	config := webrtc.Configuration{
		ICEServers: iceServers,
	}

	var (
		pc  *webrtc.PeerConnection
		err error
	)
	if s.Config.RTPPortMin > 0 && s.Config.RTPPortMax > 0 {
		se := webrtc.SettingEngine{}
		se.SetEphemeralUDPPortRange(uint16(s.Config.RTPPortMin), uint16(s.Config.RTPPortMax))
		api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
		pc, err = api.NewPeerConnection(config)
	} else {
		pc, err = webrtc.NewPeerConnection(config)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create peer connection")
		return
	}

	// Create local track for sending audio to browser
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		"audio", "voiceblender",
	)
	if err != nil {
		pc.Close()
		writeError(w, http.StatusInternalServerError, "failed to create audio track")
		return
	}
	if _, err := pc.AddTrack(localTrack); err != nil {
		pc.Close()
		writeError(w, http.StatusInternalServerError, "failed to add track")
		return
	}

	// Create the WebRTC leg
	l := leg.NewWebRTCLeg(pc, localTrack, s.Log)

	// Handle incoming tracks
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		l.HandleTrack(track, receiver)
	})

	// Handle ICE connection state changes
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "ice_failure")
		}
	})

	// Trickle ICE: buffer locally gathered candidates for the client to poll
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			l.SetICEGatheringDone()
			return
		}
		init := c.ToJSON()
		l.PushLocalCandidate(init)
	})

	// Set remote description
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  req.SDP,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		writeError(w, http.StatusBadRequest, "invalid SDP offer")
		return
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		writeError(w, http.StatusInternalServerError, "failed to create answer")
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		writeError(w, http.StatusInternalServerError, "failed to set local description")
		return
	}

	// Register leg immediately — no waiting for ICE gathering
	s.LegMgr.Add(l)
	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  "webrtc",
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"leg_id": l.ID(),
		"sdp":    answer.SDP,
	})
}

func (s *Server) webrtcAddCandidate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	wl, ok := l.(*leg.WebRTCLeg)
	if !ok {
		writeError(w, http.StatusBadRequest, "leg is not a WebRTC leg")
		return
	}

	var candidate webrtc.ICECandidateInit
	if err := decodeJSON(r, &candidate); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if err := wl.AddICECandidate(candidate); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to add ICE candidate")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "added"})
}

func (s *Server) webrtcGetCandidates(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	wl, ok := l.(*leg.WebRTCLeg)
	if !ok {
		writeError(w, http.StatusBadRequest, "leg is not a WebRTC leg")
		return
	}

	candidates, done := wl.DrainCandidates()
	if candidates == nil {
		candidates = []webrtc.ICECandidateInit{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"candidates": candidates,
		"done":       done,
	})
}
