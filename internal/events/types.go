package events

import "time"

type EventType string

const (
	LegRinging     EventType = "leg.ringing"
	LegConnected   EventType = "leg.connected"
	LegDisconnected EventType = "leg.disconnected"
	LegJoinedRoom  EventType = "leg.joined_room"
	LegLeftRoom    EventType = "leg.left_room"
	LegEarlyMedia  EventType = "leg.early_media"
	LegMuted       EventType = "leg.muted"
	LegUnmuted     EventType = "leg.unmuted"

	DTMFReceived EventType = "dtmf.received"

	PlaybackStarted  EventType = "playback.started"
	PlaybackFinished EventType = "playback.finished"
	PlaybackError    EventType = "playback.error"

	RecordingStarted  EventType = "recording.started"
	RecordingFinished EventType = "recording.finished"

	SpeakingStarted EventType = "speaking.started"
	SpeakingStopped EventType = "speaking.stopped"

	RoomCreated EventType = "room.created"
	RoomDeleted EventType = "room.deleted"

	STTText EventType = "stt.text"

	AgentConnected      EventType = "agent.connected"
	AgentDisconnected   EventType = "agent.disconnected"
	AgentUserTranscript EventType = "agent.user_transcript"
	AgentAgentResponse  EventType = "agent.agent_response"
)

type Event struct {
	Type      EventType              `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	Data      map[string]interface{} `json:"data"`
}
