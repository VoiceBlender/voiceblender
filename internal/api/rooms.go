package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
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
	// Snapshot participants before tearing the room down so we can publish
	// leg.disconnected per leg afterwards. RoomMgr.Delete hangs the legs up
	// (sends BYE) but does not surface them as disconnect events on its own.
	var participants []leg.Leg
	appID := ""
	if rm, ok := s.RoomMgr.Get(id); ok {
		participants = rm.Participants()
		appID = rm.AppID
	}

	s.cleanupRoomAgent(id)
	// Deleting the room detaches every leg from it, so the per-leg cleanup
	// below cannot reach the room-scoped teardown — RoomMgr.Delete clears each
	// leg's RoomID, and the room is gone from the manager by then anyway. The
	// agent is handled by the explicit call above; the recording needs the
	// same treatment or it is never finalized and recording.finished never
	// fires for a deleted room.
	s.finalizeRoomRecording(id, appID, "room deleted")
	s.Webhooks.ClearRoomWebhook(id)
	if err := s.RoomMgr.Delete(id); err != nil {
		return newAPIError(http.StatusNotFound, "%s", err.Error())
	}

	// RoomMgr.Delete already called Hangup on each leg. Run the standard
	// cleanup + disconnect-publish for each former participant. The
	// ClaimDisconnect gate in publishDisconnect deduplicates against any
	// concurrent termination path (e.g. a racing DELETE /legs/{id}).
	for _, l := range participants {
		s.cleanupLeg(l)
		s.publishDisconnect(l, "room_deleted")
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

	addLeg := func(roomID string) error {
		if req.Role != nil {
			return s.RoomMgr.AddLegWithRole(roomID, req.LegID, *req.Role)
		}
		return s.RoomMgr.AddLeg(roomID, req.LegID)
	}

	// If the leg is already in a room, move it instead of adding.
	if fromRoomID, inRoom := s.RoomMgr.FindLegRoom(req.LegID); inRoom {
		if fromRoomID == roomID {
			return nil, newAPIError(http.StatusBadRequest, "leg already in this room")
		}
		if err := s.roomScopedLegRemoval(fromRoomID, req.LegID, func() error {
			return s.RoomMgr.MoveLeg(fromRoomID, roomID, req.LegID)
		}); err != nil {
			return nil, newAPIError(http.StatusBadRequest, "%s", err.Error())
		}
		if req.Role != nil {
			if err := s.RoomMgr.SetLegRole(req.LegID, *req.Role); err != nil {
				return nil, newAPIError(http.StatusBadRequest, "%s", err.Error())
			}
		}
		s.onLegJoinedRoom(roomID, req.LegID)
		return map[string]string{
			"status": "moved",
			"from":   fromRoomID,
			"to":     roomID,
		}, nil
	}

	// Auto-answer ringing inbound SIP legs before adding to the room. Since
	// the answer must complete before AddLeg accepts the leg, do the wait
	// on a goroutine so the HTTP handler returns immediately. Failures
	// surface as leg.command_failed.
	if sipLeg, ok := l.(*leg.SIPLeg); ok && l.State() == leg.StateRinging && l.Type() == leg.TypeSIPInbound {
		sipLeg.SignalAnswer(codec.CodecUnknown)
		go func() {
			waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := sipLeg.WaitConnected(waitCtx); err != nil {
				s.publishCommandFailed(sipLeg, "add_to_room", fmt.Errorf("auto-answer failed: %w", err))
				return
			}
			if err := addLeg(roomID); err != nil {
				s.publishCommandFailed(sipLeg, "add_to_room", err)
				return
			}
			s.onLegJoinedRoom(roomID, req.LegID)
		}()
		return map[string]string{"status": "adding"}, nil
	}

	if err := addLeg(roomID); err != nil {
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

// roomScopedLegRemoval runs the cleanup that hangs off the room a leg is
// leaving, around the removal itself. Every path that takes a leg out of a
// room must go through here: the per-participant recording channel has to be
// closed while the leg is still in the room, and the room's agent and
// recording can only be judged empty once it is gone.
//
// It exists because those three calls used to be written out by hand at each
// call site, and the sites had drifted — two of them omitted the room
// recording, so moving or removing a room's last leg left the recording
// running on an empty room and never published recording.finished. Going
// through one function makes an omission a compile error rather than
// something each new path has to remember.
//
// remove may be nil when the caller has already removed the leg. It returns
// remove's error unchanged so callers keep their own status mapping.
func (s *Server) roomScopedLegRemoval(roomID, legID string, remove func() error) error {
	s.onLegLeavingRoomRecording(roomID, legID)
	if remove != nil {
		if err := remove(); err != nil {
			return err
		}
	}
	s.stopRoomAgentIfEmpty(roomID)
	s.stopRoomRecordingIfEmpty(roomID)
	return nil
}

func (s *Server) doRemoveLegFromRoom(roomID, legID string) error {
	if err := s.roomScopedLegRemoval(roomID, legID, func() error {
		return s.RoomMgr.RemoveLeg(roomID, legID)
	}); err != nil {
		return newAPIError(http.StatusBadRequest, "%s", err.Error())
	}
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
