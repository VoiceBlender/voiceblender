package api

import (
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/go-chi/chi/v5"
	"github.com/pion/webrtc/v4"
)

// WebRTCOfferResult is the response payload for webrtc_offer (REST and VSI).
type WebRTCOfferResult struct {
	LegID string `json:"leg_id"`
	SDP   string `json:"sdp"`
}

// WebRTCCandidatesResult is the response payload for webrtc_get_candidates
// (REST and VSI). `Done` indicates that ICE gathering has finished — once
// true, no further candidates will be produced.
type WebRTCCandidatesResult struct {
	Candidates []webrtc.ICECandidateInit `json:"candidates"`
	Done       bool                      `json:"done"`
}

func (s *Server) doWebRTCOffer(req WebRTCOfferRequest) (*WebRTCOfferResult, error) {
	var l *leg.WebRTCLeg
	media, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:       codec.CodecOpus,
		ICEServers:  s.Config.ICEServers,
		ExternalIPs: s.Config.WebRTCExternalIPs,
		RTPPortMin:  uint16(s.Config.RTPPortMin),
		RTPPortMax:  uint16(s.Config.RTPPortMax),
		Log:         s.Log,
		OnDisconnect: func(reason string) {
			if l != nil {
				s.cleanupLeg(l)
				s.publishDisconnect(l, "ice_failure")
			}
		},
		OnConnected: func() {
			if l == nil {
				return
			}
			s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
				LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
				LegType:  "webrtc",
			})
			s.maybeStartSpeakingDetector(l, s.takeSpeechOverride(l.ID()))
		},
	})
	if err != nil {
		return nil, newAPIError(http.StatusInternalServerError, "failed to create peer connection")
	}

	pc := media.PC()
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.SDP}
	if err := pc.SetRemoteDescription(offer); err != nil {
		media.Close()
		return nil, newAPIError(http.StatusBadRequest, "invalid SDP offer")
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		media.Close()
		return nil, newAPIError(http.StatusInternalServerError, "failed to create answer")
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		media.Close()
		return nil, newAPIError(http.StatusInternalServerError, "failed to set local description")
	}

	l = leg.NewWebRTCLeg(media, s.Log)
	// Must precede Add: OnConnected can fire immediately and publishes l.AppID().
	if req.AppID != "" {
		l.SetAppID(req.AppID)
	}
	s.LegMgr.Add(l)

	return &WebRTCOfferResult{LegID: l.ID(), SDP: answer.SDP}, nil
}

func (s *Server) doWebRTCAddCandidate(legID string, c webrtc.ICECandidateInit) error {
	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	wl, ok := l.(*leg.WebRTCLeg)
	if !ok {
		return newAPIError(http.StatusBadRequest, "leg is not a WebRTC leg")
	}
	if err := wl.AddICECandidate(c); err != nil {
		return newAPIError(http.StatusInternalServerError, "failed to add ICE candidate")
	}
	return nil
}

func (s *Server) doWebRTCGetCandidates(legID string) (*WebRTCCandidatesResult, error) {
	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	wl, ok := l.(*leg.WebRTCLeg)
	if !ok {
		return nil, newAPIError(http.StatusBadRequest, "leg is not a WebRTC leg")
	}
	candidates, done := wl.DrainCandidates()
	if candidates == nil {
		candidates = []webrtc.ICECandidateInit{}
	}
	return &WebRTCCandidatesResult{Candidates: candidates, Done: done}, nil
}

func (s *Server) webrtcOffer(w http.ResponseWriter, r *http.Request) {
	var req WebRTCOfferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	result, err := s.doWebRTCOffer(req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) webrtcAddCandidate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var candidate webrtc.ICECandidateInit
	if err := decodeJSON(r, &candidate); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.doWebRTCAddCandidate(id, candidate); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "added"})
}

func (s *Server) webrtcGetCandidates(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	result, err := s.doWebRTCGetCandidates(id)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
