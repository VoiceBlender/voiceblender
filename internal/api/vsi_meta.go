package api

import (
	"reflect"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// VSICommandMeta describes a single command accepted on the /v1/vsi
// WebSocket. asyncapi-gen consumes this list — every command must be
// registered here or it will be missing from asyncapi.yaml.
//
// Inbound:  {"type": Name, "request_id": "...", "payload": Payload}
// Result:   {"type": Name + ".result", "request_id": "...", "data": Result}
// Error:    {"type": "error", "request_id": "...", "data": {"code": int, "message": string}}
type VSICommandMeta struct {
	Name        string
	Summary     string
	Description string
	// PayloadType is a zero-value Go instance of the inbound payload struct
	// (or nil when the command takes no payload).
	PayloadType interface{}
	// ResultType is a zero-value Go instance of the success-response data
	// struct (or nil when the response carries no data beyond `type`/`request_id`).
	ResultType interface{}
	// ErrorCodes lists the HTTP-style status codes the command can surface in
	// an "error" frame. Documentation only; not enforced.
	ErrorCodes []int
}

// vsiStatusResponse is the trivial {"status": "..."} shape returned by most
// commands when there's nothing else to say. The actual status string varies
// per command (see ResultStatus on each VSICommandMeta entry).
type vsiStatusResponse struct {
	Status string `json:"status"`
}

// AddLegToRoomResult is the success payload for add_leg_to_room over VSI.
// Mirrors what doAddLegToRoom returns inline today.
type AddLegToRoomResult struct {
	Status string `json:"status"`
	RoomID string `json:"room_id"`
	LegID  string `json:"leg_id"`
}

// VSICommandsMetadata returns the authoritative list of VSI commands.
// IMPORTANT: when adding, removing, or changing the shape of a VSI command,
// update this list AND run `make asyncapi` so asyncapi.yaml stays in sync.
func VSICommandsMetadata() []VSICommandMeta {
	return []VSICommandMeta{
		// ── Leg queries ─────────────────────────────────────────────────
		{
			Name: "list_legs", Summary: "List all active legs",
			ResultType: []LegView{},
		},
		{
			Name: "get_leg", Summary: "Get a single leg by id",
			PayloadType: idPayload{}, ResultType: LegView{},
			ErrorCodes: []int{404},
		},
		// ── Leg lifecycle ───────────────────────────────────────────────
		{
			Name: "create_leg", Summary: "Originate an outbound leg",
			Description: "Currently returns a 501 error directing clients to use POST /v1/legs. " +
				"Reserved for the future when the originate flow is fully extracted into a do* helper.",
			PayloadType: CreateLegRequest{}, ResultType: LegView{},
			ErrorCodes: []int{400, 501},
		},
		{
			Name: "answer_leg", Summary: "Answer a ringing inbound leg",
			PayloadType: answerLegPayload{}, ResultType: vsiStatusResponse{},
			ErrorCodes: []int{400, 404, 409},
		},
		{
			Name: "delete_leg", Summary: "Hang up a leg",
			PayloadType: deleteLegPayload{}, ResultType: vsiStatusResponse{},
			ErrorCodes: []int{400, 404},
		},
		// ── Mute / deaf / hold ──────────────────────────────────────────
		{Name: "mute_leg", Summary: "Mute a leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "unmute_leg", Summary: "Unmute a leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "deaf_leg", Summary: "Deafen a leg (stop receiving room audio)", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "undeaf_leg", Summary: "Undeafen a leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "hold_leg", Summary: "Put a SIP leg on hold", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404, 409}},
		{Name: "unhold_leg", Summary: "Resume a held SIP leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404, 409}},
		// ── DTMF ────────────────────────────────────────────────────────
		{Name: "send_leg_dtmf", Summary: "Send DTMF digits on a leg", PayloadType: dtmfPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 500}},
		{Name: "accept_leg_dtmf", Summary: "Enable DTMF reception", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "reject_leg_dtmf", Summary: "Disable DTMF reception", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		// ── RTT (T.140) ─────────────────────────────────────────────────
		{Name: "send_leg_rtt", Summary: "Send Real-Time Text (T.140) on a SIP leg", PayloadType: rttPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409, 500}},
		{Name: "accept_leg_rtt", Summary: "Enable RTT reception", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "reject_leg_rtt", Summary: "Disable RTT reception", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		// ── WebRTC ──────────────────────────────────────────────────────
		{
			Name: "webrtc_offer", Summary: "Establish a WebRTC leg via SDP offer/answer",
			Description: "Accepts a browser SDP offer, allocates a peer connection, returns the local SDP answer plus the new leg id. " +
				"Subsequent ICE candidates from the browser are delivered via webrtc_add_candidate; server-side candidates are drained via webrtc_get_candidates.",
			PayloadType: WebRTCOfferRequest{}, ResultType: WebRTCOfferResult{},
			ErrorCodes: []int{400, 500},
		},
		{
			Name: "webrtc_add_candidate", Summary: "Add a remote ICE candidate to a WebRTC leg",
			PayloadType: vsiWebRTCAddCandidatePayload{}, ResultType: vsiStatusResponse{},
			ErrorCodes: []int{400, 404, 500},
		},
		{
			Name: "webrtc_get_candidates", Summary: "Drain server-gathered ICE candidates for a WebRTC leg",
			Description: "Returns any local ICE candidates that have been gathered since the last call, plus a `done` flag indicating ICE gathering has completed. Clients should poll until `done` is true.",
			PayloadType: idPayload{}, ResultType: WebRTCCandidatesResult{},
			ErrorCodes: []int{400, 404},
		},
		// ── Rooms ───────────────────────────────────────────────────────
		{Name: "list_rooms", Summary: "List all rooms", ResultType: []RoomView{}},
		{Name: "get_room", Summary: "Get a single room by id", PayloadType: idPayload{}, ResultType: RoomView{}, ErrorCodes: []int{404}},
		{Name: "create_room", Summary: "Create a room", PayloadType: CreateRoomRequest{}, ResultType: RoomView{}, ErrorCodes: []int{400, 409}},
		{Name: "delete_room", Summary: "Delete a room", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "add_leg_to_room", Summary: "Add or move a leg into a room", PayloadType: addLegPayload{}, ResultType: AddLegToRoomResult{}, ErrorCodes: []int{400, 404}},
		{Name: "remove_leg_from_room", Summary: "Remove a leg from a room", PayloadType: roomLegPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},

		// ── Bridges ─────────────────────────────────────────────────────
		{Name: "bridge_create", Summary: "Join two rooms' mixers", PayloadType: bridgeCreatePayload{}, ResultType: BridgeView{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "bridge_list", Summary: "List bridges involving a room", PayloadType: bridgeListPayload{}, ResultType: []BridgeView{}, ErrorCodes: []int{404}},
		{Name: "bridge_get", Summary: "Get a bridge involving a room", PayloadType: bridgeRefPayload{}, ResultType: BridgeView{}, ErrorCodes: []int{404}},
		{Name: "bridge_update", Summary: "Change a bridge's direction", PayloadType: bridgeUpdatePayload{}, ResultType: BridgeView{}, ErrorCodes: []int{400, 404}},
		{Name: "bridge_delete", Summary: "Tear down a bridge", PayloadType: bridgeRefPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},

		// ── Routing matrix ──────────────────────────────────────────────
		{Name: "room_routing_get", Summary: "Get a room's audio routing matrix", PayloadType: idPayload{}, ResultType: RoomRoutingView{}, ErrorCodes: []int{404}},
		{Name: "room_routing_set", Summary: "Replace a room's audio routing matrix", PayloadType: roomRoutingSetPayload{}, ResultType: RoomRoutingView{}, ErrorCodes: []int{400, 404}},
		{Name: "room_routing_update", Summary: "Update selected rows of a room's audio routing matrix", PayloadType: roomRoutingUpdatePayload{}, ResultType: RoomRoutingView{}, ErrorCodes: []int{400, 404}},
		{Name: "set_leg_role", Summary: "Change a leg's routing role (recomputes the room matrix if the leg is in a room)", PayloadType: setLegRolePayload{}, ResultType: LegView{}, ErrorCodes: []int{400, 404}},

		// ── Leg control ─────────────────────────────────────────────────
		// New commands use resource-first naming (leg_*, room_*) by design.
		// This diverges from earlier verb-first commands (mute_leg, send_leg_dtmf)
		// — preserve the resource-first style for any future commands added here.
		{Name: "leg_ring", Summary: "Send a 180 Ringing on a SIP inbound leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_early_media", Summary: "Enable early media (183 Session Progress) on a SIP inbound leg", PayloadType: earlyMediaPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_amd_start", Summary: "Start Answering Machine Detection on a connected SIP leg", PayloadType: legAMDStartPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409}},

		// ── Recording ───────────────────────────────────────────────────
		{Name: "leg_record_start", Summary: "Start recording a leg (stereo when in a room or SIP, mono otherwise)", PayloadType: recordStartPayload{}, ResultType: RecordingStartResult{}, ErrorCodes: []int{400, 404, 409, 500}},
		{Name: "room_record_start", Summary: "Start recording the full room mix (mono; multi_channel adds per-participant tracks)", PayloadType: recordStartPayload{}, ResultType: RecordingStartResult{}, ErrorCodes: []int{400, 404, 409, 500}},
		{Name: "leg_record_pause", Summary: "Pause a leg recording", PayloadType: idPayload{}, ResultType: RecordingPauseResumeResult{}, ErrorCodes: []int{404}},
		{Name: "leg_record_resume", Summary: "Resume a paused leg recording", PayloadType: idPayload{}, ResultType: RecordingPauseResumeResult{}, ErrorCodes: []int{404}},
		{Name: "leg_record_stop", Summary: "Stop a leg recording", PayloadType: idPayload{}, ResultType: RecordingStopLegResult{}, ErrorCodes: []int{404}},
		{Name: "room_record_stop", Summary: "Stop a room recording", PayloadType: idPayload{}, ResultType: RecordingStopRoomResult{}, ErrorCodes: []int{404}},
		{Name: "room_record_pause", Summary: "Pause a room recording", PayloadType: idPayload{}, ResultType: RecordingPauseResumeResult{}, ErrorCodes: []int{404}},
		{Name: "room_record_resume", Summary: "Resume a paused room recording", PayloadType: idPayload{}, ResultType: RecordingPauseResumeResult{}, ErrorCodes: []int{404}},

		// ── Playback ────────────────────────────────────────────────────
		{Name: "leg_play_start", Summary: "Start audio playback on a leg", PayloadType: playbackStartPayload{}, ResultType: PlaybackStartResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_play_stop", Summary: "Stop a leg playback", PayloadType: playbackTargetPayload{}, ResultType: PlaybackStopResult{}, ErrorCodes: []int{404}},
		{Name: "room_play_start", Summary: "Start audio playback on a room mix", PayloadType: playbackStartPayload{}, ResultType: PlaybackStartResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "room_play_stop", Summary: "Stop a room playback", PayloadType: playbackTargetPayload{}, ResultType: PlaybackStopResult{}, ErrorCodes: []int{404}},
		{Name: "leg_play_volume", Summary: "Adjust the volume of an active leg playback", PayloadType: playbackVolumePayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404}},
		{Name: "room_play_volume", Summary: "Adjust the volume of an active room playback", PayloadType: playbackVolumePayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404}},

		// ── STT ─────────────────────────────────────────────────────────
		{Name: "leg_stt_start", Summary: "Start speech-to-text on a leg", PayloadType: sttStartPayload{}, ResultType: STTStartLegResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_stt_start", Summary: "Start speech-to-text on every participant of a room (auto-extends to legs that join later)", PayloadType: sttStartPayload{}, ResultType: STTStartRoomResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_stt_stop", Summary: "Stop speech-to-text on a leg", PayloadType: idPayload{}, ResultType: STTStopResult{}, ErrorCodes: []int{404}},
		{Name: "room_stt_stop", Summary: "Stop speech-to-text on a room", PayloadType: idPayload{}, ResultType: STTStopResult{}, ErrorCodes: []int{404}},

		// ── TTS ─────────────────────────────────────────────────────────
		{Name: "leg_tts", Summary: "Synthesize speech and play it on a leg", PayloadType: ttsStartPayload{}, ResultType: TTSStartResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_tts", Summary: "Synthesize speech and play it into a room mix", PayloadType: ttsStartPayload{}, ResultType: TTSStartResult{}, ErrorCodes: []int{400, 404, 409, 503}},

		// ── Transfer ────────────────────────────────────────────────────
		{Name: "leg_transfer", Summary: "Initiate a SIP REFER transfer (blind or attended)", PayloadType: transferLegPayload{}, ResultType: TransferLegResult{}, ErrorCodes: []int{400, 404, 409}},

		// ── Agent ───────────────────────────────────────────────────────
		{Name: "leg_agent_elevenlabs", Summary: "Attach an ElevenLabs Conversational AI agent to a leg", PayloadType: agentElevenLabsPayload{}, ResultType: AgentStartLegResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_agent_vapi", Summary: "Attach a VAPI agent to a leg", PayloadType: agentVAPIPayload{}, ResultType: AgentStartLegResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_agent_pipecat", Summary: "Attach a Pipecat bot to a leg", PayloadType: agentPipecatPayload{}, ResultType: AgentStartLegResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_agent_deepgram", Summary: "Attach a Deepgram Voice Agent to a leg", PayloadType: agentDeepgramPayload{}, ResultType: AgentStartLegResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_agent_elevenlabs", Summary: "Attach an ElevenLabs Conversational AI agent to a room", PayloadType: agentElevenLabsPayload{}, ResultType: AgentStartRoomResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_agent_vapi", Summary: "Attach a VAPI agent to a room", PayloadType: agentVAPIPayload{}, ResultType: AgentStartRoomResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_agent_pipecat", Summary: "Attach a Pipecat bot to a room", PayloadType: agentPipecatPayload{}, ResultType: AgentStartRoomResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "room_agent_deepgram", Summary: "Attach a Deepgram Voice Agent to a room", PayloadType: agentDeepgramPayload{}, ResultType: AgentStartRoomResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_agent_stop", Summary: "Detach the agent attached to a leg", PayloadType: idPayload{}, ResultType: AgentStopResult{}, ErrorCodes: []int{404}},
		{Name: "room_agent_stop", Summary: "Detach the agent attached to a room", PayloadType: idPayload{}, ResultType: AgentStopResult{}, ErrorCodes: []int{404}},
		{Name: "leg_agent_message", Summary: "Inject a text message into a leg agent session", PayloadType: agentMessagePayload{}, ResultType: AgentMessageResult{}, ErrorCodes: []int{400, 404, 409, 500, 501}},
		{Name: "room_agent_message", Summary: "Inject a text message into a room agent session", PayloadType: agentMessagePayload{}, ResultType: AgentMessageResult{}, ErrorCodes: []int{400, 404, 409, 500, 501}},

		// ── SIP Registrations ───────────────────────────────────────────
		{Name: "list_sip_registrations", Summary: "List active SIP AOR registrations", ResultType: RegistrationsResponse{}},
	}
}

// EventMeta describes a single event published on the bus, the webhook
// channel, and the VSI WebSocket. asyncapi-gen and openapi-gen both consume
// this list. Adding a new event type is a two-step change: register it in
// internal/events/types.go AND add it here.
type EventMeta struct {
	Type     events.EventType
	Summary  string
	DataType reflect.Type
}

// EventsMetadata returns every event published over the bus. asyncapi-gen
// emits one channel per entry; openapi-gen emits one x-webhooks entry per
// entry. Keeping a single list ensures the two specs cannot drift apart.
func EventsMetadata() []EventMeta {
	return []EventMeta{
		{events.LegRinging, "SIP call ringing (inbound or outbound)", reflect.TypeOf(events.LegRingingData{})},
		{events.LegEarlyMedia, "Outbound leg received 183 Session Progress with SDP; media pipeline active", reflect.TypeOf(events.LegEarlyMediaData{})},
		{events.LegConnected, "Leg answered/connected", reflect.TypeOf(events.LegConnectedData{})},
		{events.LegDisconnected, "Leg hung up (CDR-style nested structure)", reflect.TypeOf(events.LegDisconnectedData{})},
		{events.LegJoinedRoom, "Leg added to a room", reflect.TypeOf(events.LegJoinedRoomData{})},
		{events.LegLeftRoom, "Leg removed from a room", reflect.TypeOf(events.LegLeftRoomData{})},
		{events.LegMuted, "Leg muted", reflect.TypeOf(events.LegMutedData{})},
		{events.LegUnmuted, "Leg unmuted", reflect.TypeOf(events.LegUnmutedData{})},
		{events.LegDeaf, "Leg deafened (stops receiving room audio)", reflect.TypeOf(events.LegDeafData{})},
		{events.LegUndeaf, "Leg undeafened (resumes receiving room audio)", reflect.TypeOf(events.LegUndeafData{})},
		{events.LegHold, "Leg put on hold (local or remote)", reflect.TypeOf(events.LegHoldData{})},
		{events.LegUnhold, "Leg taken off hold (local or remote)", reflect.TypeOf(events.LegUnholdData{})},
		{events.LegCommandFailed, "An asynchronous leg command (202 Accepted) failed during execution", reflect.TypeOf(events.LegCommandFailedData{})},
		{events.DTMFReceived, "DTMF digit received", reflect.TypeOf(events.DTMFReceivedData{})},
		{events.RTTReceived, "Real-Time Text (T.140 / RFC 4103) chunk received from the remote", reflect.TypeOf(events.RTTReceivedData{})},
		{events.SpeakingStarted, "Participant started speaking", reflect.TypeOf(events.SpeakingData{})},
		{events.SpeakingStopped, "Participant stopped speaking", reflect.TypeOf(events.SpeakingData{})},
		{events.PlaybackStarted, "Playback began", reflect.TypeOf(events.PlaybackStartedData{})},
		{events.PlaybackFinished, "Playback ended", reflect.TypeOf(events.PlaybackFinishedData{})},
		{events.PlaybackError, "Playback failed", reflect.TypeOf(events.PlaybackErrorData{})},
		{events.TTSStarted, "TTS synthesis began playing", reflect.TypeOf(events.TTSStartedData{})},
		{events.TTSFinished, "TTS synthesis finished playing", reflect.TypeOf(events.TTSFinishedData{})},
		{events.TTSError, "TTS synthesis or playback failed", reflect.TypeOf(events.TTSErrorData{})},
		{events.RecordingStarted, "Recording began", reflect.TypeOf(events.RecordingStartedData{})},
		{events.RecordingFinished, "Recording ended", reflect.TypeOf(events.RecordingFinishedData{})},
		{events.RecordingPaused, "Recording paused (audio replaced with silence)", reflect.TypeOf(events.RecordingPausedData{})},
		{events.RecordingResumed, "Recording resumed from a paused state", reflect.TypeOf(events.RecordingResumedData{})},
		{events.LegTransferInitiated, "We sent a SIP REFER (transfer initiated by the operator)", reflect.TypeOf(events.LegTransferInitiatedData{})},
		{events.LegTransferRequested, "We received a SIP REFER from the peer", reflect.TypeOf(events.LegTransferRequestedData{})},
		{events.LegTransferProgress, "Transfer progress reported via NOTIFY sipfrag", reflect.TypeOf(events.LegTransferProgressData{})},
		{events.LegTransferCompleted, "Transfer reached terminal 2xx", reflect.TypeOf(events.LegTransferCompletedData{})},
		{events.LegTransferFailed, "Transfer failed (REFER rejected, sipfrag non-2xx, or local error)", reflect.TypeOf(events.LegTransferFailedData{})},
		{events.RoomCreated, "Room created", reflect.TypeOf(events.RoomCreatedData{})},
		{events.RoomDeleted, "Room deleted", reflect.TypeOf(events.RoomDeletedData{})},
		{events.RoomBridged, "Two rooms' mixers were joined", reflect.TypeOf(events.RoomBridgedData{})},
		{events.RoomBridgeUpdated, "A bridge's audio flow direction changed", reflect.TypeOf(events.RoomBridgeUpdatedData{})},
		{events.RoomUnbridged, "A bridge was torn down", reflect.TypeOf(events.RoomUnbridgedData{})},
		{events.RoomRoutingChanged, "The room's audio routing matrix changed", reflect.TypeOf(events.RoomRoutingChangedData{})},
		{events.LegRoleChanged, "A leg's routing role changed", reflect.TypeOf(events.LegRoleChangedData{})},
		{events.STTText, "Speech-to-text transcript", reflect.TypeOf(events.STTTextData{})},
		{events.AgentConnected, "Agent connected to provider", reflect.TypeOf(events.AgentConnectedData{})},
		{events.AgentDisconnected, "Agent session ended", reflect.TypeOf(events.AgentDisconnectedData{})},
		{events.AgentUserTranscript, "User speech transcribed by agent", reflect.TypeOf(events.AgentTranscriptData{})},
		{events.AgentAgentResponse, "Agent generated a response", reflect.TypeOf(events.AgentResponseData{})},
		{events.AMDResult, "Answering machine detection completed", reflect.TypeOf(events.AMDResultData{})},
		{events.AMDBeep, "Voicemail beep tone detected after machine classification", reflect.TypeOf(events.AMDBeepData{})},
		{events.SIPRegistrationActive, "SIP AOR registration created or refreshed (one event per Contact)", reflect.TypeOf(events.SIPRegistrationActiveData{})},
		{events.SIPRegistrationExpired, "SIP AOR registration removed (TTL, explicit unregister, force-delete, or single-binding replacement)", reflect.TypeOf(events.SIPRegistrationExpiredData{})},
		{events.LiveKitParticipantJoined, "Remote LiveKit participant joined the room of a livekit_room leg", reflect.TypeOf(events.LiveKitParticipantJoinedData{})},
		{events.LiveKitParticipantLeft, "Remote LiveKit participant left the room of a livekit_room leg", reflect.TypeOf(events.LiveKitParticipantLeftData{})},
		{events.LiveKitParticipantSpeakingStarted, "Remote LiveKit participant started speaking", reflect.TypeOf(events.LiveKitParticipantSpeakingData{})},
		{events.LiveKitParticipantSpeakingStopped, "Remote LiveKit participant stopped speaking", reflect.TypeOf(events.LiveKitParticipantSpeakingData{})},
		{events.LiveKitTrackPublished, "A remote LiveKit participant published a track", reflect.TypeOf(events.LiveKitTrackPublishedData{})},
		{events.LiveKitTrackUnpublished, "A remote LiveKit participant's track was removed", reflect.TypeOf(events.LiveKitTrackUnpublishedData{})},
	}
}

// VSILifecycleFrames returns the special server-sent and client-sent
// non-command frames on the /v1/vsi WebSocket. asyncapi-gen emits these
// as separate operations alongside the command set.
type VSILifecycleFrame struct {
	Name        string
	Direction   string // "send" (server→client) or "receive" (client→server)
	Description string
}

func VSILifecycleFramesMetadata() []VSILifecycleFrame {
	return []VSILifecycleFrame{
		{Name: "connected", Direction: "send", Description: "First frame the server sends after the WebSocket upgrade completes. Carries no data."},
		{Name: "ping", Direction: "send", Description: "Periodic application-level keepalive (every ~30 s). Clients may reply with `pong` or ignore."},
		{Name: "pong", Direction: "receive", Description: "Optional client keepalive reply to a `ping`. Currently a no-op on the server side."},
		{Name: "stop", Direction: "receive", Description: "Client-initiated graceful close. The server stops the recv loop and closes the connection."},
		{Name: "events_dropped", Direction: "send", Description: "Sent when the per-connection event buffer overflowed. The accompanying `count` indicates how many events were dropped since the last notification. Clients should resync via REST after seeing this."},
		{Name: "error", Direction: "send", Description: "Generic error frame for invalid JSON, unknown command types, or command handler failures. Echoes `request_id` when the offending message had one. `data` carries `{code, message}`."},
	}
}
