package api

import (
	"context"
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/room"
	"github.com/go-chi/chi/v5"
)

// resolveStreamLeg returns the SIP leg addressed by id. Only SIP legs carry
// several negotiated audio streams.
func (s *Server) resolveStreamLeg(id string) (*leg.SIPLeg, error) {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		return nil, newAPIError(http.StatusBadRequest, "only SIP legs carry multiple audio streams")
	}
	return sl, nil
}

// streamAPIError maps a leg-layer stream failure onto an HTTP status.
func streamAPIError(err error) error {
	if err == nil {
		return nil
	}
	if err == leg.ErrStreamNotFound {
		return newAPIError(http.StatusNotFound, "stream not found")
	}
	type statuser interface{ Status() int }
	if se, ok := err.(statuser); ok {
		return newAPIError(se.Status(), "%s", err.Error())
	}
	return newAPIError(http.StatusInternalServerError, "%s", err.Error())
}

func toLegStreamView(info leg.StreamInfo) LegStreamView {
	return LegStreamView{
		ID:               info.ID,
		MID:              info.MID,
		Index:            info.Index,
		Primary:          info.Primary,
		State:            info.State,
		Direction:        info.Direction,
		DesiredDirection: info.Desired,
		Codec:            info.Codec,
		SampleRate:       info.SampleRate,
		LocalPort:        info.LocalPort,
		RemoteAddr:       info.RemoteAddr,
		Label:            info.Label,
		Content:          info.Content,
		Lang:             info.Lang,
		RoomID:           info.RoomID,
		Role:             info.Role,
	}
}

func (s *Server) doListLegStreams(legID string) ([]LegStreamView, error) {
	sl, err := s.resolveStreamLeg(legID)
	if err != nil {
		return nil, err
	}
	infos := sl.Streams()
	out := make([]LegStreamView, len(infos))
	for i, info := range infos {
		out[i] = toLegStreamView(info)
	}
	return out, nil
}

func (s *Server) doGetLegStream(legID, streamID string) (LegStreamView, error) {
	sl, err := s.resolveStreamLeg(legID)
	if err != nil {
		return LegStreamView{}, err
	}
	info, ok := sl.Stream(streamID)
	if !ok {
		return LegStreamView{}, newAPIError(http.StatusNotFound, "stream not found")
	}
	return toLegStreamView(info), nil
}

func (s *Server) doUpdateLegStream(legID, streamID string, req UpdateLegStreamRequest) (LegStreamView, error) {
	sl, err := s.resolveStreamLeg(legID)
	if err != nil {
		return LegStreamView{}, err
	}
	info, ok := sl.Stream(streamID)
	if !ok {
		return LegStreamView{}, newAPIError(http.StatusNotFound, "stream not found")
	}
	if info.Primary && !sl.StreamsIndependent() {
		return LegStreamView{}, newAPIError(http.StatusBadRequest,
			"the primary stream's role follows the leg; use PATCH /v1/legs/{id}/role")
	}

	if req.Role != nil {
		if err := s.RoomMgr.SetLegStreamRole(legID, streamID, *req.Role); err != nil {
			return LegStreamView{}, newAPIError(http.StatusNotFound, "%s", err.Error())
		}
	}
	return s.doGetLegStream(legID, streamID)
}

func (s *Server) doAddLegStream(ctx context.Context, legID string, req AddLegStreamRequest) (LegStreamView, error) {
	sl, err := s.resolveStreamLeg(legID)
	if err != nil {
		return LegStreamView{}, err
	}

	info, err := sl.AddStream(ctx, leg.StreamRequest{
		Direction: req.Direction,
		Lang:      req.Lang,
		Content:   req.Content,
		Label:     req.Label,
	})
	if err != nil {
		s.Bus.Publish(events.LegStreamRejected, &events.LegStreamData{
			LegScope: events.LegScope{LegID: legID, AppID: sl.AppID()},
			Reason:   err.Error(),
		})
		return LegStreamView{}, streamAPIError(err)
	}

	view := toLegStreamView(info)
	s.Bus.Publish(events.LegStreamAdded, &events.LegStreamData{
		LegScope:  events.LegScope{LegID: legID, AppID: sl.AppID()},
		StreamID:  info.ID,
		MID:       info.MID,
		Direction: info.Direction,
		Lang:      info.Lang,
	})

	if req.RoomID != "" {
		if err := s.attachStreamRoom(sl, info.ID, req.RoomID, req.Role); err != nil {
			return view, err
		}
		view, _ = s.doGetLegStream(legID, info.ID)
	}
	return view, nil
}

