package api

import (
	"context"
	"net/http"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/go-chi/chi/v5"
)

func (s *Server) sendDTMF(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	var req DTMFRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.Digits == "" {
		writeError(w, http.StatusBadRequest, "digits required")
		return
	}

	if err := l.SendDTMF(r.Context(), req.Digits); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) acceptDTMFLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	l.SetAcceptDTMF(true)
	writeJSON(w, http.StatusOK, map[string]string{"status": "dtmf_accepting"})
}

func (s *Server) rejectDTMFLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	l.SetAcceptDTMF(false)
	writeJSON(w, http.StatusOK, map[string]string{"status": "dtmf_rejecting"})
}

// broadcastDTMF forwards a DTMF digit received on fromLegID to every other
// leg in the same room that has accept_dtmf enabled and supports SendDTMF.
// WebRTC legs are skipped (their SendDTMF is not yet implemented).
func (s *Server) broadcastDTMF(fromLegID string, digit rune) {
	roomID, ok := s.RoomMgr.FindLegRoom(fromLegID)
	if !ok {
		return
	}
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}
	digits := string(digit)
	for _, p := range rm.Participants() {
		if p.ID() == fromLegID || !p.AcceptDTMF() {
			continue
		}
		if p.Type() != leg.TypeSIPInbound && p.Type() != leg.TypeSIPOutbound {
			continue
		}
		go func(target leg.Leg) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := target.SendDTMF(ctx, digits); err != nil {
				s.Log.Warn("dtmf forward failed", "from_leg", fromLegID, "to_leg", target.ID(), "digit", digits, "error", err)
			}
		}(p)
	}
}
