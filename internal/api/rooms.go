package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type roomView struct {
	ID           string    `json:"id"`
	Participants []legView `json:"participants"`
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		// Allow empty body
		req.ID = ""
	}

	room, err := s.RoomMgr.Create(req.ID)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, roomView{ID: room.ID, Participants: []legView{}})
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	rooms := s.RoomMgr.List()
	views := make([]roomView, len(rooms))
	for i, rm := range rooms {
		parts := rm.Participants()
		pViews := make([]legView, len(parts))
		for j, p := range parts {
			pViews[j] = toLegView(p)
		}
		views[i] = roomView{ID: rm.ID, Participants: pViews}
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
	pViews := make([]legView, len(parts))
	for j, p := range parts {
		pViews[j] = toLegView(p)
	}
	writeJSON(w, http.StatusOK, roomView{ID: rm.ID, Participants: pViews})
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.cleanupRoomAgent(id)
	if err := s.RoomMgr.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) addLegToRoom(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	var req struct {
		LegID string `json:"leg_id"`
	}
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
	if err := s.RoomMgr.RemoveLeg(roomID, legID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.stopRoomAgentIfEmpty(roomID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}
