package api

import (
	"context"
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/go-chi/chi/v5"
)

func (s *Server) doCreateRoom(req CreateRoomRequest) (RoomView, error) {
	rate := req.SampleRate
	if rate == 0 {
		rate = s.Config.DefaultSampleRate
	}
	if !mixer.ValidSampleRate(rate) {
		return RoomView{}, newAPIError(http.StatusBadRequest, "invalid sample_rate: must be 8000, 16000, or 48000")
	}
	room, err := s.RoomMgr.Create(req.ID, req.AppID, rate)
	if err != nil {
		return RoomView{}, newAPIError(http.StatusConflict, "%s", err.Error())
	}
	if req.WebhookURL != "" {
		s.Webhooks.SetRoomWebhook(room.ID, req.WebhookURL, req.WebhookSecret)
	}
	return RoomView{ID: room.ID, AppID: room.AppID, SampleRate: room.SampleRate, Participants: []LegView{}}, nil
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req CreateRoomRequest
	if err := decodeJSON(r, &req); err != nil {
		req = CreateRoomRequest{}
	}

	view, err := s.doCreateRoom(req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
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
		views[i] = RoomView{ID: rm.ID, AppID: rm.AppID, SampleRate: rm.SampleRate, Participants: pViews}
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
	writeJSON(w, http.StatusOK, RoomView{ID: rm.ID, AppID: rm.AppID, SampleRate: rm.SampleRate, Participants: pViews})
}

func (s *Server) doDeleteRoom(id string) error {
	s.cleanupRoomAgent(id)
	s.Webhooks.ClearRoomWebhook(id)
	if err := s.RoomMgr.Delete(id); err != nil {
		return newAPIError(http.StatusNotFound, "%s", err.Error())
	}
	return nil
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doDeleteRoom(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) doAddLegToRoom(ctx context.Context, roomID string, req AddLegRequest) (interface{}, error) {
	l, ok := s.LegMgr.Get(req.LegID)
	if !ok {
		return nil, newAPIError(http.StatusBadRequest, "leg %s not found", req.LegID)
	}

	// Auto-create the room if it doesn't exist, inheriting app_id from the leg.
	if _, ok := s.RoomMgr.Get(roomID); !ok {
		if _, err := s.RoomMgr.Create(roomID, l.AppID(), s.Config.DefaultSampleRate); err != nil {
			return nil, newAPIError(http.StatusInternalServerError, "create room: %v", err)
		}
	}

	// Apply mute/deaf before the leg enters the mixer so the participant
	// is added with the desired state in a single atomic step.
	if req.Mute != nil {
		l.SetMuted(*req.Mute)
	}
	if req.Deaf != nil {
		l.SetDeaf(*req.Deaf)
	}
	if req.AcceptDTMF != nil {
		l.SetAcceptDTMF(*req.AcceptDTMF)
	}

	// If the leg is already in a room, move it instead of adding.
	if fromRoomID, inRoom := s.RoomMgr.FindLegRoom(req.LegID); inRoom {
		if fromRoomID == roomID {
			return nil, newAPIError(http.StatusBadRequest, "leg already in this room")
		}
		s.onLegLeavingRoomRecording(fromRoomID, req.LegID)
		if err := s.RoomMgr.MoveLeg(fromRoomID, roomID, req.LegID); err != nil {
			return nil, newAPIError(http.StatusBadRequest, "%s", err.Error())
		}
		s.onLegJoinedRoom(roomID, req.LegID)
		s.stopRoomAgentIfEmpty(fromRoomID)
		return map[string]string{
			"status": "moved",
			"from":   fromRoomID,
			"to":     roomID,
		}, nil
	}

	// Auto-answer ringing inbound SIP legs before adding to the room.
	if sipLeg, ok := l.(*leg.SIPLeg); ok && l.State() == leg.StateRinging && l.Type() == leg.TypeSIPInbound {
		sipLeg.SignalAnswer()
		if err := sipLeg.WaitConnected(ctx); err != nil {
			return nil, newAPIError(http.StatusInternalServerError, "auto-answer failed: %v", err)
		}
	}

	if err := s.RoomMgr.AddLeg(roomID, req.LegID); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "%s", err.Error())
	}
	s.onLegJoinedRoom(roomID, req.LegID)
	return map[string]string{"status": "added"}, nil
}

func (s *Server) addLegToRoom(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	var req AddLegRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	result, err := s.doAddLegToRoom(r.Context(), roomID, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) doRemoveLegFromRoom(roomID, legID string) error {
	s.onLegLeavingRoomRecording(roomID, legID)
	if err := s.RoomMgr.RemoveLeg(roomID, legID); err != nil {
		return newAPIError(http.StatusBadRequest, "%s", err.Error())
	}
	s.stopRoomAgentIfEmpty(roomID)
	return nil
}

func (s *Server) removeLegFromRoom(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	legID := chi.URLParam(r, "legID")
	if err := s.doRemoveLegFromRoom(roomID, legID); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}
