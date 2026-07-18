package events

import "github.com/VoiceBlender/voiceblender/internal/recording"

// EventData is the interface all typed event data structs must implement.
type EventData interface {
	GetLegID() string
	GetRoomID() string
	GetAppID() string
}

// LegScope embeds in events scoped to a single leg.
type LegScope struct {
	LegID string `json:"leg_id"`
	AppID string `json:"app_id,omitempty"`
}

func (b LegScope) GetLegID() string  { return b.LegID }
func (b LegScope) GetRoomID() string { return "" }
func (b LegScope) GetAppID() string  { return b.AppID }

// RoomScope embeds in events scoped to a single room.
type RoomScope struct {
	RoomID string `json:"room_id"`
	AppID  string `json:"app_id,omitempty"`
}

func (b RoomScope) GetLegID() string  { return "" }
func (b RoomScope) GetRoomID() string { return b.RoomID }
func (b RoomScope) GetAppID() string  { return b.AppID }

// LegRoomScope embeds in events that may target a leg, a room, or both.
type LegRoomScope struct {
	LegID  string `json:"leg_id,omitempty"`
	RoomID string `json:"room_id,omitempty"`
	AppID  string `json:"app_id,omitempty"`
}

func (b LegRoomScope) GetLegID() string  { return b.LegID }
func (b LegRoomScope) GetRoomID() string { return b.RoomID }
func (b LegRoomScope) GetAppID() string  { return b.AppID }

// --- Leg lifecycle events ---

type LegRingingData struct {
	LegScope
	LegType       string            `json:"leg_type,omitempty"`
	URI           string            `json:"uri,omitempty"`
	From          string            `json:"from,omitempty"`
	To            string            `json:"to,omitempty"`
	SIPHeaders    map[string]string `json:"sip_headers,omitempty"`
	OfferedCodecs []OfferedCodec    `json:"offered_codecs,omitempty"`
	// TrunkID identifies the trunk (outbound SIP registration) that delivered
	// the call. Set on inbound INVITEs whose source socket matches a known
	// trunk's registrar; populated on outbound legs whose From matches a
	// registered AOR. Empty otherwise.
	TrunkID string `json:"trunk_id,omitempty"`
	// SourceAddress is the host:port the INVITE actually arrived on
	// (inbound legs only). Useful for diagnostics when the peer's Via /
	// Contact differs from the transport-layer source, e.g. behind NAT.
	SourceAddress string `json:"source_address,omitempty"`
	// Authenticated is true when this inbound INVITE carried digest
	// credentials that VoiceBlender verified against a prior challenge.
	Authenticated bool `json:"authenticated,omitempty"`
	// AuthUsername is the username from the verified digest credentials
	// (set only when Authenticated is true).
	AuthUsername string `json:"auth_username,omitempty"`
}

// OfferedCodec describes one codec from a remote SIP offer SDP.
// Priority is 1-based and reflects the order the codec appeared in the m= line.
type OfferedCodec struct {
	Name        string `json:"name"`
	PayloadType uint8  `json:"payload_type"`
	ClockRate   int    `json:"clock_rate"`
	Priority    int    `json:"priority"`
}

type LegConnectedData struct {
	LegScope
	LegType string `json:"leg_type"`
}

type LegEarlyMediaData struct {
	LegScope
	LegType string `json:"leg_type"`
}

type LegMutedData struct {
	LegScope
}

type LegUnmutedData struct {
	LegScope
}

type LegDeafData struct {
	LegScope
}

type LegUndeafData struct {
	LegScope
}

type LegHoldData struct {
	LegScope
	LegType string `json:"leg_type"`
}

type LegUnholdData struct {
	LegScope
	LegType string `json:"leg_type"`
}

// LegCommandFailedData is emitted when an asynchronous leg command (one that
// runs on a goroutine after the HTTP handler has returned 202) fails. The
// command field identifies the action that failed, e.g. "hold", "transfer",
// "ring", "early_media", "hangup".
type LegCommandFailedData struct {
	LegScope
	Command string `json:"command"`
	Error   string `json:"error"`
}

// --- Transfer (SIP REFER) ---

