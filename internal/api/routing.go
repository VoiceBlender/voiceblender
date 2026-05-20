package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) doGetRoomRouting(roomID string) (RoomRoutingView, error) {
	matrix, err := s.RoomMgr.GetRoomRouting(roomID)
	if err != nil {
		return RoomRoutingView{}, newAPIError(http.StatusNotFound, "%s", err.Error())
	}
	return RoomRoutingView{Matrix: matrix}, nil
}

func (s *Server) getRoomRouting(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	view, err := s.doGetRoomRouting(roomID)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) doSetRoomRouting(roomID string, req RoomRoutingRequest) (RoomRoutingView, error) {
	if err := s.RoomMgr.SetRoomRouting(roomID, req.Matrix); err != nil {
		return RoomRoutingView{}, newAPIError(http.StatusNotFound, "%s", err.Error())
	}
	return s.doGetRoomRouting(roomID)
}

func (s *Server) setRoomRouting(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	var req RoomRoutingRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doSetRoomRouting(roomID, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) doUpdateRoomRouting(roomID string, req RoomRoutingUpdateRequest) (RoomRoutingView, error) {
	for _, u := range req.Updates {
		if err := s.RoomMgr.UpdateRoomRoutingRow(roomID, u.ListenerRole, u.Sources); err != nil {
			return RoomRoutingView{}, newAPIError(http.StatusNotFound, "%s", err.Error())
		}
	}
	return s.doGetRoomRouting(roomID)
}

func (s *Server) updateRoomRouting(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	var req RoomRoutingUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doUpdateRoomRouting(roomID, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) doSetLegRole(legID string, req SetLegRoleRequest) (LegView, error) {
	if err := s.RoomMgr.SetLegRole(legID, req.Role); err != nil {
		return LegView{}, newAPIError(http.StatusNotFound, "%s", err.Error())
	}
	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return LegView{}, newAPIError(http.StatusNotFound, "leg %s not found", legID)
	}
	return toLegView(l), nil
}

func (s *Server) setLegRole(w http.ResponseWriter, r *http.Request) {
	legID := chi.URLParam(r, "id")
	var req SetLegRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doSetLegRole(legID, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
