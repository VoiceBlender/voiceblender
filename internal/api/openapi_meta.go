package api

// RouteMeta describes a single API endpoint for OpenAPI generation.
type RouteMeta struct {
	Method       string
	Path         string
	OperationID  string
	Summary      string
	Description  string
	Tags         []string
	RequestType  interface{} // nil or Go type instance (e.g. CreateLegRequest{})
	OptionalBody bool        // when true, requestBody is not marked required
	Responses    map[int]ResponseMeta
}

// ResponseMeta describes a single HTTP response for an endpoint.
type ResponseMeta struct {
	Description string
	Type        interface{} // nil, or Go type instance
}

// WebhookFieldDescriptions maps "event_type.field_name" → description for
// inline properties in x-webhooks entries.
func WebhookFieldDescriptions() map[string]string {
	return map[string]string{
		// leg.ringing
		"leg.ringing.leg_id":      "Leg identifier",
		"leg.ringing.leg_type":    "Leg type (e.g. sip_inbound, sip_outbound)",
		"leg.ringing.uri":         "Dialed SIP URI (outbound only)",
		"leg.ringing.from":        "Caller URI (inbound) or From header value (outbound, if set)",
		"leg.ringing.to":          "Callee URI (inbound only)",
		"leg.ringing.sip_headers": "X-* custom SIP headers, if present",

		// leg.early_media
		"leg.early_media.leg_id":   "Leg identifier",
		"leg.early_media.leg_type": "Leg type (e.g. sip_outbound)",

		// leg.connected
		"leg.connected.leg_id":   "Leg identifier",
		"leg.connected.leg_type": "Leg type (e.g. sip_inbound, sip_outbound, webrtc)",

		// leg.disconnected
		"leg.disconnected.leg_id": "Leg identifier",

		// leg.joined_room / left_room
		"leg.joined_room.leg_id":  "Leg identifier",
		"leg.joined_room.room_id": "Room identifier",
		"leg.left_room.leg_id":    "Leg identifier",
		"leg.left_room.room_id":   "Room identifier",

		// leg.muted / unmuted
		"leg.muted.leg_id":   "Leg identifier",
		"leg.unmuted.leg_id": "Leg identifier",

		// leg.hold / unhold
		"leg.hold.leg_id":     "Leg identifier",
		"leg.hold.leg_type":   `Hold direction: "local" (we put them on hold) or "remote" (they put us on hold)`,
		"leg.unhold.leg_id":   "Leg identifier",
		"leg.unhold.leg_type": `Hold direction: "local" or "remote"`,

		// dtmf.received
		"dtmf.received.leg_id": "Leg identifier",
		"dtmf.received.digit":  "DTMF digit received",

		// speaking
		"speaking.started.leg_id":  "Leg identifier",
		"speaking.started.room_id": "Room identifier (present only when the leg is in a room)",
		"speaking.stopped.leg_id":  "Leg identifier",
		"speaking.stopped.room_id": "Room identifier (present only when the leg is in a room)",

		// playback
		"playback.started.leg_id":       "Leg identifier",
		"playback.started.room_id":      "Room identifier",
		"playback.started.playback_id":  "Playback identifier",
		"playback.finished.leg_id":      "Leg identifier",
		"playback.finished.room_id":     "Room identifier",
		"playback.finished.playback_id": "Playback identifier",
		"playback.error.leg_id":         "Leg identifier",
		"playback.error.room_id":        "Room identifier",
		"playback.error.playback_id":    "Playback identifier",
		"playback.error.error":          "Error message",

		// tts
		"tts.started.leg_id":   "Leg identifier",
		"tts.started.room_id":  "Room identifier",
		"tts.started.tts_id":   "TTS playback identifier",
		"tts.finished.leg_id":  "Leg identifier",
		"tts.finished.room_id": "Room identifier",
		"tts.finished.tts_id":  "TTS playback identifier",
		"tts.error.leg_id":     "Leg identifier",
		"tts.error.room_id":    "Room identifier",
		"tts.error.tts_id":     "TTS playback identifier",
		"tts.error.error":      "Error message",

		// recording
		"recording.started.leg_id":   "Leg identifier",
		"recording.started.room_id":  "Room identifier",
		"recording.started.file":     "Recording file path or S3 URI",
		"recording.finished.leg_id":  "Leg identifier",
		"recording.finished.room_id": "Room identifier",
		"recording.finished.file":    "Recording file path or S3 URI",
		"recording.paused.leg_id":    "Leg identifier",
		"recording.paused.room_id":   "Room identifier",
		"recording.resumed.leg_id":   "Leg identifier",
		"recording.resumed.room_id":  "Room identifier",

		// room
		"room.created.room_id": "Room identifier",
		"room.deleted.room_id": "Room identifier",

		// transfer (SIP REFER)
		"leg.transfer_initiated.leg_id":           "Leg identifier",
		"leg.transfer_initiated.kind":             "Transfer kind: \"blind\" or \"attended\"",
		"leg.transfer_initiated.target":           "SIP URI to which the leg is being transferred",
		"leg.transfer_initiated.replaces_leg_id":  "Leg whose dialog is replaced (attended transfer only)",
		"leg.transfer_requested.leg_id":           "Leg identifier",
		"leg.transfer_requested.kind":             "Transfer kind: \"blind\" or \"attended\"",
		"leg.transfer_requested.target":           "SIP URI requested by the peer",
		"leg.transfer_requested.replaces_call_id": "Call-ID present in the Refer-To Replaces parameter (attended only)",
		"leg.transfer_requested.declined":         "True when the REFER was declined (e.g. SIP_REFER_AUTO_DIAL=false)",
		"leg.transfer_progress.leg_id":            "Leg identifier",
		"leg.transfer_progress.status_code":       "Provisional SIP status from the NOTIFY sipfrag",
		"leg.transfer_progress.reason":            "Reason phrase",
		"leg.transfer_completed.leg_id":           "Leg identifier",
		"leg.transfer_completed.status_code":      "Final 2xx SIP status from the NOTIFY sipfrag",
		"leg.transfer_completed.reason":           "Reason phrase",
		"leg.transfer_failed.leg_id":              "Leg identifier",
		"leg.transfer_failed.status_code":         "Final non-2xx SIP status (when applicable)",
		"leg.transfer_failed.reason":              "Reason phrase",
		"leg.transfer_failed.error":               "Local error message (when no SIP status applies)",

		// stt
		"stt.text.leg_id":   "Leg identifier",
		"stt.text.room_id":  "Room identifier",
		"stt.text.text":     "Transcribed text",
		"stt.text.is_final": "Whether this is a final or partial transcript",

		// agent
		"agent.connected.leg_id":          "Leg identifier",
		"agent.connected.room_id":         "Room identifier",
		"agent.connected.conversation_id": "Provider-assigned conversation identifier",
		"agent.disconnected.leg_id":       "Leg identifier",
		"agent.disconnected.room_id":      "Room identifier",
		"agent.user_transcript.leg_id":    "Leg identifier",
		"agent.user_transcript.room_id":   "Room identifier",
		"agent.user_transcript.text":      "User speech text",
		"agent.agent_response.leg_id":     "Leg identifier",
		"agent.agent_response.room_id":    "Room identifier",
		"agent.agent_response.text":       "Agent response text",

		"amd.result.leg_id":               "Leg identifier",
		"amd.result.result":               "Detection result: human, machine, no_speech, or not_sure",
		"amd.result.initial_silence_ms":   "Milliseconds of silence before first speech",
		"amd.result.greeting_duration_ms": "Milliseconds of speech in the greeting",
		"amd.result.total_analysis_ms":    "Total milliseconds of analysis before determination",

		"amd.beep.leg_id":  "Leg identifier",
		"amd.beep.beep_ms": "Milliseconds from machine detection to beep tone detection",
	}
}