// LegTransferInitiatedData fires after we successfully send a REFER request
// to a leg's peer (202 Accepted received).
type LegTransferInitiatedData struct {
	LegScope
	Kind          string `json:"kind"` // "blind" or "attended"
	Target        string `json:"target"`
	ReplacesLegID string `json:"replaces_leg_id,omitempty"`
}

// LegTransferRequestedData fires when a peer sends us a REFER targeting one of
// our legs. In the default app-driven model (SIP_REFER_AUTO_DIAL=false) this is
// a decision request: the REFER is parked and the app responds via
// accept_transfer / decline_transfer (then progress_transfer / complete_transfer)
// keyed by LegID; the actual outcome flows through leg.transfer_completed /
// leg.transfer_failed. With SIP_REFER_AUTO_DIAL=true the server accepts and
// originates the target itself. Declined is vestigial (always false) and
// retained only for wire compatibility.
type LegTransferRequestedData struct {
	LegScope
	Kind           string `json:"kind"`
	Target         string `json:"target"`
	ReplacesCallID string `json:"replaces_call_id,omitempty"`
	Declined       bool   `json:"declined"`
}

// LegTransferProgressData fires for each NOTIFY sipfrag we receive from the
// transferee while it executes a transfer we initiated.
type LegTransferProgressData struct {
	LegScope
	StatusCode int    `json:"status_code"`
	Reason     string `json:"reason,omitempty"`
}

// LegTransferCompletedData fires once a transfer reaches a terminal 2xx state.
type LegTransferCompletedData struct {
	LegScope
	StatusCode int    `json:"status_code"`
	Reason     string `json:"reason,omitempty"`
}

