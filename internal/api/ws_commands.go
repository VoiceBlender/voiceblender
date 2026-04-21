package api

import (
	"context"
	"encoding/json"
)

// idPayload is the common payload shape for commands targeting a single resource.
type idPayload struct {
	ID string `json:"id"`
}

// roomLegPayload targets a leg within a room.
type roomLegPayload struct {
	RoomID string `json:"room_id"`
	LegID  string `json:"leg_id"`
}

// dtmfPayload carries digits for send_leg_dtmf.
type dtmfPayload struct {
	ID     string `json:"id"`
	Digits string `json:"digits"`
}

// addLegPayload combines room_id with AddLegRequest fields.
type addLegPayload struct {
	RoomID     string `json:"room_id"`
	LegID      string `json:"leg_id"`
	Mute       *bool  `json:"mute,omitempty"`
	Deaf       *bool  `json:"deaf,omitempty"`
	AcceptDTMF *bool  `json:"accept_dtmf,omitempty"`
}

func (s *Server) wsHandleCommand(lw *wsLockedWriter, msg vsiInMsg) {
	switch msg.Type {

	// ── Leg queries ─────────────────────────────────────────────────
	case "list_legs":
		legs := s.LegMgr.List()
		views := make([]LegView, len(legs))
		for i, l := range legs {
			views[i] = toLegView(l)
		}
		s.wsCommandResult(lw, msg, views)

	case "get_leg":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		l, ok := s.LegMgr.Get(p.ID)
		if !ok {
			s.wsCommandError(lw, msg, newAPIError(404, "leg not found"))
			return
		}
		s.wsCommandResult(lw, msg, toLegView(l))

	// ── Leg lifecycle ───────────────────────────────────────────────
	case "create_leg":
		var req CreateLegRequest
		if !s.wsParsePayload(lw, msg, &req) {
			return
		}
		s.wsCreateLeg(lw, msg, req)

	case "answer_leg":
		var p struct {
			ID              string `json:"id"`
			SpeechDetection *bool  `json:"speech_detection,omitempty"`
		}
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doAnswerLeg(p.ID, p.SpeechDetection); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "answering"})

	case "delete_leg":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doDeleteLeg(p.ID); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "deleted"})

	// ── Leg state toggles ───────────────────────────────────────────
	case "mute_leg":
		s.wsSimpleLegCommand(lw, msg, s.doMuteLeg, "muted")
	case "unmute_leg":
		s.wsSimpleLegCommand(lw, msg, s.doUnmuteLeg, "unmuted")
	case "deaf_leg":
		s.wsSimpleLegCommand(lw, msg, s.doDeafLeg, "deaf")
	case "undeaf_leg":
		s.wsSimpleLegCommand(lw, msg, s.doUndeafLeg, "undeaf")
	case "hold_leg":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doHoldLeg(context.Background(), p.ID); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "held"})
	case "unhold_leg":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doUnholdLeg(context.Background(), p.ID); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "resumed"})

	// ── DTMF ────────────────────────────────────────────────────────
	case "send_leg_dtmf":
		var p dtmfPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doSendLegDTMF(context.Background(), p.ID, p.Digits); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "sent"})
	case "accept_leg_dtmf":
		s.wsSimpleLegCommand(lw, msg, s.doAcceptLegDTMF, "dtmf_accepting")
	case "reject_leg_dtmf":
		s.wsSimpleLegCommand(lw, msg, s.doRejectLegDTMF, "dtmf_rejecting")

	// ── Room queries ────────────────────────────────────────────────
	case "list_rooms":
		rooms := s.RoomMgr.List()
		views := make([]RoomView, len(rooms))
		for i, rm := range rooms {
			parts := rm.Participants()
			pViews := make([]LegView, len(parts))
			for j, p := range parts {
				pViews[j] = toLegView(p)
			}
			views[i] = RoomView{ID: rm.ID, AppID: rm.AppID, Participants: pViews}
		}
		s.wsCommandResult(lw, msg, views)

	case "get_room":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		rm, ok := s.RoomMgr.Get(p.ID)
		if !ok {
			s.wsCommandError(lw, msg, newAPIError(404, "room not found"))
			return
		}
		parts := rm.Participants()
		pViews := make([]LegView, len(parts))
		for j, p := range parts {
			pViews[j] = toLegView(p)
		}
		s.wsCommandResult(lw, msg, RoomView{ID: rm.ID, AppID: rm.AppID, Participants: pViews})

	// ── Room lifecycle ──────────────────────────────────────────────
	case "create_room":
		var req CreateRoomRequest
		if !s.wsParsePayload(lw, msg, &req) {
			return
		}
		view, err := s.doCreateRoom(req)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, view)

	case "delete_room":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doDeleteRoom(p.ID); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "deleted"})

	case "add_leg_to_room":
		var p addLegPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		result, err := s.doAddLegToRoom(context.Background(), p.RoomID, AddLegRequest{
			LegID:      p.LegID,
			Mute:       p.Mute,
			Deaf:       p.Deaf,
			AcceptDTMF: p.AcceptDTMF,
		})
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, result)

	case "remove_leg_from_room":
		var p roomLegPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doRemoveLegFromRoom(p.RoomID, p.LegID); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "removed"})

	default:
		s.vsiSendResponse(lw, msg.RequestID, "error",
			map[string]interface{}{"code": 400, "message": "unknown command: " + msg.Type})
	}
}

