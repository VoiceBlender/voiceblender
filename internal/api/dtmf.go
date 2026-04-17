package api

import (
	"context"
	"net/http"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/go-chi/chi/v5"
)

func (s *Server) doSendLegDTMF(ctx context.Context, id, digits string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	if digits == "" {
		return newAPIError(http.StatusBadRequest, "digits required")
	}

	if err := l.SendDTMF(ctx, digits); err != nil {
		return newAPIError(http.StatusInternalServerError, "%s", err.Error())
	}
	return nil
}

func (s *Server) sendDTMF(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req DTMFRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if err := s.doSendLegDTMF(r.Context(), id, req.Digits); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) doAcceptLegDTMF(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	l.SetAcceptDTMF(true)
	return nil
}

func (s *Server) acceptDTMFLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doAcceptLegDTMF(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "dtmf_accepting"})
}

func (s *Server) doRejectLegDTMF(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	l.SetAcceptDTMF(false)
	return nil
}

func (s *Server) rejectDTMFLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doRejectLegDTMF(id); err != nil {
		handleAPIError(w, err)
		return
	}
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
