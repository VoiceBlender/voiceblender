package api

import (
	"context"
	"encoding/json"

	"github.com/pion/webrtc/v4"
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

// earlyMediaPayload carries codec selection for leg_early_media.
type earlyMediaPayload struct {
	ID    string `json:"id"`
	Codec string `json:"codec,omitempty"`
}

// playbackTargetPayload identifies a single playback on a leg or room.
type playbackTargetPayload struct {
	ID         string `json:"id"`
	PlaybackID string `json:"playback_id"`
}

// playbackVolumePayload sets the volume on a playback.
type playbackVolumePayload struct {
	ID         string `json:"id"`
	PlaybackID string `json:"playback_id"`
	Volume     int    `json:"volume"`
}

// agentMessagePayload injects a message into a leg or room agent session.
type agentMessagePayload struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// legAMDStartPayload combines leg id with optional AMD threshold overrides.
type legAMDStartPayload struct {
	ID string `json:"id"`
	AMDParams
}

// playbackStartPayload combines a leg/room id with the playback request.
type playbackStartPayload struct {
	ID string `json:"id"`
	PlaybackRequest
}

// ttsStartPayload combines a leg/room id with the TTS request.
type ttsStartPayload struct {
	ID string `json:"id"`
	TTSRequest
}

// sttStartPayload combines a leg/room id with the STT request.
type sttStartPayload struct {
	ID string `json:"id"`
	STTRequest
}

// answerLegPayload carries the inputs for answer_leg.
type answerLegPayload struct {
	ID              string `json:"id"`
	SpeechDetection *bool  `json:"speech_detection,omitempty"`
	Codec           string `json:"codec,omitempty"`
}

// deleteLegPayload carries the inputs for delete_leg.
type deleteLegPayload struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// transferLegPayload combines a leg id with the transfer request.
type transferLegPayload struct {
	ID string `json:"id"`
	TransferRequest
}

// recordStartPayload combines a leg/room id with the record request.
type recordStartPayload struct {
	ID string `json:"id"`
	RecordRequest
}

// agentElevenLabsPayload combines a leg/room id with the ElevenLabs agent request.
type agentElevenLabsPayload struct {
	ID string `json:"id"`
	ElevenLabsAgentRequest
}

// agentVAPIPayload combines a leg/room id with the VAPI agent request.
type agentVAPIPayload struct {
	ID string `json:"id"`
	VAPIAgentRequest
}

// agentPipecatPayload combines a leg/room id with the Pipecat agent request.
type agentPipecatPayload struct {
	ID string `json:"id"`
	PipecatAgentRequest
}

// agentDeepgramPayload combines a leg/room id with the Deepgram agent request.
type agentDeepgramPayload struct {
	ID string `json:"id"`
	DeepgramAgentRequest
}

// rttPayload carries text for send_leg_rtt.
type rttPayload struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// vsiWebRTCAddCandidatePayload combines a leg id with an ICE candidate for
// webrtc_add_candidate.
type vsiWebRTCAddCandidatePayload struct {
	ID        string                  `json:"id"`
	Candidate webrtc.ICECandidateInit `json:"candidate"`
}

// addLegPayload combines room_id with AddLegRequest fields.
type addLegPayload struct {
	RoomID     string  `json:"room_id"`
	LegID      string  `json:"leg_id"`
	Mute       *bool   `json:"mute,omitempty"`
	Deaf       *bool   `json:"deaf,omitempty"`
	AcceptDTMF *bool   `json:"accept_dtmf,omitempty"`
	Role       *string `json:"role,omitempty"`
}

// Bridge payloads. room_id is the path-equivalent room; direction is
// relative to it (bidirectional|send|receive|none).
type bridgeCreatePayload struct {
	RoomID     string `json:"room_id"`
	BridgeID   string `json:"bridge_id,omitempty"`
	PeerRoomID string `json:"peer_room_id"`
	Direction  string `json:"direction,omitempty"`
}

type bridgeListPayload struct {
	RoomID string `json:"room_id"`
}

type bridgeRefPayload struct {
	RoomID   string `json:"room_id"`
	BridgeID string `json:"bridge_id"`
}

type bridgeUpdatePayload struct {
	RoomID    string `json:"room_id"`
	BridgeID  string `json:"bridge_id"`
	Direction string `json:"direction"`
}

// roomRoutingSetPayload combines room_id with the routing matrix for
// the room_routing_set VSI command.
type roomRoutingSetPayload struct {
	RoomID string              `json:"room_id"`
	Matrix map[string][]string `json:"matrix"`
}

// roomRoutingUpdatePayload combines room_id with selected row replacements
// for the room_routing_update VSI command.
type roomRoutingUpdatePayload struct {
	RoomID  string             `json:"room_id"`
	Updates []RoutingRowUpdate `json:"updates"`
}

// setLegRolePayload is the inbound payload for set_leg_role.
type setLegRolePayload struct {
	LegID string `json:"leg_id"`
	Role  string `json:"role"`
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
		var p answerLegPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doAnswerLeg(p.ID, p.SpeechDetection, p.Codec); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "answering"})

	case "delete_leg":
		var p deleteLegPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doDeleteLeg(p.ID, p.Reason); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "hanging_up"})

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
		sipLeg, err := s.resolveHoldLeg(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		go func() {
			if err := sipLeg.Hold(context.Background()); err != nil {
				s.publishCommandFailed(sipLeg, "hold", err)
			}
		}()
		s.wsCommandResult(lw, msg, map[string]string{"status": "holding"})
	case "unhold_leg":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		sipLeg, err := s.resolveHoldLeg(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		go func() {
			if err := sipLeg.Unhold(context.Background()); err != nil {
				s.publishCommandFailed(sipLeg, "unhold", err)
			}
		}()
		s.wsCommandResult(lw, msg, map[string]string{"status": "unholding"})

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

	// ── RTT (Real-Time Text, T.140) ─────────────────────────────────
	case "send_leg_rtt":
		var p rttPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doSendLegRTT(context.Background(), rttSourceVSI, p.ID, p.Text); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "sent"})
	case "accept_leg_rtt":
		s.wsSimpleLegCommand(lw, msg, s.doAcceptLegRTT, "rtt_accepting")
	case "reject_leg_rtt":
		s.wsSimpleLegCommand(lw, msg, s.doRejectLegRTT, "rtt_rejecting")

	// ── WebRTC ──────────────────────────────────────────────────────
	case "webrtc_offer":
		var req WebRTCOfferRequest
		if !s.wsParsePayload(lw, msg, &req) {
			return
		}
		result, err := s.doWebRTCOffer(req)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, result)

	case "webrtc_add_candidate":
		var p vsiWebRTCAddCandidatePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doWebRTCAddCandidate(p.ID, p.Candidate); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "added"})

	case "webrtc_get_candidates":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		result, err := s.doWebRTCGetCandidates(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, result)

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
			Role:       p.Role,
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

	// ── Bridges ─────────────────────────────────────────────────────
	case "bridge_create":
		var p bridgeCreatePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		view, err := s.doCreateRoomBridge(p.RoomID, CreateRoomBridgeRequest{
			ID:        p.BridgeID,
			RoomID:    p.PeerRoomID,
			Direction: p.Direction,
		})
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, view)

	case "bridge_list":
		var p bridgeListPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if _, ok := s.RoomMgr.Get(p.RoomID); !ok {
			s.wsCommandError(lw, msg, newAPIError(404, "room not found"))
			return
		}
		brs := s.RoomMgr.ListBridgesForRoom(p.RoomID)
		views := make([]BridgeView, len(brs))
		for i, br := range brs {
			views[i] = s.bridgeView(p.RoomID, br)
		}
		s.wsCommandResult(lw, msg, views)

	case "bridge_get":
		var p bridgeRefPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		br, ok := s.bridgeForRoom(p.RoomID, p.BridgeID)
		if !ok {
			s.wsCommandError(lw, msg, newAPIError(404, "bridge not found"))
			return
		}
		s.wsCommandResult(lw, msg, s.bridgeView(p.RoomID, br))

	case "bridge_update":
		var p bridgeUpdatePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		view, err := s.doUpdateRoomBridge(p.RoomID, p.BridgeID, UpdateRoomBridgeRequest{Direction: p.Direction})
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, view)

	case "bridge_delete":
		var p bridgeRefPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doDeleteRoomBridge(p.RoomID, p.BridgeID); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "deleted"})

	// ── Routing matrix ──────────────────────────────────────────────
	case "room_routing_get":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		view, err := s.doGetRoomRouting(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, view)

	case "room_routing_set":
		var p roomRoutingSetPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		view, err := s.doSetRoomRouting(p.RoomID, RoomRoutingRequest{Matrix: p.Matrix})
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, view)

	case "room_routing_update":
		var p roomRoutingUpdatePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		view, err := s.doUpdateRoomRouting(p.RoomID, RoomRoutingUpdateRequest{Updates: p.Updates})
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, view)

	case "set_leg_role":
		var p setLegRolePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		view, err := s.doSetLegRole(p.LegID, SetLegRoleRequest{Role: p.Role})
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, view)

	// ── Leg control gaps ────────────────────────────────────────────
	case "leg_ring":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doRingLeg(p.ID); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "ringing"})
	case "leg_early_media":
		var p earlyMediaPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doEarlyMediaLeg(p.ID, p.Codec); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "early_media"})
	case "leg_amd_start":
		var p legAMDStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		amd := p.AMDParams
		if err := s.doStartAMDLeg(p.ID, &amd); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "started"})

	// ── Recording (resource-first naming for new commands) ──────────
	case "leg_record_start":
		var p recordStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStartRecordLeg(p.ID, p.RecordRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_record_start":
		var p recordStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStartRecordRoom(p.ID, p.RecordRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "leg_record_pause":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doPauseRecordLeg(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "leg_record_resume":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doResumeRecordLeg(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "leg_record_stop":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStopRecordLeg(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_record_stop":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStopRecordRoom(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_record_pause":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doPauseRecordRoom(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_record_resume":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doResumeRecordRoom(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)

	// ── Playback start/stop ─────────────────────────────────────────
	case "leg_play_start":
		var p playbackStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStartLegPlay(p.ID, p.PlaybackRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "leg_play_stop":
		var p playbackTargetPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStopLegPlay(p.ID, p.PlaybackID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_play_start":
		var p playbackStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStartRoomPlay(p.ID, p.PlaybackRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_play_stop":
		var p playbackTargetPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStopRoomPlay(p.ID, p.PlaybackID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)

	// ── TTS ─────────────────────────────────────────────────────────
	case "leg_tts":
		var p ttsStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doLegTTS(p.ID, p.TTSRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_tts":
		var p ttsStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doRoomTTS(p.ID, p.TTSRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)

	// ── Transfer ────────────────────────────────────────────────────
	case "leg_transfer":
		var p transferLegPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doTransferLeg(p.ID, p.TransferRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)

	// ── Playback volume ─────────────────────────────────────────────
	case "leg_play_volume":
		var p playbackVolumePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doVolumeLegPlay(p.ID, p.PlaybackID, p.Volume); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "ok"})
	case "room_play_volume":
		var p playbackVolumePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		if err := s.doVolumeRoomPlay(p.ID, p.PlaybackID, p.Volume); err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, map[string]string{"status": "ok"})

	// ── STT start ───────────────────────────────────────────────────
	case "leg_stt_start":
		var p sttStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStartSTTLeg(p.ID, p.STTRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_stt_start":
		var p sttStartPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStartSTTRoom(p.ID, p.STTRequest)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)

	// ── STT stop ────────────────────────────────────────────────────
	case "leg_stt_stop":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStopSTTLeg(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_stt_stop":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStopSTTRoom(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)

	// ── Agent start (per-provider) ──────────────────────────────────
	case "leg_agent_elevenlabs":
		var p agentElevenLabsPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		s.vsiStartLegAgentElevenLabs(lw, msg, p)
	case "leg_agent_vapi":
		var p agentVAPIPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		s.vsiStartLegAgentVAPI(lw, msg, p)
	case "leg_agent_pipecat":
		var p agentPipecatPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		s.vsiStartLegAgentPipecat(lw, msg, p)
	case "leg_agent_deepgram":
		var p agentDeepgramPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		s.vsiStartLegAgentDeepgram(lw, msg, p)
	case "room_agent_elevenlabs":
		var p agentElevenLabsPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		s.vsiStartRoomAgentElevenLabs(lw, msg, p)
	case "room_agent_vapi":
		var p agentVAPIPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		s.vsiStartRoomAgentVAPI(lw, msg, p)
	case "room_agent_pipecat":
		var p agentPipecatPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		s.vsiStartRoomAgentPipecat(lw, msg, p)
	case "room_agent_deepgram":
		var p agentDeepgramPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		s.vsiStartRoomAgentDeepgram(lw, msg, p)

	// ── Agent message ───────────────────────────────────────────────
	case "leg_agent_message":
		var p agentMessagePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doLegAgentMessage(context.Background(), p.ID, p.Message)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_agent_message":
		var p agentMessagePayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doRoomAgentMessage(context.Background(), p.ID, p.Message)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)

	// ── Agent stop ──────────────────────────────────────────────────
	case "leg_agent_stop":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStopAgentLeg(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)
	case "room_agent_stop":
		var p idPayload
		if !s.wsParsePayload(lw, msg, &p) {
			return
		}
		res, err := s.doStopAgentRoom(p.ID)
		if err != nil {
			s.wsCommandError(lw, msg, err)
			return
		}
		s.wsCommandResult(lw, msg, res)

	// ── SIP Registrations ───────────────────────────────────────────
	case "list_sip_registrations":
		reg := s.SIPEngine.Registrar()
		views := []RegistrationView{}
		if reg != nil {
			for _, b := range reg.List() {
				views = append(views, toRegistrationView(b))
			}
		}
		s.wsCommandResult(lw, msg, RegistrationsResponse{Bindings: views})

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