// wsSimpleLegCommand handles the common pattern: parse {id}, call doFn(id), respond.
func (s *Server) wsSimpleLegCommand(lw *wsLockedWriter, msg vsiInMsg, doFn func(string) error, status string) {
	var p idPayload
	if !s.wsParsePayload(lw, msg, &p) {
		return
	}
	if err := doFn(p.ID); err != nil {
		s.wsCommandError(lw, msg, err)
		return
	}
	s.wsCommandResult(lw, msg, map[string]string{"status": status})
}

// wsParsePayload unmarshals msg.Payload into dst. Returns false and sends an
// error response if parsing fails.
func (s *Server) wsParsePayload(lw *wsLockedWriter, msg vsiInMsg, dst interface{}) bool {
	if len(msg.Payload) == 0 {
		return true
	}
	if err := json.Unmarshal(msg.Payload, dst); err != nil {
		s.wsCommandError(lw, msg, newAPIError(400, "invalid payload: %v", err))
		return false
	}
	return true
}

func (s *Server) wsCommandResult(lw *wsLockedWriter, msg vsiInMsg, data interface{}) {
	s.vsiSendResponse(lw, msg.RequestID, msg.Type+".result", data)
}

func (s *Server) wsCommandError(lw *wsLockedWriter, msg vsiInMsg, err error) {
	code := 500
	if ae, ok := err.(*apiError); ok {
		code = ae.Code
	}
	s.vsiSendResponse(lw, msg.RequestID, "error", map[string]interface{}{
		"code":    code,
		"message": err.Error(),
	})
}

// wsCreateLeg is a placeholder for create_leg over WS. It calls the HTTP-based
// createSIPOutboundLeg flow synchronously. Full do* extraction of the complex
// originate flow is deferred to Phase 2.
func (s *Server) wsCreateLeg(lw *wsLockedWriter, msg vsiInMsg, req CreateLegRequest) {
	switch req.Type {
	case "sip":
		// For now, return an error directing clients to use the REST API for
		// leg creation until the complex originate flow is extracted into a
		// do* method.
		s.wsCommandError(lw, msg, newAPIError(501, "create_leg over WS not yet implemented; use POST /v1/legs"))
	default:
		s.wsCommandError(lw, msg, newAPIError(400, "unsupported leg type: %s", req.Type))
	}
}
