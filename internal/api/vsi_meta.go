package api

import (
	"reflect"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// VSICommandMeta describes a single command accepted on the /v1/vsi
// WebSocket. The asyncapi-gen tool consumes this list to emit asyncapi.yaml
// — every VSI command MUST be present here, otherwise the generated spec
// will be incomplete and the rule in CLAUDE.md is violated.
//
// Inbound frame shape: {"type": Name, "request_id": "...", "payload": Payload}.
// Successful response: {"type": Name + ".result", "request_id": "...", "data": Result}.
// Error response: {"type": "error", "request_id": "...", "data": {"code": int, "message": string}}.
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
			PayloadType: vsiAnswerLegPayload{}, ResultType: vsiStatusResponse{},
			ErrorCodes: []int{400, 404, 409},
		},
		{
			Name: "delete_leg", Summary: "Hang up a leg",
			PayloadType: vsiDeleteLegPayload{}, ResultType: vsiStatusResponse{},
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
	}
}

// vsiAnswerLegPayload mirrors the inline struct in ws_commands.go answer_leg case.
type vsiAnswerLegPayload struct {
	ID              string `json:"id"`
	SpeechDetection *bool  `json:"speech_detection,omitempty"`
	Codec           string `json:"codec,omitempty"`
}

// vsiDeleteLegPayload mirrors the inline struct in ws_commands.go delete_leg case.
type vsiDeleteLegPayload struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
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
		{events.STTText, "Speech-to-text transcript", reflect.TypeOf(events.STTTextData{})},
		{events.AgentConnected, "Agent connected to provider", reflect.TypeOf(events.AgentConnectedData{})},
		{events.AgentDisconnected, "Agent session ended", reflect.TypeOf(events.AgentDisconnectedData{})},
		{events.AgentUserTranscript, "User speech transcribed by agent", reflect.TypeOf(events.AgentTranscriptData{})},
		{events.AgentAgentResponse, "Agent generated a response", reflect.TypeOf(events.AgentResponseData{})},
		{events.AMDResult, "Answering machine detection completed", reflect.TypeOf(events.AMDResultData{})},
		{events.AMDBeep, "Voicemail beep tone detected after machine classification", reflect.TypeOf(events.AMDBeepData{})},
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
