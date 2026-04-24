package api

import (
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/codec"
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

	var l *leg.WebRTCLeg
	media, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:      codec.CodecPCMU,
		ICEServers: s.Config.ICEServers,
		RTPPortMin: uint16(s.Config.RTPPortMin),
		RTPPortMax: uint16(s.Config.RTPPortMax),
		Log:        s.Log,
		OnDisconnect: func(reason string) {
			if l != nil {
				s.cleanupLeg(l)
				s.publishDisconnect(l, "ice_failure")
			}
		},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create peer connection")
		return
	}

	pc := media.PC()
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.SDP}
	if err := pc.SetRemoteDescription(offer); err != nil {
		media.Close()
		writeError(w, http.StatusBadRequest, "invalid SDP offer")
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		media.Close()
		writeError(w, http.StatusInternalServerError, "failed to create answer")
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		media.Close()
		writeError(w, http.StatusInternalServerError, "failed to set local description")
		return
	}

	l = leg.NewWebRTCLeg(media, s.Log)

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
