package api

import (
	"net/http"

	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/leg"
	"github.com/pion/webrtc/v4"
)

func (s *Server) webrtcOffer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SDP string `json:"sdp"`
	}
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

	pc, err := webrtc.NewPeerConnection(config)
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
			s.LegMgr.Remove(l.ID())
			s.Bus.Publish(events.LegDisconnected, disconnectData(l, "ice_failure"))
			l.Hangup(r.Context())
		}
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

	// Gather ICE candidates
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		writeError(w, http.StatusInternalServerError, "failed to set local description")
		return
	}
	<-gatherComplete

	s.LegMgr.Add(l)
	s.Bus.Publish(events.LegConnected, map[string]interface{}{"leg_id": l.ID(), "type": "webrtc"})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"leg_id": l.ID(),
		"sdp":    pc.LocalDescription().SDP,
	})
}
