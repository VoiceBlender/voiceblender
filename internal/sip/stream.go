package sip

import "github.com/VoiceBlender/voiceblender/internal/codec"

// Media direction attribute values (RFC 4566 / RFC 8866).
const (
	DirSendRecv = "sendrecv"
	DirSendOnly = "sendonly"
	DirRecvOnly = "recvonly"
	DirInactive = "inactive"
)

// AudioStream describes one m=audio section to generate. A zero Port emits a
// rejected/disabled section per RFC 3264 §6.
type AudioStream struct {
	// MID (RFC 5888), Label (RFC 4574), Content (RFC 4796) and Lang (RFC 8866,
	// a BCP 47 tag) are each omitted when empty.
	MID     string
	Label   string
	Content string
	Lang    string

	Port      int
	Direction string // "" means sendrecv
	Ptime     int    // 0 means 20

	// Codecs holds every codec for an offer, or the single selected codec for
	// an answer. CodecPTs overrides the default payload type per codec —
	// answers must echo the offerer's dynamic PTs.
	Codecs   []codec.CodecType
	CodecPTs map[codec.CodecType]uint8

	DTMFPT        uint8
	DTMFClockRate int
	OfferTE48k    bool // emit the extra telephone-event/48000 line used with Opus

	AMRWBOctetAligned bool
	AMRWBModeSet      string
	AMRNBOctetAligned bool
	AMRNBModeSet      string
}

// PayloadType returns the payload type to advertise for c, preferring an
// explicit CodecPTs entry over the codec's static default.
func (s *AudioStream) PayloadType(c codec.CodecType) uint8 {
	if pt, ok := s.CodecPTs[c]; ok {
		return pt
	}
	return c.PayloadType()
}

// RemoteAudioStream holds the parsed parameters of one remote m=audio section.
// A zero RemotePort means the peer rejected or disabled the section.
type RemoteAudioStream struct {
	Index int // m-line position within the SDP

	MID     string
	Label   string
	Content string
	Lang    string

	RemoteIP      string
	RemotePort    int
	AddressFamily string // "IP4" or "IP6"

	// Direction is the media-level attribute, falling back to the session-level
	// one. It stays empty when neither is present; use EffectiveDirection for
	// the RFC 8866 sendrecv default.
	Direction string
	Ptime     int
	RTCPMux   bool

	Codecs       []codec.CodecType
	CodecPTs     map[codec.CodecType]uint8
	CodecRates   map[codec.CodecType]int
	CodecFmtp    map[codec.CodecType]string
	DTMFEventPTs map[uint8]int
}

// EffectiveDirection resolves an absent direction attribute to the RFC 8866
// default of sendrecv.
func (r *RemoteAudioStream) EffectiveDirection() string {
	if r.Direction == "" {
		return DirSendRecv
	}
	return r.Direction
}

// DTMFPTForRate returns the telephone-event payload type advertised at the
// given clock rate, if any.
func (r *RemoteAudioStream) DTMFPTForRate(rate int) (uint8, bool) {
	for pt, got := range r.DTMFEventPTs {
		if got == rate {
			return pt, true
		}
	}
	return 0, false
}

// PreferredDTMFEvent returns the telephone-event PT and clock rate to mirror in
// an answer. When several were offered the lowest PT wins, for determinism.
func (r *RemoteAudioStream) PreferredDTMFEvent() (pt uint8, rate int, ok bool) {
	var best uint8
	var bestRate int
	found := false
	for p, rt := range r.DTMFEventPTs {
		if !found || p < best {
			best, bestRate, found = p, rt, true
		}
	}
	return best, bestRate, found
}

// RemoteMLine records one m= section of any kind in offer order. Sections we
// don't handle are recorded too, so an answer can preserve the m-line count and
// ordering required by RFC 3264 §6.
type RemoteMLine struct {
	Index   int
	Media   string   // "audio", "video", "text", "application", ...
	Proto   []string // must be echoed verbatim when rejecting the section
	Port    int
	MID     string
	Formats []string

	// AudioIdx indexes into SDPMedia.Audio, or -1 for non-audio sections.
	AudioIdx int
}

// MirrorDirection returns the direction an answer must carry for an offered
// direction, per RFC 3264 §6.1. An offered sendrecv permits any answer; we
// mirror it as sendrecv and let callers narrow it further.
func MirrorDirection(offered string) string {
	switch offered {
	case DirSendOnly:
		return DirRecvOnly
	case DirRecvOnly:
		return DirSendOnly
	case DirInactive:
		return DirInactive
	default:
		return DirSendRecv
	}
}

// NarrowDirection restricts a mirrored answer direction to at most max, which
// RFC 3264 §6.1 permits: an answer to sendrecv may be recvonly. An empty max
// leaves the mirrored direction alone. A recording session uses this to stay
// receive-only even when the offer was sendrecv.
func NarrowDirection(mirrored, max string) string {
	if max != DirRecvOnly {
		return mirrored
	}
	switch mirrored {
	case DirSendRecv, DirRecvOnly:
		return DirRecvOnly
	default:
		return DirInactive
	}
}

// HoldDirection returns the direction to advertise for a stream whose intent is
// desired while the leg is on hold. Off hold the intent passes through.
func HoldDirection(desired string, held bool) string {
	if !held {
		if desired == "" {
			return DirSendRecv
		}
		return desired
	}
	switch desired {
	case DirRecvOnly, DirInactive:
		return DirInactive
	default:
		return DirSendOnly
	}
}