// LegTransferFailedData fires when a transfer ends in a non-2xx terminal
// state, when the REFER itself is rejected, or when the implicit
// subscription expires without a final NOTIFY.
type LegTransferFailedData struct {
	LegScope
	StatusCode int    `json:"status_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

// --- leg.disconnected with CDR-style nesting ---

type LegDisconnectedData struct {
	LegScope
	CDR     CallCDR      `json:"cdr"`
	Quality *CallQuality `json:"quality,omitempty"`
}

type CallCDR struct {
	Reason           string  `json:"reason"`
	DurationTotal    float64 `json:"duration_total"`
	DurationAnswered float64 `json:"duration_answered"`
}

type CallQuality struct {
	MOSScore        float64 `json:"mos_score"`
	PacketsReceived uint32  `json:"rtp_packets_received"`
	PacketsLost     uint32  `json:"rtp_packets_lost"`
	JitterMs        float64 `json:"rtp_jitter_ms"`
}

// --- Room lifecycle events ---

type RoomCreatedData struct {
	RoomScope
}

type RoomDeletedData struct {
	RoomScope
}

// BridgeScope embeds in events scoped to a bridge joining two rooms.
// GetRoomID returns RoomAID so existing room-scoped event filtering still
// matches one side of the bridge; both room IDs are always present.
type BridgeScope struct {
	BridgeID string `json:"bridge_id"`
	RoomAID  string `json:"room_a_id"`
	RoomBID  string `json:"room_b_id"`
	AppID    string `json:"app_id,omitempty"`
}

func (b BridgeScope) GetLegID() string  { return "" }
func (b BridgeScope) GetRoomID() string { return b.RoomAID }
func (b BridgeScope) GetAppID() string  { return b.AppID }

// RoomBridgedData fires when two rooms' mixers are joined. Direction is
// canonical relative to room_a_id: bidirectional | a_to_b | b_to_a | none.
type RoomBridgedData struct {
	BridgeScope
	Direction string `json:"direction"`
}

type RoomBridgeUpdatedData struct {
	BridgeScope
	Direction string `json:"direction"`
}

// RoomUnbridgedData fires when a bridge is torn down. Reason is empty for an
// explicit delete, or "room_deleted" when triggered by deleting a room.
type RoomUnbridgedData struct {
	BridgeScope
	Reason string `json:"reason,omitempty"`
}

// RoomRoutingChangedData fires whenever the room's audio routing matrix
// changes. Matrix is the full post-change matrix (listener role → source
// roles). Reason narrows the trigger: "set", "update", "leg_joined",
// "leg_left", "leg_role_changed".
type RoomRoutingChangedData struct {
	RoomScope
	Matrix map[string][]string `json:"matrix"`
	Reason string              `json:"reason"`
}

// LegRoleChangedData fires when a leg's routing role changes. RoomID is
// empty when the leg is not in a room.
type LegRoleChangedData struct {
	LegRoomScope
	OldRole string `json:"old_role"`
	NewRole string `json:"new_role"`
}

type LegJoinedRoomData struct {
	LegRoomScope
}

type LegLeftRoomData struct {
	LegRoomScope
}

type SpeakingData struct {
	LegRoomScope
}

// --- DTMF ---

type DTMFReceivedData struct {
	LegScope
	Digit string `json:"digit"`
	Seq   uint64 `json:"seq"`
}

// --- RTT (Real-Time Text, ITU-T T.140 / RFC 4103) ---

// RTTReceivedData is emitted whenever a SIP leg receives a chunk of T.140
// text from the remote UA. Text may be an arbitrary UTF-8 string (single
// character, several characters, or control codes such as backspace).
// LossMarker is true when a U+FFFD has been prepended to indicate that
// preceding text was lost beyond what RFC 2198 redundancy could recover.
type RTTReceivedData struct {
	LegScope
	Text       string `json:"text"`
	Seq        uint64 `json:"seq"`
	LossMarker bool   `json:"loss_marker,omitempty"`
}

// --- Playback ---

type PlaybackStartedData struct {
	LegRoomScope
	PlaybackID string `json:"playback_id"`
}

type PlaybackFinishedData struct {
	LegRoomScope
	PlaybackID string `json:"playback_id"`
}

type PlaybackErrorData struct {
	LegRoomScope
	PlaybackID string `json:"playback_id"`
	Error      string `json:"error"`
}

// --- TTS ---

type TTSStartedData struct {
	LegRoomScope
	TTSID string `json:"tts_id"`
}

type TTSFinishedData struct {
	LegRoomScope
	TTSID string `json:"tts_id"`
}

type TTSErrorData struct {
	LegRoomScope
	TTSID string `json:"tts_id"`
	Error string `json:"error"`
}

// --- Recording ---

type RecordingStartedData struct {
	LegRoomScope
	File string `json:"file"`
}

type RecordingFinishedData struct {
	LegRoomScope
	File             string                           `json:"file"`
	MultiChannelFile string                           `json:"multi_channel_file,omitempty"`
	Channels         map[string]recording.ChannelInfo `json:"channels,omitempty"`
	// OmittedLegs names participants whose audio is missing from the merged
	// file because their capture failed. Absent when the recording is complete.
	OmittedLegs []string `json:"omitted_legs,omitempty"`
}

type RecordingPausedData struct {
	LegRoomScope
	File string `json:"file"`
}

type RecordingResumedData struct {
	LegRoomScope
	File string `json:"file"`
}

// --- STT ---

type STTTextData struct {
	LegRoomScope
	Text    string `json:"text"`
	IsFinal bool   `json:"is_final"`
}

// --- Agent ---

type AgentConnectedData struct {
	LegRoomScope
	ConversationID string `json:"conversation_id"`
}

type AgentDisconnectedData struct {
	LegRoomScope
}

type AgentTranscriptData struct {
	LegRoomScope
	Text string `json:"text"`
}

type AgentResponseData struct {
	LegRoomScope
	Text string `json:"text"`
}

// AMDResultData is emitted when answering machine detection completes on an
// outbound call. Sent immediately when a determination is made.
type AMDResultData struct {
	LegScope
	Result             string `json:"result"`               // human, machine, no_speech, not_sure
	InitialSilenceMs   int    `json:"initial_silence_ms"`   // ms of silence before first speech
	GreetingDurationMs int    `json:"greeting_duration_ms"` // ms of speech in the greeting
	TotalAnalysisMs    int    `json:"total_analysis_ms"`    // total ms of analysis
}

// AMDBeepData is emitted when the voicemail beep tone is detected after a
// "machine" classification. Only sent when beep_timeout is configured.
type AMDBeepData struct {
	LegScope
	BeepMs int `json:"beep_ms"` // ms from machine detection to beep
}

// --- SIP registrations ---

// SIPRegistrationScope embeds in SIP registration events. Registrations are
// not scoped to a leg or room; AppID is optional and propagated from the
// most recent REGISTER context.
type SIPRegistrationScope struct {
	AppID string `json:"app_id,omitempty"`
}

func (s SIPRegistrationScope) GetLegID() string  { return "" }
func (s SIPRegistrationScope) GetRoomID() string { return "" }
func (s SIPRegistrationScope) GetAppID() string  { return s.AppID }

// SIPRegistrationAttemptData fires when an inbound REGISTER that would create
// or remove a binding is surfaced to the decision callback. A VSI/REST client
// may respond by challenging (401), accepting, or rejecting the attempt,
// referencing it by AttemptID. If no decision arrives before the consult
// timeout the REGISTER is auto-accepted.
type SIPRegistrationAttemptData struct {
	SIPRegistrationScope
	AttemptID        string `json:"attempt_id"`
	AOR              string `json:"aor"`
	Contact          string `json:"contact,omitempty"`
	SourceAddress    string `json:"source_address,omitempty"`
	Transport        string `json:"transport,omitempty"`
	UserAgent        string `json:"user_agent,omitempty"`
	CallID           string `json:"call_id,omitempty"`
	HasAuthorization bool   `json:"has_authorization,omitempty"`
}

// SIPRegistrationActiveData fires when a new AOR binding is added or an
// existing one is refreshed by a REGISTER request.
type SIPRegistrationActiveData struct {
	SIPRegistrationScope
	AOR                   string `json:"aor"`
	Contact               string `json:"contact"`
	Socket                string `json:"socket"`
	Transport             string `json:"transport"`
	UserAgent             string `json:"user_agent,omitempty"`
	CallID                string `json:"call_id,omitempty"`
	GrantedExpiresSeconds int    `json:"granted_expires_seconds"`
	ExpiresAt             string `json:"expires_at"`
}

// SIPRegistrationExpiredData fires when an AOR binding is removed. Reason
// is one of: "ttl" (TTL sweep), "unregistered" (explicit de-register from
// the UA), "forced" (operator DELETE), "replaced" (single-binding mode
// replaced a prior Contact).
type SIPRegistrationExpiredData struct {
	SIPRegistrationScope
	AOR     string `json:"aor"`
	Contact string `json:"contact"`
	Socket  string `json:"socket,omitempty"`
	Reason  string `json:"reason"`
}

// --- SIP outbound registrations (trunks) ---

// SIPOutboundRegistrationActiveData fires when a sip_register trunk
// successfully (re)registers with its upstream registrar. Re-emitted on every
// refresh so observers can see liveness.
type SIPOutboundRegistrationActiveData struct {
	SIPRegistrationScope
	TrunkID               string `json:"trunk_id"`
	AOR                   string `json:"aor"`
	Registrar             string `json:"registrar"`
	Contact               string `json:"contact"`
	GrantedExpiresSeconds int    `json:"granted_expires_seconds"`
	ExpiresAt             string `json:"expires_at"`
	CallID                string `json:"call_id,omitempty"`
	// SourceAddress is the actual host:port the 2xx response came from
	// (may differ from Registrar when DNS / a load balancer fronts it).
	SourceAddress string `json:"source_address,omitempty"`
}

// SIPOutboundRegistrationFailedData fires when a REGISTER attempt receives a
// non-2xx final response (after digest retry) or fails at the transport
// layer. The trunk is not removed; refresh continues with backoff.
type SIPOutboundRegistrationFailedData struct {
	SIPRegistrationScope
	TrunkID    string `json:"trunk_id"`
	AOR        string `json:"aor"`
	Registrar  string `json:"registrar"`
	StatusCode int    `json:"status_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

// SIPOutboundRegistrationExpiredData fires when a trunk is removed (DELETE
// or shutdown) or when refresh failed past the previously granted lifetime.
// Reason is one of: "unregistered", "refresh_failed", "shutdown".
type SIPOutboundRegistrationExpiredData struct {
	SIPRegistrationScope
	TrunkID   string `json:"trunk_id"`
	AOR       string `json:"aor"`
	Registrar string `json:"registrar"`
	Reason    string `json:"reason"`
}

// LiveKit (Model B): no special event types. Remote LK participants
// surface as LiveKitParticipantLeg entries in the umbrella's VB room, so
// their lifecycle is reported via the standard leg.connected /
// leg.disconnected / speaking.started / speaking.stopped events.