func (s *Server) doRemoveLegStream(ctx context.Context, legID, streamID string) error {
	sl, err := s.resolveStreamLeg(legID)
	if err != nil {
		return err
	}
	info, ok := sl.Stream(streamID)
	if !ok {
		return newAPIError(http.StatusNotFound, "stream not found")
	}

	// Detach from its room first so the mixer stops pulling from a stream whose
	// socket is about to close.
	s.detachStreamRoom(sl, streamID)

	if err := sl.RemoveStream(ctx, streamID); err != nil {
		return streamAPIError(err)
	}
	s.Bus.Publish(events.LegStreamRemoved, &events.LegStreamData{
		LegScope: events.LegScope{LegID: legID, AppID: sl.AppID()},
		StreamID: streamID,
		MID:      info.MID,
	})
	return nil
}

func (s *Server) doAttachLegStreamRoom(legID, streamID string, req AttachStreamRoomRequest) (LegStreamView, error) {
	sl, err := s.resolveStreamLeg(legID)
	if err != nil {
		return LegStreamView{}, err
	}
	if req.RoomID == "" {
		return LegStreamView{}, newAPIError(http.StatusBadRequest, "room_id is required")
	}
	if err := s.attachStreamRoom(sl, streamID, req.RoomID, req.Role); err != nil {
		return LegStreamView{}, err
	}
	return s.doGetLegStream(legID, streamID)
}

func (s *Server) doDetachLegStreamRoom(legID, streamID string) (LegStreamView, error) {
	sl, err := s.resolveStreamLeg(legID)
	if err != nil {
		return LegStreamView{}, err
	}
	if _, ok := sl.Stream(streamID); !ok {
		return LegStreamView{}, newAPIError(http.StatusNotFound, "stream not found")
	}
	s.detachStreamRoom(sl, streamID)
	return s.doGetLegStream(legID, streamID)
}

// validateRoomStreams rejects a room-add request naming streams that cannot be
// placed, before the request mutates anything.
func (s *Server) validateRoomStreams(l leg.Leg, streams []AddRoomStream) error {
	if len(streams) == 0 {
		return nil
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		return newAPIError(http.StatusBadRequest, "only SIP legs carry multiple audio streams")
	}
	for i, req := range streams {
		if req.StreamID == "" {
			return newAPIError(http.StatusBadRequest, "streams[%d]: stream_id is required", i)
		}
		info, ok := sl.Stream(req.StreamID)
		if !ok {
			return newAPIError(http.StatusNotFound, "streams[%d]: stream %q not found", i, req.StreamID)
		}
		if info.Primary {
			return newAPIError(http.StatusBadRequest,
				"streams[%d]: the primary stream joins with the leg itself and cannot be listed here", i)
		}
	}
	return nil
}

// attachRequestedRoomStreams mixes the named streams of l into roomID. Called
// after the leg itself has joined, so a stream and its leg land in the same
// room in one request when that is what was asked for.
func (s *Server) attachRequestedRoomStreams(l leg.Leg, roomID string, streams []AddRoomStream) {
	if len(streams) == 0 {
		return
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		return
	}
	for _, req := range streams {
		if err := s.attachStreamRoom(sl, req.StreamID, roomID, req.Role); err != nil {
			s.Log.Warn("attach leg stream to room failed",
				"leg_id", l.ID(), "stream_id", req.StreamID, "room_id", roomID, "error", err)
		}
	}
}

// attachAnsweredStreamRooms places the caller's additional audio streams into
// the rooms an answer request asked for. Entries are positional over the
// streams we actually accepted — the caller's offer decides how many exist, so
// an entry with no matching stream is logged and skipped.
func (s *Server) attachAnsweredStreamRooms(l leg.Leg, streams []AnswerLegStream) {
	if len(streams) == 0 {
		return
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		return
	}
	secondary := sl.SecondaryStreamIDs()
	for i, req := range streams {
		if req.RoomID == "" {
			continue
		}
		if i >= len(secondary) {
			s.Log.Info("caller offered fewer audio streams than the answer requested rooms for",
				"leg_id", l.ID(), "stream_index", i+1, "room_id", req.RoomID)
			continue
		}
		if err := s.attachStreamRoom(sl, secondary[i], req.RoomID, req.Role); err != nil {
			s.Log.Warn("auto-attach answered leg stream to room failed",
				"leg_id", l.ID(), "stream_id", secondary[i], "room_id", req.RoomID, "error", err)
		}
	}
}

