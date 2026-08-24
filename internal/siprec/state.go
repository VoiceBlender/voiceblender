package siprec

import (
	"sort"
	"sync"
)

// ParticipantInfo is the flattened view of one recorded participant.
type ParticipantInfo struct {
	ID   string `json:"id"`
	AOR  string `json:"aor,omitempty"`
	Name string `json:"name,omitempty"`
}

// Label returns the most human-meaningful identifier available.
func (p ParticipantInfo) Label() string {
	switch {
	case p.Name != "":
		return p.Name
	case p.AOR != "":
		return p.AOR
	default:
		return p.ID
	}
}

// StreamInfo is the flattened view of one recorded media stream, including the
// participant whose audio it carries.
type StreamInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id,omitempty"`
	Label     string `json:"label,omitempty"`

	// Participant is the party sending on this stream — the one whose audio
	// the SRS receives. Zero when the metadata has not bound it yet.
	Participant ParticipantInfo `json:"participant"`
}

// Delta reports what an applied document changed, so callers can publish
// participant join/leave events without diffing snapshots themselves.
type Delta struct {
	ParticipantsAdded   []string
	ParticipantsRemoved []string
	StreamsAdded        []string
	StreamsRemoved      []string
}

// Changed reports whether the document altered the state at all.
func (d Delta) Changed() bool {
	return len(d.ParticipantsAdded)+len(d.ParticipantsRemoved)+
		len(d.StreamsAdded)+len(d.StreamsRemoved) > 0
}

// Snapshot is the read model handed to the API and to events.
type Snapshot struct {
	SessionID    string            `json:"session_id,omitempty"`
	DataMode     string            `json:"data_mode,omitempty"`
	Participants []ParticipantInfo `json:"participants"`
	Streams      []StreamInfo      `json:"streams"`
	Metadata     string            `json:"metadata,omitempty"`

	// Warnings holds the disagreements Verify found between the metadata and the
	// SDP it arrived with; non-empty means the participant bound to a stream may
	// be wrong.
	Warnings []string `json:"warnings,omitempty"`
}

// State accumulates the recording session's metadata across the initial INVITE
// and every re-INVITE that updates it. It is safe for concurrent use.
type State struct {
	mu           sync.RWMutex
	sessionID    string
	dataMode     string
	participants map[string]ParticipantInfo
	streams      map[string]StreamInfo
	// sender maps a stream_id to the participant_id sending on it.
	sender map[string]string
	// senderConflicts records the other participants that claimed to send on a
	// stream within one document, which sender alone cannot represent.
	senderConflicts map[string][]string
	raw             []byte
	warnings        []string
}

// NewState returns an empty recording session state.
func NewState() *State {
	return &State{
		participants:    make(map[string]ParticipantInfo),
		streams:         make(map[string]StreamInfo),
		sender:          make(map[string]string),
		senderConflicts: make(map[string][]string),
	}
}

