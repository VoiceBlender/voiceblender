package events

import (
	"encoding/json"
	"time"
)

type EventType string

const (
	LegRinging      EventType = "leg.ringing"
	LegConnected    EventType = "leg.connected"
	LegDisconnected EventType = "leg.disconnected"
	LegJoinedRoom   EventType = "leg.joined_room"
	LegLeftRoom     EventType = "leg.left_room"
	LegEarlyMedia   EventType = "leg.early_media"
	LegMuted        EventType = "leg.muted"
	LegUnmuted      EventType = "leg.unmuted"
	LegDeaf         EventType = "leg.deaf"
	LegUndeaf       EventType = "leg.undeaf"
	LegHold         EventType = "leg.hold"
	LegUnhold       EventType = "leg.unhold"

	LegTransferInitiated EventType = "leg.transfer_initiated"
	LegTransferRequested EventType = "leg.transfer_requested"
	LegTransferProgress  EventType = "leg.transfer_progress"
	LegTransferCompleted EventType = "leg.transfer_completed"
	LegTransferFailed    EventType = "leg.transfer_failed"

	DTMFReceived EventType = "dtmf.received"

	PlaybackStarted  EventType = "playback.started"
	PlaybackFinished EventType = "playback.finished"
	PlaybackError    EventType = "playback.error"

	TTSStarted  EventType = "tts.started"
	TTSFinished EventType = "tts.finished"
	TTSError    EventType = "tts.error"

	RecordingStarted  EventType = "recording.started"
	RecordingFinished EventType = "recording.finished"
	RecordingPaused   EventType = "recording.paused"
	RecordingResumed  EventType = "recording.resumed"

	SpeakingStarted EventType = "speaking.started"
	SpeakingStopped EventType = "speaking.stopped"

	RoomCreated EventType = "room.created"
	RoomDeleted EventType = "room.deleted"

	STTText EventType = "stt.text"

	AgentConnected      EventType = "agent.connected"
	AgentDisconnected   EventType = "agent.disconnected"
	AgentUserTranscript EventType = "agent.user_transcript"
	AgentAgentResponse  EventType = "agent.agent_response"

	AMDResult EventType = "amd.result"
	AMDBeep   EventType = "amd.beep"
)

type Event struct {
	Type       EventType `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
	InstanceID string    `json:"instance_id,omitempty"`
	Data       EventData `json:"-"`
}

// MarshalJSON flattens the event envelope and data fields into a single JSON
// object, avoiding a generic "data" wrapper.
func (e Event) MarshalJSON() ([]byte, error) {
	envelope := map[string]interface{}{
		"type":      e.Type,
		"timestamp": e.Timestamp,
	}
	if e.InstanceID != "" {
		envelope["instance_id"] = e.InstanceID
	}

	// Marshal the typed data struct and merge its fields into the envelope.
	if e.Data != nil {
		dataBytes, err := json.Marshal(e.Data)
		if err != nil {
			return nil, err
		}
		var dataMap map[string]interface{}
		if err := json.Unmarshal(dataBytes, &dataMap); err != nil {
			return nil, err
		}
		for k, v := range dataMap {
			envelope[k] = v
		}
	}

	return json.Marshal(envelope)
}
