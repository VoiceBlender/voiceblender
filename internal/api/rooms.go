package api

import (
	"fmt"
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/go-chi/chi/v5"
)

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req CreateRoomRequest
	if err := decodeJSON(r, &req); err != nil {
		// Allow empty body
		req.ID = ""
	}

	room, err := s.RoomMgr.Create(req.ID)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if req.WebhookURL != "" {
		s.Webhooks.SetRoomWebhook(room.ID, req.WebhookURL, req.WebhookSecret)
	}
	writeJSON(w, http.StatusCreated, RoomView{ID: room.ID, Participants: []LegView{}})
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	rooms := s.RoomMgr.List()
	views := make([]RoomView, len(rooms))
	for i, rm := range rooms {
		parts := rm.Participants()
		pViews := make([]LegView, len(parts))
		for j, p := range parts {
			pViews[j] = toLegView(p)
		}
		views[i] = RoomView{ID: rm.ID, Participants: pViews}
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	parts := rm.Participants()
	pViews := make([]LegView, len(parts))
	for j, p := range parts {
		pViews[j] = toLegView(p)
	}
	writeJSON(w, http.StatusOK, RoomView{ID: rm.ID, Participants: pViews})
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.cleanupRoomAgent(id)
	s.Webhooks.ClearRoomWebhook(id)
	if err := s.RoomMgr.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) addLegToRoom(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	var req AddLegRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Auto-create the room if it doesn't exist.
	if _, ok := s.RoomMgr.Get(roomID); !ok {
		if _, err := s.RoomMgr.Create(roomID); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("create room: %v", err))
			return
		}
	}

	l, ok := s.LegMgr.Get(req.LegID)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("leg %s not found", req.LegID))
		return
	}

	// Apply mute/deaf before the leg enters the mixer so the participant
	// is added with the desired state in a single atomic step (no frame
	// of un-muted audio leaks into the mix).
	if req.Mute != nil {
		l.SetMuted(*req.Mute)
	}
	if req.Deaf != nil {
		l.SetDeaf(*req.Deaf)
	}

	// If the leg is already in a room, move it instead of adding.
	if fromRoomID, inRoom := s.RoomMgr.FindLegRoom(req.LegID); inRoom {
		if fromRoomID == roomID {
			writeError(w, http.StatusBadRequest, "leg already in this room")
			return
		}
		// Stop per-participant recording in the old room before moving.
		s.onLegLeavingRoomRecording(fromRoomID, req.LegID)
		if err := s.RoomMgr.MoveLeg(fromRoomID, roomID, req.LegID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.onLegJoinedRoom(roomID, req.LegID)
		s.stopRoomAgentIfEmpty(fromRoomID)
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "moved",
			"from":   fromRoomID,
			"to":     roomID,
		})
		return
	}

	// Auto-answer ringing inbound SIP legs before adding to the room.
	if sipLeg, ok := l.(*leg.SIPLeg); ok && l.State() == leg.StateRinging && l.Type() == leg.TypeSIPInbound {
		sipLeg.SignalAnswer()
		if err := sipLeg.WaitConnected(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("auto-answer failed: %v", err))
			return
		}
	}

	if err := s.RoomMgr.AddLeg(roomID, req.LegID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.onLegJoinedRoom(roomID, req.LegID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

func (s *Server) removeLegFromRoom(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	legID := chi.URLParam(r, "legID")
	// Stop per-participant recording before removing from mixer.
	s.onLegLeavingRoomRecording(roomID, legID)
	if err := s.RoomMgr.RemoveLeg(roomID, legID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.stopRoomAgentIfEmpty(roomID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}