// Apply merges a metadata document into the state and reports what changed.
// A complete document replaces the state; a partial one is merged, with a
// non-empty disassociate-time removing the association (RFC 7865 §6.1).
func (s *State) Apply(r *Recording) Delta {
	if s == nil || r == nil {
		return Delta{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	beforeParticipants := keysOf(s.participants)
	beforeStreams := keysOf(s.streams)

	if !r.IsPartial() {
		s.participants = make(map[string]ParticipantInfo, len(r.Participants))
		s.streams = make(map[string]StreamInfo, len(r.Streams))
		s.sender = make(map[string]string, len(r.Streams))
		s.senderConflicts = make(map[string][]string)
	}
	if s.senderConflicts == nil {
		s.senderConflicts = make(map[string][]string)
	}
	s.dataMode = r.DataMode

	for _, sra := range r.SessionRecordingAssocs {
		if sra.SessionID != "" {
			s.sessionID = sra.SessionID
		}
	}
	if s.sessionID == "" {
		for _, sess := range r.Sessions {
			if sess.SessionID != "" {
				s.sessionID = sess.SessionID
				break
			}
		}
	}

	for i := range r.Participants {
		p := &r.Participants[i]
		if p.ParticipantID == "" {
			continue
		}
		s.participants[p.ParticipantID] = ParticipantInfo{
			ID:   p.ParticipantID,
			AOR:  p.AOR(),
			Name: p.DisplayName(),
		}
	}

	for _, st := range r.Streams {
		if st.StreamID == "" {
			continue
		}
		s.streams[st.StreamID] = StreamInfo{
			ID:        st.StreamID,
			SessionID: st.SessionID,
			Label:     st.Label,
		}
	}

	// claimed tracks the streams this document speaks for: only a document that
	// speaks for a stream again can resolve its conflict, so a partial update
	// silent about a stream leaves one standing.
	claimed := make(map[string]string)
	for _, psa := range r.ParticipantStreams {
		for _, streamID := range psa.Send {
			if streamID == "" {
				continue
			}
			if psa.DisassociateTime != "" {
				delete(s.sender, streamID)
				delete(s.streams, streamID)
				delete(s.senderConflicts, streamID)
				continue
			}
			if prev, seen := claimed[streamID]; seen {
				// The sender map holds one participant per stream, so a second
				// claim within one document is kept separately.
				if prev != psa.ParticipantID {
					s.senderConflicts[streamID] = append(s.senderConflicts[streamID], psa.ParticipantID)
				}
				continue
			}
			delete(s.senderConflicts, streamID)
			claimed[streamID] = psa.ParticipantID
			s.sender[streamID] = psa.ParticipantID
		}
	}

	// A participant that left the session is dropped along with the streams it
	// was sending — those m= sections stop carrying anyone's audio.
	for _, psa := range r.ParticipantSessions {
		if psa.DisassociateTime == "" || psa.ParticipantID == "" {
			continue
		}
		delete(s.participants, psa.ParticipantID)
		for streamID, pid := range s.sender {
			if pid == psa.ParticipantID {
				delete(s.sender, streamID)
				delete(s.streams, streamID)
				delete(s.senderConflicts, streamID)
			}
		}
		// A party that has left no longer contests anything.
		for streamID, others := range s.senderConflicts {
			kept := others[:0]
			for _, pid := range others {
				if pid != psa.ParticipantID {
					kept = append(kept, pid)
				}
			}
			if len(kept) == 0 {
				delete(s.senderConflicts, streamID)
			} else {
				s.senderConflicts[streamID] = kept
			}
		}
	}

	s.rebindLocked()

	return Delta{
		ParticipantsAdded:   addedKeys(beforeParticipants, s.participants),
		ParticipantsRemoved: removedKeys(beforeParticipants, s.participants),
		StreamsAdded:        addedKeys(beforeStreams, s.streams),
		StreamsRemoved:      removedKeys(beforeStreams, s.streams),
	}
}

// rebindLocked refreshes every stream's participant from the current sender map.
func (s *State) rebindLocked() {
	for id, st := range s.streams {
		st.Participant = s.participants[s.sender[id]]
		s.streams[id] = st
	}
}

// SetRaw stores the metadata document as received, for exposure over the API.
func (s *State) SetRaw(raw []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = append([]byte(nil), raw...)
}

// SetWarnings records the metadata/SDP disagreements Verify found, replacing
// any from an earlier document.
func (s *State) SetWarnings(issues []Issue) {
	if s == nil {
		return
	}
	w := make([]string, 0, len(issues))
	for _, i := range issues {
		w = append(w, i.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warnings = w
}

// SessionID returns the recording session's communication session ID.
func (s *State) SessionID() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionID
}

// StreamForLabel returns the stream carrying the given SDP a=label value.
func (s *State) StreamForLabel(label string) (StreamInfo, bool) {
	if s == nil || label == "" {
		return StreamInfo{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, st := range s.streams {
		if st.Label == label {
			return st, true
		}
	}
	return StreamInfo{}, false
}

// ParticipantForLabel returns the participant whose audio arrives on the m=
// section carrying the given a=label.
func (s *State) ParticipantForLabel(label string) (ParticipantInfo, bool) {
	st, ok := s.StreamForLabel(label)
	if !ok || st.Participant.ID == "" {
		return ParticipantInfo{}, false
	}
	return st.Participant, true
}

// Snapshot returns the current read model, ordered deterministically.
func (s *State) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{Participants: []ParticipantInfo{}, Streams: []StreamInfo{}}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := Snapshot{
		SessionID:    s.sessionID,
		DataMode:     s.dataMode,
		Participants: make([]ParticipantInfo, 0, len(s.participants)),
		Streams:      make([]StreamInfo, 0, len(s.streams)),
		Metadata:     string(s.raw),
		Warnings:     append([]string(nil), s.warnings...),
	}
	for _, p := range s.participants {
		snap.Participants = append(snap.Participants, p)
	}
	for _, st := range s.streams {
		snap.Streams = append(snap.Streams, st)
	}
	sort.Slice(snap.Participants, func(i, j int) bool { return snap.Participants[i].ID < snap.Participants[j].ID })
	sort.Slice(snap.Streams, func(i, j int) bool {
		if snap.Streams[i].Label != snap.Streams[j].Label {
			return snap.Streams[i].Label < snap.Streams[j].Label
		}
		return snap.Streams[i].ID < snap.Streams[j].ID
	})
	return snap
}

func keysOf[V any](m map[string]V) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func addedKeys[V any](before map[string]struct{}, after map[string]V) []string {
	var out []string
	for k := range after {
		if _, ok := before[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func removedKeys[V any](before map[string]struct{}, after map[string]V) []string {
	var out []string
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Merged returns the accumulated session as one complete document, so a partial
// update can be checked against the whole session rather than the delta it
// arrived in. Ordering is deterministic.
func (s *State) Merged() *Recording {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec := &Recording{DataMode: DataModeComplete}
	if s.sessionID != "" {
		rec.Sessions = []Session{{SessionID: s.sessionID}}
		rec.SessionRecordingAssocs = []SessionRecordingAssoc{{SessionID: s.sessionID}}
	}

	for _, id := range sortedKeys(s.participants) {
		p := s.participants[id]
		rec.Participants = append(rec.Participants, Participant{
			ParticipantID: p.ID,
			SessionID:     s.sessionID,
			NameIDs:       []NameID{{AOR: p.AOR}},
		})
	}

	for _, id := range sortedKeys(s.streams) {
		st := s.streams[id]
		rec.Streams = append(rec.Streams, Stream{
			StreamID:  st.ID,
			SessionID: st.SessionID,
			Label:     st.Label,
		})
	}

	sends := make(map[string][]string)
	for streamID, pid := range s.sender {
		sends[pid] = append(sends[pid], streamID)
	}
	for streamID, others := range s.senderConflicts {
		for _, pid := range others {
			sends[pid] = append(sends[pid], streamID)
		}
	}
	for _, pid := range sortedKeys(sends) {
		streams := sends[pid]
		sort.Strings(streams)
		rec.ParticipantStreams = append(rec.ParticipantStreams, ParticipantStreamAssoc{
			ParticipantID: pid,
			Send:          streams,
		})
	}
	return rec
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
