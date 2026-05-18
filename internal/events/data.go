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

// LegTransferRequestedData fires when a peer sends us a REFER targeting one
// of our legs. Declined=true means we rejected it (default-deny when
// SIP_REFER_AUTO_DIAL=false); declined=false means we accepted (202) and
// will originate the new leg.
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
