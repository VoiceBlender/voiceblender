package api

import (
	"reflect"

	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
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
	Tag         string // REST tag of the matching endpoints
	Section     string // one of TagSections()[Tag], when that tag is sectioned
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
			Name: "list_legs", Tag: "Legs", Section: "Lifecycle", Summary: "List all active legs",
			ResultType: []LegView{},
		},
		{
			Name: "get_leg", Tag: "Legs", Section: "Lifecycle", Summary: "Get a single leg by id",
			PayloadType: idPayload{}, ResultType: LegView{},
			ErrorCodes: []int{404},
		},
		// ── Leg lifecycle ───────────────────────────────────────────────
		{
			Name: "create_leg", Tag: "Legs", Section: "Lifecycle", Summary: "Originate an outbound leg",
			Description: "Originates an outbound leg, mirroring POST /v1/legs, and returns the leg " +
				"view. Supports all leg types: \"sip\", \"websocket\", \"whatsapp\", and " +
				"\"livekit_room\". For \"livekit_room\", custom headers come from the request's " +
				"`headers` map (there is no HTTP request over the WebSocket).",
			PayloadType: CreateLegRequest{}, ResultType: LegView{},
			ErrorCodes: []int{400, 404, 500, 502, 503},
		},
		{
			Name: "answer_leg", Tag: "Legs", Section: "Inbound calls", Summary: "Answer a ringing inbound leg",
			PayloadType: answerLegPayload{}, ResultType: vsiStatusResponse{},
			ErrorCodes: []int{400, 404, 409},
		},
		{
			Name: "delete_leg", Tag: "Legs", Section: "Lifecycle", Summary: "Hang up a leg",
			PayloadType: deleteLegPayload{}, ResultType: vsiStatusResponse{},
			ErrorCodes: []int{400, 404},
		},
		// ── Mute / deaf / hold ──────────────────────────────────────────
		{Name: "mute_leg", Tag: "Legs", Section: "Mute, deafen & hold", Summary: "Mute a leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "unmute_leg", Tag: "Legs", Section: "Mute, deafen & hold", Summary: "Unmute a leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "deaf_leg", Tag: "Legs", Section: "Mute, deafen & hold", Summary: "Deafen a leg (stop receiving room audio)", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "undeaf_leg", Tag: "Legs", Section: "Mute, deafen & hold", Summary: "Undeafen a leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "hold_leg", Tag: "Legs", Section: "Mute, deafen & hold", Summary: "Put a SIP leg on hold", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404, 409}},
		{Name: "unhold_leg", Tag: "Legs", Section: "Mute, deafen & hold", Summary: "Resume a held SIP leg", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404, 409}},
		// ── DTMF ────────────────────────────────────────────────────────
		{Name: "send_leg_dtmf", Tag: "Legs", Section: "DTMF & real-time text", Summary: "Send DTMF digits on a leg", PayloadType: dtmfPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 500}},
		{Name: "accept_leg_dtmf", Tag: "Legs", Section: "DTMF & real-time text", Summary: "Enable DTMF reception", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "reject_leg_dtmf", Tag: "Legs", Section: "DTMF & real-time text", Summary: "Disable DTMF reception", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		// ── RTT (T.140) ─────────────────────────────────────────────────
		{Name: "send_leg_rtt", Tag: "Legs", Section: "DTMF & real-time text", Summary: "Send Real-Time Text (T.140) on a SIP leg", PayloadType: rttPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409, 500}},
		{Name: "accept_leg_rtt", Tag: "Legs", Section: "DTMF & real-time text", Summary: "Enable RTT reception", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "reject_leg_rtt", Tag: "Legs", Section: "DTMF & real-time text", Summary: "Disable RTT reception", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		// ── WebRTC ──────────────────────────────────────────────────────
		{
			Name: "webrtc_offer", Tag: "WebRTC", Summary: "Establish a WebRTC leg via SDP offer/answer",
			Description: "Accepts a browser SDP offer, allocates a peer connection, returns the local SDP answer plus the new leg id. " +
				"Subsequent ICE candidates from the browser are delivered via webrtc_add_candidate; server-side candidates are drained via webrtc_get_candidates.",
			PayloadType: WebRTCOfferRequest{}, ResultType: WebRTCOfferResult{},
			ErrorCodes: []int{400, 500},
		},
		{
			Name: "webrtc_add_candidate", Tag: "WebRTC", Summary: "Add a remote ICE candidate to a WebRTC leg",
			PayloadType: vsiWebRTCAddCandidatePayload{}, ResultType: vsiStatusResponse{},
			ErrorCodes: []int{400, 404, 500},
		},
		{
			Name: "webrtc_get_candidates", Tag: "WebRTC", Summary: "Drain server-gathered ICE candidates for a WebRTC leg",
			Description: "Returns any local ICE candidates that have been gathered since the last call, plus a `done` flag indicating ICE gathering has completed. Clients should poll until `done` is true.",
			PayloadType: idPayload{}, ResultType: WebRTCCandidatesResult{},
			ErrorCodes: []int{400, 404},
		},
		{
			Name: "webrtc_ice_restart", Tag: "WebRTC", Summary: "Restart ICE on a WebRTC leg",
			Description: "Applies a new SDP offer carrying fresh ICE credentials (browser `restartIce()`) to the existing leg and returns the answer; the leg id, media session and room membership are kept. " +
				"Send the client's new candidates with webrtc_add_candidate only after this result arrives, and poll webrtc_get_candidates again: its `done` flag restarts at false.",
			PayloadType: vsiWebRTCICERestartPayload{}, ResultType: WebRTCOfferResult{},
			ErrorCodes: []int{400, 404, 500},
		},
		// ── Rooms ───────────────────────────────────────────────────────
		{Name: "list_rooms", Tag: "Rooms", Section: "Lifecycle", Summary: "List all rooms", ResultType: []RoomView{}},
		{Name: "get_room", Tag: "Rooms", Section: "Lifecycle", Summary: "Get a single room by id", PayloadType: idPayload{}, ResultType: RoomView{}, ErrorCodes: []int{404}},
		{Name: "create_room", Tag: "Rooms", Section: "Lifecycle", Summary: "Create a room", PayloadType: CreateRoomRequest{}, ResultType: RoomView{}, ErrorCodes: []int{400, 409}},
		{Name: "delete_room", Tag: "Rooms", Section: "Lifecycle", Summary: "Delete a room", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "add_leg_to_room", Tag: "Rooms", Section: "Participants", Summary: "Add or move a leg into a room", PayloadType: addLegPayload{}, ResultType: AddLegToRoomResult{}, ErrorCodes: []int{400, 404}},
		{Name: "remove_leg_from_room", Tag: "Rooms", Section: "Participants", Summary: "Remove a leg from a room", PayloadType: roomLegPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},

		// ── Bridges ─────────────────────────────────────────────────────
		{Name: "bridge_create", Tag: "Rooms", Section: "Bridges", Summary: "Join two rooms' mixers", PayloadType: bridgeCreatePayload{}, ResultType: BridgeView{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "bridge_list", Tag: "Rooms", Section: "Bridges", Summary: "List bridges involving a room", PayloadType: bridgeListPayload{}, ResultType: []BridgeView{}, ErrorCodes: []int{404}},
		{Name: "bridge_get", Tag: "Rooms", Section: "Bridges", Summary: "Get a bridge involving a room", PayloadType: bridgeRefPayload{}, ResultType: BridgeView{}, ErrorCodes: []int{404}},
		{Name: "bridge_update", Tag: "Rooms", Section: "Bridges", Summary: "Change a bridge's direction", PayloadType: bridgeUpdatePayload{}, ResultType: BridgeView{}, ErrorCodes: []int{400, 404}},
		{Name: "bridge_delete", Tag: "Rooms", Section: "Bridges", Summary: "Tear down a bridge", PayloadType: bridgeRefPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},

		// ── Routing matrix ──────────────────────────────────────────────
		{Name: "room_routing_get", Tag: "Rooms", Section: "Audio routing", Summary: "Get a room's audio routing matrix", PayloadType: idPayload{}, ResultType: RoomRoutingView{}, ErrorCodes: []int{404}},
		{Name: "room_routing_set", Tag: "Rooms", Section: "Audio routing", Summary: "Replace a room's audio routing matrix", PayloadType: roomRoutingSetPayload{}, ResultType: RoomRoutingView{}, ErrorCodes: []int{400, 404}},
		{Name: "room_routing_update", Tag: "Rooms", Section: "Audio routing", Summary: "Update selected rows of a room's audio routing matrix", PayloadType: roomRoutingUpdatePayload{}, ResultType: RoomRoutingView{}, ErrorCodes: []int{400, 404}},
		{Name: "set_leg_role", Tag: "Legs", Section: "Settings", Summary: "Change a leg's routing role (recomputes the room matrix if the leg is in a room)", PayloadType: setLegRolePayload{}, ResultType: LegView{}, ErrorCodes: []int{400, 404}},
		{Name: "set_leg_filters", Tag: "Legs", Section: "Settings", Summary: "Replace the audio filter chain running on a live leg", PayloadType: setLegFiltersPayload{}, ResultType: LegView{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "set_leg_custom_data", Tag: "Legs", Section: "Settings", Summary: "Replace a leg's custom_data (carried on every subsequent event for the leg)", PayloadType: setLegCustomDataPayload{}, ResultType: LegView{}, ErrorCodes: []int{400, 404}},
		{Name: "delete_leg_custom_data", Tag: "Legs", Section: "Settings", Summary: "Clear a leg's custom_data", PayloadType: idPayload{}, ResultType: LegView{}, ErrorCodes: []int{404}},

		// ── Per-leg audio streams (multiple m=audio sections) ───────────
		{Name: "leg_stream_list", Tag: "Legs", Section: "Audio streams", Summary: "List a SIP leg's negotiated audio streams", PayloadType: idPayload{}, ResultType: []LegStreamView{}, ErrorCodes: []int{400, 404}},
		{Name: "leg_stream_get", Tag: "Legs", Section: "Audio streams", Summary: "Get one of a SIP leg's audio streams", PayloadType: legStreamPayload{}, ResultType: LegStreamView{}, ErrorCodes: []int{400, 404}},
		{Name: "leg_stream_add", Tag: "Legs", Section: "Audio streams", Summary: "Negotiate an additional m=audio section on a live dialog via re-INVITE", PayloadType: legStreamAddPayload{}, ResultType: LegStreamView{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_stream_update", Tag: "Legs", Section: "Audio streams", Summary: "Change one of a leg's audio streams' routing role in place", PayloadType: legStreamUpdatePayload{}, ResultType: LegStreamView{}, ErrorCodes: []int{400, 404}},
		{Name: "leg_stream_remove", Tag: "Legs", Section: "Audio streams", Summary: "Disable one of a leg's audio streams with a port-0 re-INVITE", PayloadType: legStreamPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_stream_attach_room", Tag: "Legs", Section: "Audio streams", Summary: "Mix one of a leg's audio streams into a room (may differ from the leg's own room)", PayloadType: legStreamRoomPayload{}, ResultType: LegStreamView{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_stream_detach_room", Tag: "Legs", Section: "Audio streams", Summary: "Remove one of a leg's audio streams from whichever room mixes it", PayloadType: legStreamPayload{}, ResultType: LegStreamView{}, ErrorCodes: []int{400, 404}},

		// ── SIPREC recording sessions (RFC 7865 / RFC 7866) ─────────────
		{Name: "leg_siprec_start", Tag: "Legs", Section: "Recording & SIPREC", Summary: "Fork a single call to an external SIPREC recording server", PayloadType: roomSIPRECStartPayload{}, ResultType: LegView{}, ErrorCodes: []int{400, 403, 404, 409, 502}},
		{Name: "room_siprec_start", Tag: "Rooms", Section: "Recording & SIPREC", Summary: "Fork a room's participants to an external SIPREC recording server", PayloadType: roomSIPRECStartPayload{}, ResultType: LegView{}, ErrorCodes: []int{400, 403, 404, 409, 502}},
		{Name: "siprec_get", Tag: "Legs", Section: "Recording & SIPREC", Summary: "Get an inbound SIPREC recording session's participants, streams and metadata", PayloadType: idPayload{}, ResultType: SIPRECSessionView{}, ErrorCodes: []int{400, 404}},

		// ── Leg control ─────────────────────────────────────────────────
		// New commands use resource-first naming (leg_*, room_*) by design.
		// This diverges from earlier verb-first commands (mute_leg, send_leg_dtmf)
		// — preserve the resource-first style for any future commands added here.
		{Name: "leg_ring", Tag: "Legs", Section: "Inbound calls", Summary: "Send a 180 Ringing on a SIP inbound leg", PayloadType: ringLegPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_early_media", Tag: "Legs", Section: "Inbound calls", Summary: "Enable early media (183 Session Progress) on a SIP inbound leg", PayloadType: earlyMediaPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_amd_start", Tag: "Legs", Section: "Speech-to-text & AMD", Summary: "Start Answering Machine Detection on a connected SIP leg", PayloadType: legAMDStartPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409}},

		// ── Recording ───────────────────────────────────────────────────
		{Name: "leg_record_start", Tag: "Legs", Section: "Recording & SIPREC", Summary: "Start recording a leg (stereo when in a room or SIP, mono otherwise)", PayloadType: recordStartPayload{}, ResultType: RecordingStartResult{}, ErrorCodes: []int{400, 404, 409, 500}},
		{Name: "room_record_start", Tag: "Rooms", Section: "Recording & SIPREC", Summary: "Start recording the full room mix (mono; multi_channel adds per-participant tracks)", PayloadType: recordStartPayload{}, ResultType: RecordingStartResult{}, ErrorCodes: []int{400, 404, 409, 500}},
		{Name: "leg_record_pause", Tag: "Legs", Section: "Recording & SIPREC", Summary: "Pause a leg recording", PayloadType: idPayload{}, ResultType: RecordingPauseResumeResult{}, ErrorCodes: []int{404}},
		{Name: "leg_record_resume", Tag: "Legs", Section: "Recording & SIPREC", Summary: "Resume a paused leg recording", PayloadType: idPayload{}, ResultType: RecordingPauseResumeResult{}, ErrorCodes: []int{404}},
		{Name: "leg_record_stop", Tag: "Legs", Section: "Recording & SIPREC", Summary: "Stop a leg recording", PayloadType: idPayload{}, ResultType: RecordingStopLegResult{}, ErrorCodes: []int{404}},
		{Name: "room_record_stop", Tag: "Rooms", Section: "Recording & SIPREC", Summary: "Stop a room recording", PayloadType: idPayload{}, ResultType: RecordingStopRoomResult{}, ErrorCodes: []int{404}},
		{Name: "room_record_pause", Tag: "Rooms", Section: "Recording & SIPREC", Summary: "Pause a room recording", PayloadType: idPayload{}, ResultType: RecordingPauseResumeResult{}, ErrorCodes: []int{404}},
		{Name: "room_record_resume", Tag: "Rooms", Section: "Recording & SIPREC", Summary: "Resume a paused room recording", PayloadType: idPayload{}, ResultType: RecordingPauseResumeResult{}, ErrorCodes: []int{404}},

		// ── Playback ────────────────────────────────────────────────────
		{Name: "leg_play_start", Tag: "Legs", Section: "Playback & TTS", Summary: "Start audio playback on a leg", PayloadType: playbackStartPayload{}, ResultType: PlaybackStartResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_play_stop", Tag: "Legs", Section: "Playback & TTS", Summary: "Stop a leg playback", PayloadType: playbackTargetPayload{}, ResultType: PlaybackStopResult{}, ErrorCodes: []int{404}},
		{Name: "room_play_start", Tag: "Rooms", Section: "Playback & TTS", Summary: "Start audio playback on a room mix", PayloadType: playbackStartPayload{}, ResultType: PlaybackStartResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "room_play_stop", Tag: "Rooms", Section: "Playback & TTS", Summary: "Stop a room playback", PayloadType: playbackTargetPayload{}, ResultType: PlaybackStopResult{}, ErrorCodes: []int{404}},
		{Name: "leg_play_volume", Tag: "Legs", Section: "Playback & TTS", Summary: "Adjust the volume of an active leg playback", PayloadType: playbackVolumePayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404}},
		{Name: "room_play_volume", Tag: "Rooms", Section: "Playback & TTS", Summary: "Adjust the volume of an active room playback", PayloadType: playbackVolumePayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404}},

		// ── STT ─────────────────────────────────────────────────────────
		{Name: "leg_stt_start", Tag: "Legs", Section: "Speech-to-text & AMD", Summary: "Start speech-to-text on a leg", PayloadType: sttStartPayload{}, ResultType: STTStartLegResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_stt_start", Tag: "Rooms", Section: "Speech-to-text", Summary: "Start speech-to-text on every participant of a room (auto-extends to legs that join later)", PayloadType: sttStartPayload{}, ResultType: STTStartRoomResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_stt_stop", Tag: "Legs", Section: "Speech-to-text & AMD", Summary: "Stop speech-to-text on a leg", PayloadType: idPayload{}, ResultType: STTStopResult{}, ErrorCodes: []int{404}},
		{Name: "room_stt_stop", Tag: "Rooms", Section: "Speech-to-text", Summary: "Stop speech-to-text on a room", PayloadType: idPayload{}, ResultType: STTStopResult{}, ErrorCodes: []int{404}},
		{Name: "leg_stt_finalize", Tag: "Legs", Section: "Speech-to-text & AMD", Summary: "Flush the speech-to-text buffer on a leg and emit a final transcript without stopping STT", PayloadType: idPayload{}, ResultType: STTFinalizeResult{}, ErrorCodes: []int{404, 409, 501}},

		// ── TTS ─────────────────────────────────────────────────────────
		{Name: "leg_tts", Tag: "Legs", Section: "Playback & TTS", Summary: "Synthesize speech and play it on a leg", PayloadType: ttsStartPayload{}, ResultType: TTSStartResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_tts_preflight", Tag: "Legs", Section: "Playback & TTS", Summary: "Synthesize speech and hold it for a later commit", Description: "Stages a speculative reply: the audio is synthesized and buffered but not played, so that committing it starts playback with no synthesis delay. Use with the eager_end_of_turn stt.turn event to begin generating a reply before the caller has finished speaking, then commit on end_of_turn or discard on turn_resumed. A tts.staged event reports when the audio is ready. Staged utterances are dropped after TTS_PREFLIGHT_TTL or when the leg ends.", PayloadType: ttsStartPayload{}, ResultType: TTSStartResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_tts_commit", Tag: "Legs", Section: "Playback & TTS", Summary: "Play a staged TTS utterance", Description: "Starts playback of an utterance staged by leg_tts_preflight. Returns immediately, before synthesis has necessarily finished; failure is reported asynchronously on tts.error, exactly as for leg_tts. Once committed, stop it with leg_play_stop using the same tts_id.", PayloadType: ttsTargetPayload{}, ResultType: TTSStartResult{}, ErrorCodes: []int{404, 409}},
		{Name: "leg_tts_discard", Tag: "Legs", Section: "Playback & TTS", Summary: "Drop a staged TTS utterance without playing it", PayloadType: ttsTargetPayload{}, ResultType: TTSDiscardResult{}, ErrorCodes: []int{404, 409}},
		{Name: "room_tts", Tag: "Rooms", Section: "Playback & TTS", Summary: "Synthesize speech and play it into a room mix", PayloadType: ttsStartPayload{}, ResultType: TTSStartResult{}, ErrorCodes: []int{400, 404, 409, 503}},

		// ── Transfer ────────────────────────────────────────────────────
		{Name: "leg_transfer", Tag: "Legs", Section: "Transfer", Summary: "Initiate a SIP REFER transfer (blind or attended)", PayloadType: transferLegPayload{}, ResultType: TransferLegResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "accept_transfer", Tag: "Legs", Section: "Transfer", Summary: "Accept a parked inbound REFER (send 202 + NOTIFY 100 Trying)", PayloadType: acceptTransferPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "progress_transfer", Tag: "Legs", Section: "Transfer", Summary: "Send an interim sipfrag NOTIFY (e.g. 180 Ringing) on an accepted inbound transfer", PayloadType: progressTransferPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404}},
		{Name: "complete_transfer", Tag: "Legs", Section: "Transfer", Summary: "Terminate an accepted inbound transfer with a final sipfrag NOTIFY (200 OK or failure)", PayloadType: completeTransferPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "decline_transfer", Tag: "Legs", Section: "Transfer", Summary: "Reject a parked (not-yet-accepted) inbound REFER (603 by default)", PayloadType: declineTransferPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},

		// ── Agent ───────────────────────────────────────────────────────
		{Name: "leg_agent_elevenlabs", Tag: "Legs", Section: "AI agents", Summary: "Attach an ElevenLabs Conversational AI agent to a leg", PayloadType: agentElevenLabsPayload{}, ResultType: AgentStartLegResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_agent_vapi", Tag: "Legs", Section: "AI agents", Summary: "Attach a VAPI agent to a leg", PayloadType: agentVAPIPayload{}, ResultType: AgentStartLegResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_agent_pipecat", Tag: "Legs", Section: "AI agents", Summary: "Attach a Pipecat bot to a leg", PayloadType: agentPipecatPayload{}, ResultType: AgentStartLegResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "leg_agent_deepgram", Tag: "Legs", Section: "AI agents", Summary: "Attach a Deepgram Voice Agent to a leg", PayloadType: agentDeepgramPayload{}, ResultType: AgentStartLegResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_agent_elevenlabs", Tag: "Rooms", Section: "AI agents", Summary: "Attach an ElevenLabs Conversational AI agent to a room", PayloadType: agentElevenLabsPayload{}, ResultType: AgentStartRoomResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_agent_vapi", Tag: "Rooms", Section: "AI agents", Summary: "Attach a VAPI agent to a room", PayloadType: agentVAPIPayload{}, ResultType: AgentStartRoomResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "room_agent_pipecat", Tag: "Rooms", Section: "AI agents", Summary: "Attach a Pipecat bot to a room", PayloadType: agentPipecatPayload{}, ResultType: AgentStartRoomResult{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "room_agent_deepgram", Tag: "Rooms", Section: "AI agents", Summary: "Attach a Deepgram Voice Agent to a room", PayloadType: agentDeepgramPayload{}, ResultType: AgentStartRoomResult{}, ErrorCodes: []int{400, 404, 409, 503}},
		{Name: "leg_agent_stop", Tag: "Legs", Section: "AI agents", Summary: "Detach the agent attached to a leg", PayloadType: idPayload{}, ResultType: AgentStopResult{}, ErrorCodes: []int{404}},
		{Name: "room_agent_stop", Tag: "Rooms", Section: "AI agents", Summary: "Detach the agent attached to a room", PayloadType: idPayload{}, ResultType: AgentStopResult{}, ErrorCodes: []int{404}},
		{Name: "leg_agent_message", Tag: "Legs", Section: "AI agents", Summary: "Inject a text message into a leg agent session", PayloadType: agentMessagePayload{}, ResultType: AgentMessageResult{}, ErrorCodes: []int{400, 404, 409, 500, 501}},
		{Name: "room_agent_message", Tag: "Rooms", Section: "AI agents", Summary: "Inject a text message into a room agent session", PayloadType: agentMessagePayload{}, ResultType: AgentMessageResult{}, ErrorCodes: []int{400, 404, 409, 500, 501}},

		// ── SIP Registrations ───────────────────────────────────────────
		{Name: "list_sip_registrations", Tag: "SIP Registrations", Summary: "List active SIP AOR registrations", ResultType: RegistrationsResponse{}},
		{Name: "delete_sip_registration", Tag: "SIP Registrations", Summary: "Force-unbind an AOR (or a single contact under it)", PayloadType: deleteRegistrationPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},

		// ── Inbound auth (digest challenge) ─────────────────────────────
		{Name: "challenge_leg", Tag: "Legs", Section: "Inbound calls", Summary: "Send a 401 digest challenge on a ringing inbound SIP leg (INVITE)", PayloadType: challengeLegPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404, 409}},
		{Name: "challenge_registration", Tag: "SIP Registrations", Summary: "Send a 401 digest challenge for a parked inbound REGISTER attempt", PayloadType: challengeRegistrationPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{400, 404}},
		{Name: "accept_registration", Tag: "SIP Registrations", Summary: "Accept a parked inbound REGISTER attempt (bind and reply 200 OK)", PayloadType: acceptRegistrationPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
		{Name: "reject_registration", Tag: "SIP Registrations", Summary: "Reject a parked inbound REGISTER attempt (reply 403 by default)", PayloadType: rejectRegistrationPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},

		// ── SIP Trunks ──────────────────────────────────────────────────
		{Name: "create_sip_trunk", Tag: "SIP Trunks", Summary: "Create a SIP trunk (REGISTER or static peering)", PayloadType: CreateTrunkRequest{}, ResultType: CreateTrunkResponse{}, ErrorCodes: []int{400}},
		{Name: "list_sip_trunks", Tag: "SIP Trunks", Summary: "List configured SIP trunks", ResultType: TrunksListResponse{}},
		{Name: "get_sip_trunk", Tag: "SIP Trunks", Summary: "Get a single SIP trunk", PayloadType: idPayload{}, ResultType: sipmod.TrunkView{}, ErrorCodes: []int{404}},
		{Name: "delete_sip_trunk", Tag: "SIP Trunks", Summary: "Remove a SIP trunk", PayloadType: idPayload{}, ResultType: vsiStatusResponse{}, ErrorCodes: []int{404}},
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
	Section  string // one of EventSections()
}

// EventSections lists, in display order, the sections events are grouped into.
func EventSections() []SectionMeta {
	return []SectionMeta{
		{"Call progress", "Ringing, answer, hang-up and failed asynchronous commands"},
		{"Mute, deafen & hold", "Changes to a leg's mute, deaf and hold state"},
		{"Transfer", "SIP REFER transfers sent or received"},
		{"WebRTC", "ICE connectivity of WebRTC legs"},
		{"Audio streams", "Additional audio streams of a multi-stream call"},
		{"Rooms & routing", "Room membership, bridges, routing roles and the routing matrix"},
		{"DTMF & real-time text", "DTMF digits and T.140 text received from the remote"},
		{"Playback & TTS", "Audio file and synthesized speech playback"},
		{"Recording & SIPREC", "Local recordings and SIPREC sessions"},
		{"Speech & AMD", "Speaking detection, transcripts and answering machine detection"},
		{"AI agents", "Conversational AI agent sessions"},
		{"SIP registrations & trunks", "Inbound registrations and outbound trunk state"},
	}
}

// EventsMetadata returns every event published over the bus. asyncapi-gen
// emits one channel per entry; openapi-gen emits one x-webhooks entry per
// entry. Keeping a single list ensures the two specs cannot drift apart.
func EventsMetadata() []EventMeta {
	return []EventMeta{
		{events.LegRinging, "SIP call ringing (inbound or outbound)", reflect.TypeOf(events.LegRingingData{}), "Call progress"},
		{events.LegEarlyMedia, "Outbound leg received 183 Session Progress with SDP; media pipeline active", reflect.TypeOf(events.LegEarlyMediaData{}), "Call progress"},
		{events.LegConnected, "Leg answered/connected", reflect.TypeOf(events.LegConnectedData{}), "Call progress"},
		{events.LegDisconnected, "Leg hung up (CDR-style nested structure)", reflect.TypeOf(events.LegDisconnectedData{}), "Call progress"},
		{events.LegJoinedRoom, "Leg added to a room", reflect.TypeOf(events.LegJoinedRoomData{}), "Rooms & routing"},
		{events.LegLeftRoom, "Leg removed from a room", reflect.TypeOf(events.LegLeftRoomData{}), "Rooms & routing"},
		{events.LegMuted, "Leg muted", reflect.TypeOf(events.LegMutedData{}), "Mute, deafen & hold"},
		{events.LegUnmuted, "Leg unmuted", reflect.TypeOf(events.LegUnmutedData{}), "Mute, deafen & hold"},
		{events.LegDeaf, "Leg deafened (stops receiving room audio)", reflect.TypeOf(events.LegDeafData{}), "Mute, deafen & hold"},
		{events.LegUndeaf, "Leg undeafened (resumes receiving room audio)", reflect.TypeOf(events.LegUndeafData{}), "Mute, deafen & hold"},
		{events.LegHold, "Leg put on hold (local or remote)", reflect.TypeOf(events.LegHoldData{}), "Mute, deafen & hold"},
		{events.LegUnhold, "Leg taken off hold (local or remote)", reflect.TypeOf(events.LegUnholdData{}), "Mute, deafen & hold"},
		{events.LegCommandFailed, "An asynchronous leg command (202 Accepted) failed during execution", reflect.TypeOf(events.LegCommandFailedData{}), "Call progress"},
		{events.LegICEInterrupted, "WebRTC leg lost ICE connectivity; the leg is kept so the peer can restart ICE", reflect.TypeOf(events.LegICEInterruptedData{}), "WebRTC"},
		{events.LegICERestored, "WebRTC leg regained ICE connectivity after leg.ice_interrupted", reflect.TypeOf(events.LegICERestoredData{}), "WebRTC"},
		{events.LegStreamAdded, "An additional m=audio stream was negotiated on a live dialog", reflect.TypeOf(events.LegStreamData{}), "Audio streams"},
		{events.LegStreamRemoved, "An audio stream was disabled with a port-0 re-INVITE; its m-line slot survives as a tombstone", reflect.TypeOf(events.LegStreamData{}), "Audio streams"},
		{events.LegStreamRejected, "The peer refused an additional audio stream, or it could not be negotiated; the call is unaffected", reflect.TypeOf(events.LegStreamData{}), "Audio streams"},
		{events.LegStreamFailed, "An audio stream's media loop failed and the stream was torn down; the call continues on its remaining streams", reflect.TypeOf(events.LegStreamData{}), "Audio streams"},
		{events.LegStreamRoomChanged, "An audio stream was attached to or detached from a room (an empty room_id means detached)", reflect.TypeOf(events.LegStreamData{}), "Audio streams"},
		{events.LegStreamRoleChanged, "An audio stream's routing role changed; the room's allow-sets were recomputed atomically", reflect.TypeOf(events.LegStreamData{}), "Audio streams"},
		{events.SIPRECSessionStarted, "An inbound SIPREC recording session was accepted; carries the participants and the stream-to-participant bindings", reflect.TypeOf(events.SIPRECSessionStartedData{}), "Recording & SIPREC"},
		{events.SIPRECSessionEnded, "A SIPREC recording session ended", reflect.TypeOf(events.SIPRECSessionEndedData{}), "Recording & SIPREC"},
		{events.SIPRECMetadataUpdated, "A SIPREC recording session's metadata document was updated on a re-INVITE", reflect.TypeOf(events.SIPRECMetadataUpdatedData{}), "Recording & SIPREC"},
		{events.SIPRECParticipantJoined, "A party joined the call being recorded by a SIPREC session", reflect.TypeOf(events.SIPRECParticipantJoinedData{}), "Recording & SIPREC"},
		{events.SIPRECParticipantLeft, "A party left the call being recorded by a SIPREC session", reflect.TypeOf(events.SIPRECParticipantLeftData{}), "Recording & SIPREC"},
		{events.DTMFReceived, "DTMF digit received", reflect.TypeOf(events.DTMFReceivedData{}), "DTMF & real-time text"},
		{events.RTTReceived, "Real-Time Text (T.140 / RFC 4103) chunk received from the remote", reflect.TypeOf(events.RTTReceivedData{}), "DTMF & real-time text"},
		{events.SpeakingStarted, "Participant started speaking", reflect.TypeOf(events.SpeakingData{}), "Speech & AMD"},
		{events.SpeakingStopped, "Participant stopped speaking", reflect.TypeOf(events.SpeakingData{}), "Speech & AMD"},
		{events.PlaybackStarted, "Playback began", reflect.TypeOf(events.PlaybackStartedData{}), "Playback & TTS"},
		{events.PlaybackFinished, "Playback ended", reflect.TypeOf(events.PlaybackFinishedData{}), "Playback & TTS"},
		{events.PlaybackError, "Playback failed", reflect.TypeOf(events.PlaybackErrorData{}), "Playback & TTS"},
		{events.TTSStarted, "TTS synthesis began playing", reflect.TypeOf(events.TTSStartedData{}), "Playback & TTS"},
		{events.TTSFinished, "TTS synthesis finished playing", reflect.TypeOf(events.TTSFinishedData{}), "Playback & TTS"},
		{events.TTSError, "TTS synthesis or playback failed", reflect.TypeOf(events.TTSErrorData{}), "Playback & TTS"},
		{events.TTSStaged, "Preflight TTS finished synthesizing and is ready to commit", reflect.TypeOf(events.TTSStagedData{}), "Playback & TTS"},
		{events.TTSDiscarded, "Staged TTS was dropped without being played", reflect.TypeOf(events.TTSDiscardedData{}), "Playback & TTS"},
		{events.RecordingStarted, "Recording began", reflect.TypeOf(events.RecordingStartedData{}), "Recording & SIPREC"},
		{events.RecordingFinished, "Recording ended", reflect.TypeOf(events.RecordingFinishedData{}), "Recording & SIPREC"},
		{events.RecordingPaused, "Recording paused (audio replaced with silence)", reflect.TypeOf(events.RecordingPausedData{}), "Recording & SIPREC"},
		{events.RecordingResumed, "Recording resumed from a paused state", reflect.TypeOf(events.RecordingResumedData{}), "Recording & SIPREC"},
		{events.LegTransferInitiated, "We sent a SIP REFER (transfer initiated by the operator)", reflect.TypeOf(events.LegTransferInitiatedData{}), "Transfer"},
		{events.LegTransferRequested, "We received a SIP REFER from the peer; decide via accept_transfer/decline_transfer (unless SIP_REFER_AUTO_DIAL=true)", reflect.TypeOf(events.LegTransferRequestedData{}), "Transfer"},
		{events.LegTransferProgress, "Transfer progress reported via NOTIFY sipfrag", reflect.TypeOf(events.LegTransferProgressData{}), "Transfer"},
		{events.LegTransferCompleted, "Transfer reached terminal 2xx", reflect.TypeOf(events.LegTransferCompletedData{}), "Transfer"},
		{events.LegTransferFailed, "Transfer failed (REFER rejected, sipfrag non-2xx, or local error)", reflect.TypeOf(events.LegTransferFailedData{}), "Transfer"},
		{events.RoomCreated, "Room created", reflect.TypeOf(events.RoomCreatedData{}), "Rooms & routing"},
		{events.RoomDeleted, "Room deleted", reflect.TypeOf(events.RoomDeletedData{}), "Rooms & routing"},
		{events.RoomBridged, "Two rooms' mixers were joined", reflect.TypeOf(events.RoomBridgedData{}), "Rooms & routing"},
		{events.RoomBridgeUpdated, "A bridge's audio flow direction changed", reflect.TypeOf(events.RoomBridgeUpdatedData{}), "Rooms & routing"},
		{events.RoomUnbridged, "A bridge was torn down", reflect.TypeOf(events.RoomUnbridgedData{}), "Rooms & routing"},
		{events.RoomRoutingChanged, "The room's audio routing matrix changed", reflect.TypeOf(events.RoomRoutingChangedData{}), "Rooms & routing"},
		{events.LegRoleChanged, "A leg's routing role changed", reflect.TypeOf(events.LegRoleChangedData{}), "Rooms & routing"},
		{events.STTText, "Speech-to-text transcript", reflect.TypeOf(events.STTTextData{}), "Speech & AMD"},
		{events.STTTurn, "Speech-to-text turn boundary", reflect.TypeOf(events.STTTurnData{}), "Speech & AMD"},
		{events.AgentConnected, "Agent connected to provider", reflect.TypeOf(events.AgentConnectedData{}), "AI agents"},
		{events.AgentDisconnected, "Agent session ended", reflect.TypeOf(events.AgentDisconnectedData{}), "AI agents"},
		{events.AgentUserTranscript, "User speech transcribed by agent", reflect.TypeOf(events.AgentTranscriptData{}), "AI agents"},
		{events.AgentAgentResponse, "Agent generated a response", reflect.TypeOf(events.AgentResponseData{}), "AI agents"},
		{events.AMDResult, "Answering machine detection completed", reflect.TypeOf(events.AMDResultData{}), "Speech & AMD"},
		{events.AMDBeep, "Voicemail beep tone detected after machine classification", reflect.TypeOf(events.AMDBeepData{}), "Speech & AMD"},
		{events.SIPRegistrationAttempt, "Inbound REGISTER surfaced for a challenge/accept/reject decision (auto-accepts on consult timeout)", reflect.TypeOf(events.SIPRegistrationAttemptData{}), "SIP registrations & trunks"},
		{events.SIPRegistrationActive, "SIP AOR registration created or refreshed (one event per Contact)", reflect.TypeOf(events.SIPRegistrationActiveData{}), "SIP registrations & trunks"},
		{events.SIPRegistrationExpired, "SIP AOR registration removed (TTL, explicit unregister, force-delete, or single-binding replacement)", reflect.TypeOf(events.SIPRegistrationExpiredData{}), "SIP registrations & trunks"},
		{events.SIPOutboundRegistrationActive, "Outbound SIP trunk REGISTER accepted (initial or refresh)", reflect.TypeOf(events.SIPOutboundRegistrationActiveData{}), "SIP registrations & trunks"},
		{events.SIPOutboundRegistrationFailed, "Outbound SIP trunk REGISTER failed (transport error, non-2xx response, or digest auth rejected)", reflect.TypeOf(events.SIPOutboundRegistrationFailedData{}), "SIP registrations & trunks"},
		{events.SIPOutboundRegistrationExpired, "Outbound SIP trunk removed (DELETE, shutdown, or refresh failed past granted lifetime)", reflect.TypeOf(events.SIPOutboundRegistrationExpiredData{}), "SIP registrations & trunks"},
		{events.SIPTrunkUp, "SIP trunk peer answered its OPTIONS health check (first answer, or recovery after sip.trunk_down)", reflect.TypeOf(events.SIPTrunkStatusData{}), "SIP registrations & trunks"},
		{events.SIPTrunkDown, "SIP trunk peer failed its OPTIONS health check (no reply, transport error, 408, or 5xx other than 501)", reflect.TypeOf(events.SIPTrunkStatusData{}), "SIP registrations & trunks"},
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
		{Name: "ping", Direction: "send", Description: "Periodic application-level keepalive (every ~30 s), carrying a monotonic per-connection counter in `seq`. Clients may reply with `pong` or ignore."},
		{Name: "pong", Direction: "receive", Description: "Optional client keepalive reply to a `ping`. Currently a no-op on the server side."},
		{Name: "stop", Direction: "receive", Description: "Client-initiated graceful close. The server stops the recv loop and closes the connection."},
		{Name: "events_dropped", Direction: "send", Description: "Sent when the per-connection event buffer overflowed. The accompanying `count` indicates how many events were dropped since the last notification. Clients should resync via REST after seeing this."},
		{Name: "error", Direction: "send", Description: "Generic error frame for invalid JSON, unknown command types, or command handler failures. Echoes `request_id` when the offending message had one. `data` carries `{code, message}`."},
	}
}
