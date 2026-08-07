package sip

import "strconv"

// SlotState is the negotiation state of one m-line slot.
type SlotState uint8

const (
	// SlotPending is offered by us but not yet answered by the peer.
	SlotPending SlotState = iota
	// SlotActive is negotiated and carrying (or ready to carry) media.
	SlotActive
	// SlotTombstone is disabled forever: the slot keeps its position and is
	// re-emitted with port 0 in every subsequent offer (RFC 3264 §8.2).
	SlotTombstone
)

func (s SlotState) String() string {
	switch s {
	case SlotPending:
		return "pending"
	case SlotActive:
		return "active"
	case SlotTombstone:
		return "removed"
	default:
		return "unknown"
	}
}

// MLineSlot is one position in a dialog's m-line vector.
type MLineSlot struct {
	Index int
	Media string   // "audio", "text", or a kind we only hold a place for
	Proto []string // echoed verbatim when the slot is rejected

	MID     string
	Label   string
	Content string
	Lang    string

	State SlotState

	// StreamID links an audio slot to the media stream carrying it. Empty for
	// tombstones and for slots we merely hold a position for.
	StreamID string

	Local  AudioStream       // what we advertise
	Remote RemoteAudioStream // what the peer last told us
}

// Audio reports whether the slot is an audio section.
func (s *MLineSlot) Audio() bool { return s.Media == "audio" }

// MLineTable is a dialog's m-line vector. Slots are append-only for the life of
// the dialog: RFC 3264 §8 requires that the m-line count never decreases and
// that the i-th line always describes the same stream. Removal is a tombstone,
// never a deletion.
//
// Not safe for concurrent use — the owning leg serializes access.
type MLineTable struct {
	slots   []MLineSlot
	nextMID int
}

// Len returns the number of slots, i.e. the m-line count every offer and answer
// on this dialog must carry.
func (t *MLineTable) Len() int { return len(t.slots) }

// Slot returns the slot at index i, or nil when i is out of range.
func (t *MLineTable) Slot(i int) *MLineSlot {
	if i < 0 || i >= len(t.slots) {
		return nil
	}
	return &t.slots[i]
}

// Slots returns the backing slice for iteration. Callers must not append to it.
func (t *MLineTable) Slots() []MLineSlot { return t.slots }

// ByMID returns the slot carrying the given a=mid value.
func (t *MLineTable) ByMID(mid string) (*MLineSlot, bool) {
	if mid == "" {
		return nil, false
	}
	for i := range t.slots {
		if t.slots[i].MID == mid {
			return &t.slots[i], true
		}
	}
	return nil, false
}

// ByStreamID returns the slot bound to the given media stream.
func (t *MLineTable) ByStreamID(id string) (*MLineSlot, bool) {
	if id == "" {
		return nil, false
	}
	for i := range t.slots {
		if t.slots[i].StreamID == id {
			return &t.slots[i], true
		}
	}
	return nil, false
}

// Append adds a slot at the next free position and returns its index. The
// slot's Index is overwritten to match.
func (t *MLineTable) Append(s MLineSlot) int {
	s.Index = len(t.slots)
	t.slots = append(t.slots, s)
	return s.Index
}

// Tombstone disables the slot at index i permanently, keeping its position.
func (t *MLineTable) Tombstone(i int) {
	s := t.Slot(i)
	if s == nil {
		return
	}
	s.State = SlotTombstone
	s.StreamID = ""
	s.Local.Port = 0
	s.Remote = RemoteAudioStream{Index: i}
}

// ActiveAudio returns the audio slots currently carrying media, in m-line order.
func (t *MLineTable) ActiveAudio() []*MLineSlot {
	var out []*MLineSlot
	for i := range t.slots {
		if t.slots[i].Audio() && t.slots[i].State == SlotActive {
			out = append(out, &t.slots[i])
		}
	}
	return out
}

// ActiveAudioCount returns how many audio slots are active or awaiting an
// answer — the figure a per-dialog stream cap applies to.
func (t *MLineTable) ActiveAudioCount() int {
	n := 0
	for i := range t.slots {
		if t.slots[i].Audio() && t.slots[i].State != SlotTombstone {
			n++
		}
	}
	return n
}

// MintMID returns an a=mid value not yet used in this table.
func (t *MLineTable) MintMID() string {
	for {
		mid := strconv.Itoa(t.nextMID)
		t.nextMID++
		if _, taken := t.ByMID(mid); !taken {
			return mid
		}
	}
}

// ReserveMID records mid as used so MintMID never collides with it. Call it for
// every mid adopted from a peer's offer.
func (t *MLineTable) ReserveMID(mid string) {
	if n, err := strconv.Atoi(mid); err == nil && n >= t.nextMID {
		t.nextMID = n + 1
	}
}

// LocalStreams renders the table as the audio sections of an offer or answer:
// every slot in index order, tombstones as port 0. held applies the RFC 3264
// hold transform to each active slot's desired direction.
//
// Non-audio slots are returned as zero-port audio placeholders only when they
// were never ours to describe; callers that own an m=text section render it
// themselves and pass skipMedia to exclude it here.
func (t *MLineTable) LocalStreams(held bool, skipMedia map[int]bool) []AudioStream {
	out := make([]AudioStream, 0, len(t.slots))
	for i := range t.slots {
		s := &t.slots[i]
		if skipMedia[i] {
			continue
		}
		if s.State == SlotTombstone || s.Local.Port == 0 {
			out = append(out, AudioStream{Port: 0, MID: s.MID})
			continue
		}
		local := s.Local
		local.MID = s.MID
		local.Label = s.Label
		local.Content = s.Content
		local.Lang = s.Lang
		local.Direction = HoldDirection(s.Local.Direction, held)
		out = append(out, local)
	}
	return out
}
