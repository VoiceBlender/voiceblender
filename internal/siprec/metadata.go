// Package siprec implements the SIPREC recording metadata model of RFC 7865:
// the XML document that binds each recorded media stream to the participant
// whose audio it carries.
//
// Element and attribute tags deliberately omit the namespace so documents that
// declare it with any prefix — or omit it entirely, as some SBCs do — still
// parse. Marshalling always emits the registered namespace.
package siprec

import "encoding/xml"

// Namespace is the RFC 7865 recording metadata namespace.
const Namespace = "urn:ietf:params:xml:ns:recording:1"

// Data modes (RFC 7865 §6.1). A complete document replaces the recording
// server's view; a partial one is merged into it.
const (
	DataModeComplete = "complete"
	DataModePartial  = "partial"
)

// Recording is the root metadata element.
type Recording struct {
	XMLName  xml.Name `xml:"recording"`
	Xmlns    string   `xml:"xmlns,attr,omitempty"`
	DataMode string   `xml:"datamode,omitempty"`

	Groups                 []Group                   `xml:"group"`
	Sessions               []Session                 `xml:"session"`
	SessionRecordingAssocs []SessionRecordingAssoc   `xml:"sessionrecordingassoc"`
	Participants           []Participant             `xml:"participant"`
	ParticipantSessions    []ParticipantSessionAssoc `xml:"participantsessionassoc"`
	Streams                []Stream                  `xml:"stream"`
	ParticipantStreams     []ParticipantStreamAssoc  `xml:"participantstreamassoc"`
}

// Group associates several communication sessions recorded together.
type Group struct {
	GroupID          string `xml:"group_id,attr"`
	AssociateTime    string `xml:"associate-time,omitempty"`
	DisassociateTime string `xml:"disassociate-time,omitempty"`
}

// Session is one recorded communication session.
type Session struct {
	SessionID string `xml:"session_id,attr"`
	GroupRef  string `xml:"group-ref,omitempty"`
	StartTime string `xml:"start-time,omitempty"`
	StopTime  string `xml:"stop-time,omitempty"`
}

// SessionRecordingAssoc ties a communication session to the recording session.
type SessionRecordingAssoc struct {
	SessionID        string `xml:"session_id,attr"`
	AssociateTime    string `xml:"associate-time,omitempty"`
	DisassociateTime string `xml:"disassociate-time,omitempty"`
}

// Name is a participant's display name, optionally language-tagged.
type Name struct {
	Lang  string `xml:"lang,attr,omitempty"`
	Value string `xml:",chardata"`
}

// NameID carries a participant's address of record and display name.
type NameID struct {
	AOR  string `xml:"aor,attr,omitempty"`
	Name *Name  `xml:"name,omitempty"`
}

// Participant is one party whose media is being recorded.
type Participant struct {
	ParticipantID string   `xml:"participant_id,attr"`
	SessionID     string   `xml:"session_id,attr,omitempty"`
	NameIDs       []NameID `xml:"nameID"`
}

// AOR returns the participant's first address of record, or "".
func (p *Participant) AOR() string {
	for _, n := range p.NameIDs {
		if n.AOR != "" {
			return n.AOR
		}
	}
	return ""
}

// DisplayName returns the participant's first non-empty display name, or "".
func (p *Participant) DisplayName() string {
	for _, n := range p.NameIDs {
		if n.Name != nil && n.Name.Value != "" {
			return n.Name.Value
		}
	}
	return ""
}

// ParticipantSessionAssoc records a participant joining or leaving a session.
// A non-empty DisassociateTime means the participant has left.
type ParticipantSessionAssoc struct {
	ParticipantID    string `xml:"participant_id,attr"`
	SessionID        string `xml:"session_id,attr,omitempty"`
	AssociateTime    string `xml:"associate-time,omitempty"`
	DisassociateTime string `xml:"disassociate-time,omitempty"`
}

// Stream is one recorded media stream. Label is what appears as a=label in the
// recording session's SDP, which is how a stream is matched to an m= section.
type Stream struct {
	StreamID  string `xml:"stream_id,attr"`
	SessionID string `xml:"session_id,attr,omitempty"`
	Label     string `xml:"label,omitempty"`
}

// ParticipantStreamAssoc binds a participant to the streams it sends and
// receives. Send and Recv hold stream_id values, not labels.
type ParticipantStreamAssoc struct {
	ParticipantID    string   `xml:"participant_id,attr"`
	Send             []string `xml:"send"`
	Recv             []string `xml:"recv"`
	AssociateTime    string   `xml:"associate-time,omitempty"`
	DisassociateTime string   `xml:"disassociate-time,omitempty"`
}