// attachOfferedStreamRooms places the extra streams offered in a create-leg
// request into their rooms once the call is up. Entries are matched to streams
// positionally: request entry i is m-line i+1, since the primary occupies 0.
//
// A stream the peer refused simply is not there, which is not an error — the
// call runs on whatever was negotiated.
func (s *Server) attachOfferedStreamRooms(l leg.Leg, reqs []CreateLegStream) {
	if len(reqs) == 0 {
		return
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		return
	}
	secondary := sl.SecondaryStreamIDs()
	for i, req := range reqs {
		if req.RoomID == "" {
			continue
		}
		if i >= len(secondary) {
			s.Log.Info("offered audio stream was not negotiated; nothing to attach",
				"leg_id", l.ID(), "stream_index", i+1, "room_id", req.RoomID)
			continue
		}
		if err := s.attachStreamRoom(sl, secondary[i], req.RoomID, req.Role); err != nil {
			s.Log.Warn("auto-attach leg stream to room failed",
				"leg_id", l.ID(), "stream_id", secondary[i], "room_id", req.RoomID, "error", err)
		}
	}
}

// attachStreamRoom moves a stream into a room, leaving whichever room it was in.
func (s *Server) attachStreamRoom(sl *leg.SIPLeg, streamID, roomID, role string) error {
	info, ok := sl.Stream(streamID)
	if !ok {
		return newAPIError(http.StatusNotFound, "stream not found")
	}
	if info.Primary && !sl.StreamsIndependent() {
		return newAPIError(http.StatusBadRequest, "the primary stream follows the leg's own room; use /v1/rooms/{id}/legs")
	}
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return newAPIError(http.StatusNotFound, "room not found")
	}

	s.detachStreamRoom(sl, streamID)

	if _, ok := rm.AddLegStream(sl, streamID, role); !ok {
		return newAPIError(http.StatusConflict, "stream carries no audio in either direction")
	}
	s.onStreamJoinedRoomRecording(roomID, room.StreamParticipantID(sl.ID(), streamID))
	s.Bus.Publish(events.LegStreamRoomChanged, &events.LegStreamData{
		LegScope: events.LegScope{LegID: sl.ID(), AppID: sl.AppID()},
		StreamID: streamID,
		MID:      info.MID,
		RoomID:   roomID,
		Role:     role,
	})
	return nil
}

// detachStreamRoom removes a stream from whichever room currently mixes it.
func (s *Server) detachStreamRoom(sl *leg.SIPLeg, streamID string) {
	roomID, ok := sl.StreamRooms()[streamID]
	if !ok || roomID == "" {
		return
	}
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}
	participantID := room.StreamParticipantID(sl.ID(), streamID)
	s.onStreamLeavingRoomRecording(roomID, participantID)
	rm.RemoveLegStream(participantID)
	s.Bus.Publish(events.LegStreamRoomChanged, &events.LegStreamData{
		LegScope: events.LegScope{LegID: sl.ID(), AppID: sl.AppID()},
		StreamID: streamID,
	})
	// A room whose last stream just left has nothing more to record.
	s.stopRoomRecordingIfEmpty(roomID)
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (s *Server) listLegStreams(w http.ResponseWriter, r *http.Request) {
	views, err := s.doListLegStreams(chi.URLParam(r, "id"))
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getLegStream(w http.ResponseWriter, r *http.Request) {
	view, err := s.doGetLegStream(chi.URLParam(r, "id"), chi.URLParam(r, "streamId"))
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) updateLegStream(w http.ResponseWriter, r *http.Request) {
	var req UpdateLegStreamRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doUpdateLegStream(chi.URLParam(r, "id"), chi.URLParam(r, "streamId"), req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) addLegStream(w http.ResponseWriter, r *http.Request) {
	var req AddLegStreamRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doAddLegStream(r.Context(), chi.URLParam(r, "id"), req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) removeLegStream(w http.ResponseWriter, r *http.Request) {
	if err := s.doRemoveLegStream(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "streamId")); err != nil {
		handleAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) attachLegStreamRoom(w http.ResponseWriter, r *http.Request) {
	var req AttachStreamRoomRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doAttachLegStreamRoom(chi.URLParam(r, "id"), chi.URLParam(r, "streamId"), req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) detachLegStreamRoom(w http.ResponseWriter, r *http.Request) {
	view, err := s.doDetachLegStreamRoom(chi.URLParam(r, "id"), chi.URLParam(r, "streamId"))
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