// WebhookNestedFieldDescriptions provides descriptions for nested struct
// fields in webhook events (e.g. cdr.reason, cdr.duration_total).
func WebhookNestedFieldDescriptions() map[string]string {
	return map[string]string{
		"CallCDR.reason":                   "Disconnect reason. Common SIP failures are mapped to named reasons; unmapped 4xx/5xx/6xx codes appear as sip_{code}.",
		"CallCDR.duration_total":           "Seconds from leg creation to disconnect",
		"CallCDR.duration_answered":        "Seconds from answer to disconnect (0 if never answered)",
		"CallQuality.mos_score":            "Mean Opinion Score (1.0–5.0) estimated via simplified E-model (ITU-T G.107) from packet loss and jitter",
		"CallQuality.rtp_packets_received": "Total inbound RTP audio packets received",
		"CallQuality.rtp_packets_lost":     "Estimated lost packets based on sequence number gaps",
		"CallQuality.rtp_jitter_ms":        "Inter-arrival jitter in milliseconds (RFC 3550 §A.8)",
	}
}

// DisconnectReasonEnum lists all possible disconnect reason values.
var DisconnectReasonEnum = []string{
	"api_hangup", "remote_bye", "caller_cancel", "ring_timeout", "max_duration",
	"busy", "unavailable", "not_found", "forbidden", "unauthorized", "timeout",
	"cancelled", "not_acceptable", "service_unavailable", "declined",
	"rtp_timeout", "session_expired", "invite_failed", "connect_failed", "ice_failure",
}

