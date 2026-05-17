package api

import (
	"errors"
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/room"
	"github.com/go-chi/chi/v5"
)

// restToCanonical maps a path-relative direction (send/receive/...) to the
// canonical room-A direction. pathIsA is true when the room in the request
// path is the bridge's room A. ok is false for an unknown value.
func restToCanonical(rest string, pathIsA bool) (dir room.Direction, ok bool) {
	switch rest {
	case "", "bidirectional":
		return room.DirectionBidirectional, true
	case "none":
		return room.DirectionNone, true
	case "send":
		if pathIsA {
			return room.DirectionAToB, true
		}
		return room.DirectionBToA, true
	case "receive":
		if pathIsA {
			return room.DirectionBToA, true
		}
		return room.DirectionAToB, true
	}
	return "", false
}

// canonicalToRest is the inverse of restToCanonical.
func canonicalToRest(d room.Direction, pathIsA bool) string {
	switch d {
	case room.DirectionBidirectional:
		return "bidirectional"
	case room.DirectionNone:
		return "none"
	case room.DirectionAToB:
		if pathIsA {
			return "send"
		}
		return "receive"
	case room.DirectionBToA:
		if pathIsA {
			return "receive"
		}
		return "send"
	}
	return string(d)
}

func bridgeErrToAPI(err error) *apiError {
	switch {
	case errors.Is(err, room.ErrBridgeSelf),
		errors.Is(err, room.ErrBridgeSampleRate),
		errors.Is(err, room.ErrBridgeDirection):
		return newAPIError(http.StatusBadRequest, "%s", err.Error())
	case errors.Is(err, room.ErrBridgeRoomMissing),
		errors.Is(err, room.ErrBridgeNotFound):
		return newAPIError(http.StatusNotFound, "%s", err.Error())
	case errors.Is(err, room.ErrBridgeExists):
		return newAPIError(http.StatusConflict, "%s", err.Error())
	default:
		return newAPIError(http.StatusInternalServerError, "%s", err.Error())
	}
}

// bridgeView renders a bridge from the perspective of the room in the path.
func (s *Server) bridgeView(pathRoomID string, br *room.Bridge) BridgeView {
	pathIsA := pathRoomID == br.RoomAID
	peer := br.RoomBID
	if !pathIsA {
		peer = br.RoomAID
	}
	rate := 0
	if rm, ok := s.RoomMgr.Get(pathRoomID); ok {
		rate = rm.SampleRate
	}
	return BridgeView{
		ID:         br.ID,
		RoomID:     peer,
		Direction:  canonicalToRest(br.Direction, pathIsA),
		SampleRate: rate,
	}
}

// bridgeForRoom looks up a bridge and verifies it has roomID as an endpoint.
func (s *Server) bridgeForRoom(roomID, bridgeID string) (*room.Bridge, bool) {
	br, ok := s.RoomMgr.GetBridge(bridgeID)
	if !ok || (br.RoomAID != roomID && br.RoomBID != roomID) {
		return nil, false
	}
	return br, true
}

func (s *Server) doCreateRoomBridge(roomID string, req CreateRoomBridgeRequest) (BridgeView, error) {
	if req.RoomID == "" {
		return BridgeView{}, newAPIError(http.StatusBadRequest, "room_id is required")
	}
	// On create, the path room becomes the bridge's room A.
	dir, ok := restToCanonical(req.Direction, true)
	if !ok {
		return BridgeView{}, newAPIError(http.StatusBadRequest, "invalid direction: must be bidirectional, send, receive, or none")
	}
	br, err := s.RoomMgr.CreateBridge(req.ID, roomID, req.RoomID, dir)
	if err != nil {
		return BridgeView{}, bridgeErrToAPI(err)
	}
	return s.bridgeView(roomID, br), nil
}

func (s *Server) createRoomBridge(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	var req CreateRoomBridgeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doCreateRoomBridge(roomID, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) listRoomBridges(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	if _, ok := s.RoomMgr.Get(roomID); !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}
	brs := s.RoomMgr.ListBridgesForRoom(roomID)
	views := make([]BridgeView, len(brs))
	for i, br := range brs {
		views[i] = s.bridgeView(roomID, br)
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getRoomBridge(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	bridgeID := chi.URLParam(r, "bridgeID")
	br, ok := s.bridgeForRoom(roomID, bridgeID)
	if !ok {
		writeError(w, http.StatusNotFound, "bridge not found")
		return
	}
	writeJSON(w, http.StatusOK, s.bridgeView(roomID, br))
}

func (s *Server) doUpdateRoomBridge(roomID, bridgeID string, req UpdateRoomBridgeRequest) (BridgeView, error) {
	br, ok := s.bridgeForRoom(roomID, bridgeID)
	if !ok {
		return BridgeView{}, newAPIError(http.StatusNotFound, "bridge not found")
	}
	if req.Direction == "" {
		return BridgeView{}, newAPIError(http.StatusBadRequest, "direction is required")
	}
	pathIsA := roomID == br.RoomAID
	dir, valid := restToCanonical(req.Direction, pathIsA)
	if !valid {
		return BridgeView{}, newAPIError(http.StatusBadRequest, "invalid direction: must be bidirectional, send, receive, or none")
	}
	if err := s.RoomMgr.SetBridgeDirection(bridgeID, dir); err != nil {
		return BridgeView{}, bridgeErrToAPI(err)
	}
	updated, _ := s.RoomMgr.GetBridge(bridgeID)
	return s.bridgeView(roomID, updated), nil
}

func (s *Server) updateRoomBridge(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	bridgeID := chi.URLParam(r, "bridgeID")
	var req UpdateRoomBridgeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doUpdateRoomBridge(roomID, bridgeID, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) doDeleteRoomBridge(roomID, bridgeID string) error {
	if _, ok := s.bridgeForRoom(roomID, bridgeID); !ok {
		return newAPIError(http.StatusNotFound, "bridge not found")
	}
	if err := s.RoomMgr.DeleteBridge(bridgeID); err != nil {
		return bridgeErrToAPI(err)
	}
	return nil
}

func (s *Server) deleteRoomBridge(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	bridgeID := chi.URLParam(r, "bridgeID")
	if err := s.doDeleteRoomBridge(roomID, bridgeID); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