// QualityDescription is the description for the quality object in leg.disconnected.
const QualityDescription = "RTP quality metrics. Omitted for WebRTC legs or unanswered legs with no media."

// RoutesMetadata returns the authoritative list of all API routes with their
// OpenAPI metadata. Used by cmd/openapi-gen to produce openapi.yaml.
func RoutesMetadata() []RouteMeta {
	return []RouteMeta{
		// ── Legs ────────────────────────────────────────────────────────
		{
			Method: "POST", Path: "/legs", OperationID: "createLeg",
			Summary:     "Originate an outbound SIP call",
			Tags:        []string{"Legs"},
			RequestType: CreateLegRequest{},
			Responses: map[int]ResponseMeta{
				201: {Description: "Leg created", Type: LegView{}},
				400: {Description: "Invalid JSON, bad SIP URI, unknown codec, or unsupported type"},
			},
		},
		{
			Method: "GET", Path: "/legs", OperationID: "listLegs",
			Summary: "List all active legs",
			Tags:    []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Array of legs", Type: []LegView{}},
			},
		},
		{
			Method: "GET", Path: "/legs/{id}", OperationID: "getLeg",
			Summary: "Get a single leg",
			Tags:    []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Leg details", Type: LegView{}},
				404: {Description: "Leg not found"},
			},
		},
		{
			Method: "DELETE", Path: "/legs/{id}", OperationID: "deleteLeg",
			Summary: "Hang up a leg",
			Tags:    []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Leg hung up"},
				404: {Description: "Leg not found"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/answer", OperationID: "answerLeg",
			Summary:      "Answer a ringing or early-media inbound SIP leg",
			Tags:         []string{"Legs"},
			RequestType:  AnswerLegRequest{},
			OptionalBody: true,
			Responses: map[int]ResponseMeta{
				200: {Description: "Answer initiated"},
				400: {Description: "Not a SIP inbound leg or invalid body"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg is not in ringing or early_media state"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/early-media", OperationID: "earlyMediaLeg",
			Summary: "Enable early media on a ringing inbound SIP leg",
			Description: "Sends SIP 183 Session Progress with SDP and sets up the media pipeline. " +
				"The leg transitions to `early_media` state, allowing audio playback and " +
				"room participation before the call is answered.",
			Tags: []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Early media enabled"},
				400: {Description: "Not a SIP inbound leg"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg is not in ringing state"},
				500: {Description: "Media setup failed"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/mute", OperationID: "muteLeg",
			Summary: "Mute a leg",
			Description: "A muted leg's audio is excluded from the room mix and speaking events " +
				"are suppressed. Taps (recording/STT) still receive the muted leg's own audio.",
			Tags: []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Leg muted"},
				404: {Description: "Leg not found"},
			},
		},
		{
			Method: "DELETE", Path: "/legs/{id}/mute", OperationID: "unmuteLeg",
			Summary: "Unmute a leg",
			Tags:    []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Leg unmuted"},
				404: {Description: "Leg not found"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/hold", OperationID: "holdLeg",
			Summary: "Put a SIP call on hold",
			Description: "Sends a re-INVITE with `sendonly` SDP direction. The RTP timeout is " +
				"paused while held, and a 2-hour auto-hangup timer starts.",
			Tags: []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Call held"},
				400: {Description: "Not a SIP leg"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg is not in connected state, or already held"},
			},
		},
		{
			Method: "DELETE", Path: "/legs/{id}/hold", OperationID: "unholdLeg",
			Summary:     "Resume a held SIP call",
			Description: "Sends a re-INVITE with `sendrecv` SDP direction.",
			Tags:        []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Call resumed"},
				400: {Description: "Not a SIP leg"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg is not held"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/transfer", OperationID: "transferLeg",
			Summary: "Transfer a SIP leg via REFER (asynchronous)",
			Description: "Asynchronously transfers a SIP leg. The HTTP call returns 202 as soon as the request is validated; " +
				"the REFER is sent in the background and its outcome is surfaced through `leg.transfer_initiated` / " +
				"`leg.transfer_progress` / `leg.transfer_completed` / `leg.transfer_failed` events. " +
				"Blind transfer when `replaces_leg_id` is omitted; attended transfer when present (the named leg's dialog " +
				"identity is embedded as a Replaces parameter per RFC 3891). On terminal 2xx the leg (and the replaces leg, " +
				"if any) is hung up automatically.",
			Tags:        []string{"Legs"},
			RequestType: TransferRequest{},
			Responses: map[int]ResponseMeta{
				202: {Description: "Transfer request accepted for processing"},
				400: {Description: "Missing or invalid target URI (including URIs without a host such as sip:)"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg not connected, not a SIP leg, or replaces_leg_id is invalid"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/dtmf", OperationID: "sendDTMF",
			Summary:     "Send DTMF digits on a leg",
			Tags:        []string{"Legs"},
			RequestType: DTMFRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Digits sent"},
				400: {Description: "Invalid JSON or empty digits"},
				404: {Description: "Leg not found"},
				500: {Description: "DTMF writer unavailable"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/dtmf/accept", OperationID: "acceptDTMFLeg",
			Summary: "Enable DTMF reception on a leg",
			Description: "Allow this leg to receive DTMF digits broadcast from other legs in the same room. " +
				"This is the default state for new legs.",
			Tags: []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "DTMF reception enabled"},
				404: {Description: "Leg not found"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/dtmf/reject", OperationID: "rejectDTMFLeg",
			Summary: "Disable DTMF reception on a leg",
			Description: "Block this leg from receiving DTMF digits broadcast from other legs in the same room. " +
				"DTMF received from this leg's own far end is still emitted as a leg.dtmf event.",
			Tags: []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "DTMF reception disabled"},
				404: {Description: "Leg not found"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/play", OperationID: "playLeg",
			Summary:     "Start audio playback to a leg",
			Tags:        []string{"Legs"},
			RequestType: PlaybackRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Playback started"},
				400: {Description: "Invalid JSON or volume out of range"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg has no audio writer"},
			},
		},
		{
			Method: "PATCH", Path: "/legs/{id}/play/{playbackID}", OperationID: "volumePlayLeg",
			Summary:     "Change the volume of an active leg playback",
			Tags:        []string{"Legs"},
			RequestType: VolumeRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Volume updated"},
				400: {Description: "Invalid JSON or volume out of range"},
				404: {Description: "Playback not found"},
			},
		},
		{
			Method: "DELETE", Path: "/legs/{id}/play/{playbackID}", OperationID: "stopPlayLeg",
			Summary: "Stop audio playback on a leg",
			Tags:    []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Playback stopped"},
				404: {Description: "No playback in progress"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/tts", OperationID: "ttsLeg",
			Summary: "Synthesize speech and play it on a leg",
			Description: "Synthesizes the provided text using the configured TTS provider and plays the audio on the leg. " +
				"When `TTS_CACHE_ENABLED=true`, identical requests (same text, voice, model, language, and prompt) are stored on disk in `TTS_CACHE_DIR` and persist across restarts, without calling the external provider.",
			Tags:        []string{"Legs"},
			RequestType: TTSRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "TTS playback started"},
				400: {Description: "Invalid JSON, missing text/voice, or volume out of range"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg has no audio writer"},
				503: {Description: "No API key provided for the selected provider"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/record", OperationID: "recordLeg",
			Summary: "Start recording a leg to a WAV file",
			Description: "For SIP legs, recording is stereo (left=incoming, right=outgoing). " +
				"For legs in a room, stereo at 16kHz (left=participant audio, right=mixed-minus-self).",
			Tags:         []string{"Legs"},
			RequestType:  RecordRequest{},
			OptionalBody: true,
			Responses: map[int]ResponseMeta{
				200: {Description: "Recording started"},
				400: {Description: "Invalid storage type, S3 not configured, or invalid S3 credentials"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg has no audio reader or room not found"},
				500: {Description: "Failed to create recording file"},
			},
		},
		{
			Method: "DELETE", Path: "/legs/{id}/record", OperationID: "stopRecordLeg",
			Summary: "Stop recording a leg",
			Tags:    []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Recording stopped"},
				404: {Description: "No recording in progress"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/record/pause", OperationID: "pauseRecordLeg",
			Summary: "Pause a leg recording",
			Description: "Replaces incoming audio with silence on the active recording until " +
				"`/record/resume` is called. The WAV's timeline is preserved (silent gap where " +
				"audio was paused), so reviewers can see exactly when sensitive data was excluded. " +
				"Idempotent: calling while already paused returns `status: already_paused`.",
			Tags: []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Recording paused (or already paused)"},
				404: {Description: "No recording in progress"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/record/resume", OperationID: "resumeRecordLeg",
			Summary: "Resume a paused leg recording",
			Description: "Resumes writing real audio after a prior `/record/pause`. Idempotent: " +
				"calling while not paused returns `status: not_paused`.",
			Tags: []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Recording resumed (or was not paused)"},
				404: {Description: "No recording in progress"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/stt", OperationID: "sttLeg",
			Summary:      "Start real-time speech-to-text on a leg",
			Tags:         []string{"Legs"},
			RequestType:  STTRequest{},
			OptionalBody: true,
			Responses: map[int]ResponseMeta{
				200: {Description: "STT started"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg not connected, STT already running, or no audio reader"},
				503: {Description: "No ElevenLabs API key provided"},
			},
		},
		{
			Method: "DELETE", Path: "/legs/{id}/stt", OperationID: "stopSTTLeg",
			Summary: "Stop speech-to-text on a leg",
			Tags:    []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "STT stopped"},
				404: {Description: "No STT in progress"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/agent/elevenlabs", OperationID: "agentLegElevenLabs",
			Summary:     "Attach an ElevenLabs ConvAI agent to a leg",
			Description: "Bridges audio bidirectionally with an ElevenLabs conversational AI agent. Standalone legs use direct audio; legs in a room use mixer taps.",
			Tags:        []string{"Legs"},
			RequestType: ElevenLabsAgentRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent started"},
				400: {Description: "Invalid JSON or missing agent_id"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg not connected, agent already attached, or no audio reader/writer"},
				503: {Description: "No ElevenLabs API key provided"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/agent/vapi", OperationID: "agentLegVAPI",
			Summary:     "Attach a VAPI agent to a leg",
			Description: "Bridges audio bidirectionally with a VAPI conversational AI agent. Standalone legs use direct audio; legs in a room use mixer taps.",
			Tags:        []string{"Legs"},
			RequestType: VAPIAgentRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent started"},
				400: {Description: "Invalid JSON or missing assistant_id"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg not connected, agent already attached, or no audio reader/writer"},
				503: {Description: "No VAPI API key provided"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/agent/pipecat", OperationID: "agentLegPipecat",
			Summary:     "Attach a Pipecat bot to a leg",
			Description: "Bridges audio bidirectionally with a self-hosted Pipecat bot via WebSocket. Standalone legs use direct audio; legs in a room use mixer taps.",
			Tags:        []string{"Legs"},
			RequestType: PipecatAgentRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent started"},
				400: {Description: "Invalid JSON or missing websocket_url"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg not connected, agent already attached, or no audio reader/writer"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/agent/deepgram", OperationID: "agentLegDeepgram",
			Summary:     "Attach a Deepgram Voice Agent to a leg",
			Description: "Bridges audio bidirectionally with a Deepgram Voice Agent. Standalone legs use direct audio; legs in a room use mixer taps.",
			Tags:        []string{"Legs"},
			RequestType: DeepgramAgentRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent started"},
				400: {Description: "Invalid JSON"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg not connected, agent already attached, or no audio reader/writer"},
				503: {Description: "No Deepgram API key provided"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/agent/message", OperationID: "agentLegMessage",
			Summary:     "Inject a message into a running agent session on a leg",
			Description: "Sends a context message or instruction to the running agent. Supported by Deepgram (InjectAgentMessage), Pipecat (TextFrame), and VAPI (control URL). Returns 501 for ElevenLabs.",
			Tags:        []string{"Legs"},
			RequestType: AgentMessageRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Message sent"},
				400: {Description: "Invalid JSON or missing message"},
				404: {Description: "No agent attached to this leg"},
				409: {Description: "Agent session not running"},
				501: {Description: "Provider does not support message injection"},
			},
		},
		{
			Method: "DELETE", Path: "/legs/{id}/agent", OperationID: "stopAgentLeg",
			Summary: "Detach the agent from a leg",
			Tags:    []string{"Legs"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent stopped"},
				404: {Description: "No agent attached to this leg"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/amd", OperationID: "startAMDLeg",
			Summary:     "Start answering machine detection on a connected leg",
			Tags:        []string{"Legs"},
			RequestType: AMDParams{},
			Responses: map[int]ResponseMeta{
				200: {Description: "AMD started"},
				400: {Description: "Invalid AMD params or not a SIP leg"},
				404: {Description: "Leg not found"},
				409: {Description: "Leg is not in connected state"},
			},
		},
		{
			Method: "POST", Path: "/legs/{id}/ice-candidates", OperationID: "addICECandidate",
			Summary: "Send a remote ICE candidate to a WebRTC leg (trickle ICE)",
			Tags:    []string{"WebRTC"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Candidate added"},
				400: {Description: "Invalid JSON or leg is not a WebRTC leg"},
				404: {Description: "Leg not found"},
				500: {Description: "Failed to add ICE candidate"},
			},
		},
		{
			Method: "GET", Path: "/legs/{id}/ice-candidates", OperationID: "getICECandidates",
			Summary: "Get server-side ICE candidates for a WebRTC leg (trickle ICE)",
			Tags:    []string{"WebRTC"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Buffered ICE candidates"},
				400: {Description: "Leg is not a WebRTC leg"},
				404: {Description: "Leg not found"},
			},
		},

		// ── Rooms ───────────────────────────────────────────────────────
		{
			Method: "POST", Path: "/rooms", OperationID: "createRoom",
			Summary:      "Create a room",
			Tags:         []string{"Rooms"},
			RequestType:  CreateRoomRequest{},
			OptionalBody: true,
			Responses: map[int]ResponseMeta{
				201: {Description: "Room created", Type: RoomView{}},
				409: {Description: "Room ID already exists"},
			},
		},
		{
			Method: "GET", Path: "/rooms", OperationID: "listRooms",
			Summary: "List all rooms with participants",
			Tags:    []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Array of rooms", Type: []RoomView{}},
			},
		},
		{
			Method: "GET", Path: "/rooms/{id}", OperationID: "getRoom",
			Summary: "Get a room with participants",
			Tags:    []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Room details", Type: RoomView{}},
				404: {Description: "Room not found"},
			},
		},
		{
			Method: "DELETE", Path: "/rooms/{id}", OperationID: "deleteRoom",
			Summary: "Delete a room",
			Tags:    []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Room deleted"},
				404: {Description: "Room not found"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/legs", OperationID: "addLegToRoom",
			Summary: "Add or move a leg to a room",
			Description: "Add a leg to a room (auto-creates room if it doesn't exist). " +
				"If the leg is already in a different room, it is atomically moved " +
				"to the target room. A ringing inbound SIP leg is automatically " +
				"answered before being added.",
			Tags:        []string{"Rooms"},
			RequestType: AddLegRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Leg added or moved"},
				400: {Description: "Invalid JSON, leg not found, or leg not connected"},
			},
		},
		{
			Method: "DELETE", Path: "/rooms/{id}/legs/{legID}", OperationID: "removeLegFromRoom",
			Summary: "Remove a leg from a room",
			Tags:    []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Leg removed"},
				400: {Description: "Room or leg not found"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/play", OperationID: "playRoom",
			Summary:     "Play audio to a room",
			Tags:        []string{"Rooms"},
			RequestType: PlaybackRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Playback started"},
				400: {Description: "Invalid JSON or volume out of range"},
				404: {Description: "Room not found"},
				409: {Description: "Room has no participants"},
			},
		},
		{
			Method: "PATCH", Path: "/rooms/{id}/play/{playbackID}", OperationID: "volumePlayRoom",
			Summary:     "Change the volume of an active room playback",
			Tags:        []string{"Rooms"},
			RequestType: VolumeRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Volume updated"},
				400: {Description: "Invalid JSON or volume out of range"},
				404: {Description: "Playback not found"},
			},
		},
		{
			Method: "DELETE", Path: "/rooms/{id}/play/{playbackID}", OperationID: "stopPlayRoom",
			Summary: "Stop room playback",
			Tags:    []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Playback stopped"},
				404: {Description: "No playback in progress"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/tts", OperationID: "ttsRoom",
			Summary: "Synthesize speech and play it into a room",
			Description: "Synthesizes the provided text using the configured TTS provider and plays the audio into the room. " +
				"When `TTS_CACHE_ENABLED=true`, identical requests (same text, voice, model, language, and prompt) are stored on disk in `TTS_CACHE_DIR` and persist across restarts, without calling the external provider.",
			Tags:        []string{"Rooms"},
			RequestType: TTSRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "TTS playback started"},
				400: {Description: "Invalid JSON, missing text/voice, or volume out of range"},
				404: {Description: "Room not found"},
				409: {Description: "Room has no participants"},
				503: {Description: "No API key provided for the selected provider"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/record", OperationID: "recordRoom",
			Summary:      "Start recording the room mix to a WAV file",
			Description:  "Records the full room mix at 16kHz, 16-bit, mono.",
			Tags:         []string{"Rooms"},
			RequestType:  RecordRequest{},
			OptionalBody: true,
			Responses: map[int]ResponseMeta{
				200: {Description: "Recording started"},
				400: {Description: "Invalid storage type, S3 not configured, or invalid S3 credentials"},
				404: {Description: "Room not found"},
				409: {Description: "Room has no participants"},
				500: {Description: "Failed to create recording file"},
			},
		},
		{
			Method: "DELETE", Path: "/rooms/{id}/record", OperationID: "stopRecordRoom",
			Summary: "Stop room recording",
			Tags:    []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Recording stopped"},
				404: {Description: "No recording in progress"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/record/pause", OperationID: "pauseRecordRoom",
			Summary: "Pause a room recording",
			Description: "Replaces the room mix with silence on the active recording until " +
				"`/record/resume` is called. When multi-channel recording is active, every " +
				"per-participant track is paused too (including tracks for participants who " +
				"join while paused). Idempotent: returns `status: already_paused` when already paused.",
			Tags: []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Recording paused (or already paused)"},
				404: {Description: "No recording in progress"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/record/resume", OperationID: "resumeRecordRoom",
			Summary: "Resume a paused room recording",
			Description: "Resumes writing real audio after a prior `/record/pause`. Resumes " +
				"every per-participant track if multi-channel recording is active. Idempotent.",
			Tags: []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Recording resumed (or was not paused)"},
				404: {Description: "No recording in progress"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/stt", OperationID: "sttRoom",
			Summary:      "Start speech-to-text on all room participants",
			Tags:         []string{"Rooms"},
			RequestType:  STTRequest{},
			OptionalBody: true,
			Responses: map[int]ResponseMeta{
				200: {Description: "STT started"},
				404: {Description: "Room not found"},
				409: {Description: "STT already running or room has no participants"},
				503: {Description: "No ElevenLabs API key provided"},
			},
		},
		{
			Method: "DELETE", Path: "/rooms/{id}/stt", OperationID: "stopSTTRoom",
			Summary: "Stop speech-to-text on a room",
			Tags:    []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "STT stopped"},
				404: {Description: "No STT in progress"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/agent/elevenlabs", OperationID: "agentRoomElevenLabs",
			Summary:     "Attach an ElevenLabs ConvAI agent to a room",
			Description: "The agent joins as a virtual participant, hearing all participants (mixed-minus-self) and speaking to everyone.",
			Tags:        []string{"Rooms"},
			RequestType: ElevenLabsAgentRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent started"},
				400: {Description: "Invalid JSON or missing agent_id"},
				404: {Description: "Room not found"},
				409: {Description: "Agent already attached to this room"},
				503: {Description: "No ElevenLabs API key provided"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/agent/vapi", OperationID: "agentRoomVAPI",
			Summary:     "Attach a VAPI agent to a room",
			Description: "The agent joins as a virtual participant, hearing all participants (mixed-minus-self) and speaking to everyone.",
			Tags:        []string{"Rooms"},
			RequestType: VAPIAgentRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent started"},
				400: {Description: "Invalid JSON or missing assistant_id"},
				404: {Description: "Room not found"},
				409: {Description: "Agent already attached to this room"},
				503: {Description: "No VAPI API key provided"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/agent/pipecat", OperationID: "agentRoomPipecat",
			Summary:     "Attach a Pipecat bot to a room",
			Description: "The bot joins as a virtual participant via WebSocket, hearing all participants (mixed-minus-self) and speaking to everyone.",
			Tags:        []string{"Rooms"},
			RequestType: PipecatAgentRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent started"},
				400: {Description: "Invalid JSON or missing websocket_url"},
				404: {Description: "Room not found"},
				409: {Description: "Agent already attached to this room"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/agent/deepgram", OperationID: "agentRoomDeepgram",
			Summary:     "Attach a Deepgram Voice Agent to a room",
			Description: "The agent joins as a virtual participant, hearing all participants (mixed-minus-self) and speaking to everyone.",
			Tags:        []string{"Rooms"},
			RequestType: DeepgramAgentRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent started"},
				400: {Description: "Invalid JSON"},
				404: {Description: "Room not found"},
				409: {Description: "Agent already attached to this room"},
				503: {Description: "No Deepgram API key provided"},
			},
		},
		{
			Method: "POST", Path: "/rooms/{id}/agent/message", OperationID: "agentRoomMessage",
			Summary:     "Inject a message into a running agent session on a room",
			Description: "Sends a context message or instruction to the running agent. Supported by Deepgram (InjectAgentMessage), Pipecat (TextFrame), and VAPI (control URL). Returns 501 for ElevenLabs.",
			Tags:        []string{"Rooms"},
			RequestType: AgentMessageRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "Message sent"},
				400: {Description: "Invalid JSON or missing message"},
				404: {Description: "No agent attached to this room"},
				409: {Description: "Agent session not running"},
				501: {Description: "Provider does not support message injection"},
			},
		},
		{
			Method: "DELETE", Path: "/rooms/{id}/agent", OperationID: "stopAgentRoom",
			Summary: "Detach the agent from a room",
			Tags:    []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				200: {Description: "Agent stopped"},
				404: {Description: "No agent attached to this room"},
			},
		},
		{
			Method: "GET", Path: "/rooms/{id}/ws", OperationID: "wsRoom",
			Summary: "WebSocket audio stream for a room",
			Description: "Upgrades to a WebSocket connection and joins the room as a bidirectional " +
				"audio participant. The client sends and receives 16kHz 16-bit signed " +
				"little-endian PCM audio (mono), base64-encoded in JSON text frames. " +
				"Each audio frame is 640 bytes (20ms).",
			Tags: []string{"Rooms"},
			Responses: map[int]ResponseMeta{
				101: {Description: "WebSocket upgrade successful. Server sends a `connected` message followed by mixed-minus-self audio frames."},
				404: {Description: "Room not found"},
			},
		},

		// ── Event Stream ────────────────────────────────────────────────
		{
			Method: "GET", Path: "/vsi", OperationID: "vsi",
			Summary: "VoiceBlender Streaming Interface (VSI)",
			Description: "Upgrades to a WebSocket connection and streams all events in real-time as " +
				"JSON text frames. The JSON shape is identical to webhook payloads. " +
				"The server sends a `{\"type\":\"connected\"}` message on connect, followed by events " +
				"and periodic `{\"type\":\"ping\"}` keepalives. " +
				"Clients may send `{\"type\":\"pong\"}` or `{\"type\":\"stop\"}` to close gracefully. " +
				"Unknown message types receive an error response with the echoed `request_id` (reserved for future commands).",
			Tags: []string{"Events"},
			Responses: map[int]ResponseMeta{
				101: {Description: "WebSocket upgrade successful. Server sends events as JSON text frames."},
			},
		},

		// ── WebRTC ──────────────────────────────────────────────────────
		{
			Method: "POST", Path: "/webrtc/offer", OperationID: "webrtcOffer",
			Summary:     "Establish a WebRTC leg via SDP offer/answer",
			Tags:        []string{"WebRTC"},
			RequestType: WebRTCOfferRequest{},
			Responses: map[int]ResponseMeta{
				200: {Description: "SDP answer with leg ID"},
				400: {Description: "Invalid JSON or invalid SDP offer"},
				500: {Description: "Peer connection, track creation, or answer generation failed"},
			},
		},
	}
}
