package sip

import "github.com/VoiceBlender/voiceblender/internal/codec"

// AnswerOptions parameterizes PlanAnswer.
type AnswerOptions struct {
	SupportedCodecs []codec.CodecType
	// Preferred biases codec selection for the first accepted audio section.
	Preferred codec.CodecType

	// StrictMLines makes the plan cover every offered m= section, rejecting the
	// ones we don't handle with port 0 as RFC 3264 §6 requires. With it off
	// the plan omits unhandled non-audio sections, preserving the legacy
	// (spec-violating) answer shape.
	StrictMLines bool

	// TextMLineIndex is the m= section the caller renders itself as m=text, or
	// -1 when there is none.
	TextMLineIndex int
}

// SlotAction is what an answer does with one offered m= section.
type SlotAction uint8

const (
	// SlotAccept negotiates the section and carries media on it.
	SlotAccept SlotAction = iota
	// SlotReject emits a port-0 placeholder so the answer keeps the offer's
	// m-line count and ordering.
	SlotReject
	// SlotSkip leaves the section to the caller (the m=text path).
	SlotSkip
	// SlotOmit drops the section from the answer entirely. Only produced when
	// StrictMLines is off, for backward compatibility.
	SlotOmit
)

// SlotPlan is the decision for one offered m= section.
type SlotPlan struct {
	Index  int
	Media  string
	Proto  []string
	Action SlotAction
	Reason string // why a section was rejected; "" when accepted

	// AudioIdx indexes into SDPMedia.Audio, or -1 for non-audio sections.
	AudioIdx int

	Codec     codec.CodecType
	PT        uint8
	Direction string // the direction our answer must carry

	MID     string
	Label   string
	Content string
	Lang    string
}

// PlanAnswer decides, for every m= section in an offer, what the answer does
// with it. It is pure: no ports are allocated and no media is touched, so the
// caller can reject a section it cannot materialize and still emit a
// conformant answer.
//
// RFC 3264 §6 governs the shape: one answer section per offered section, in
// the same order, with port 0 for anything refused.
func PlanAnswer(offer *SDPMedia, opts AnswerOptions) []SlotPlan {
	if offer == nil {
		return nil
	}
	plans := make([]SlotPlan, 0, len(offer.MLines))
	accepted := 0

	for _, ml := range offer.MLines {
		p := SlotPlan{
			Index:    ml.Index,
			Media:    ml.Media,
			Proto:    ml.Proto,
			AudioIdx: ml.AudioIdx,
			MID:      ml.MID,
		}

		if ml.Media != "audio" {
			switch {
			case ml.Index == opts.TextMLineIndex:
				p.Action = SlotSkip
			case opts.StrictMLines:
				p.Action = SlotReject
				p.Reason = "unsupported_media"
			default:
				p.Action = SlotOmit
				p.Reason = "unsupported_media"
			}
			plans = append(plans, p)
			continue
		}

		ra := &offer.Audio[ml.AudioIdx]
		p.Label, p.Content, p.Lang = ra.Label, ra.Content, ra.Lang
		if p.MID == "" {
			p.MID = ra.MID
		}

		switch {
		case ra.RemotePort == 0:
			// The peer already disabled this section; echo the rejection.
			p.Action = SlotReject
			p.Reason = "offer_disabled"
		default:
			preferred := codec.CodecUnknown
			if accepted == 0 {
				preferred = opts.Preferred
			}
			selected, pt, ok := NegotiateCodecStream(ra, opts.SupportedCodecs, preferred)
			if !ok {
				p.Action = SlotReject
				p.Reason = "no_common_codec"
				break
			}
			p.Action = SlotAccept
			p.Codec = selected
			p.PT = pt
			p.Direction = MirrorDirection(ra.EffectiveDirection())
			accepted++
		}

		plans = append(plans, p)
	}

	return plans
}

// AcceptedAudio returns the plans that negotiated an audio section.
func AcceptedAudio(plans []SlotPlan) []SlotPlan {
	var out []SlotPlan
	for _, p := range plans {
		if p.Action == SlotAccept {
			out = append(out, p)
		}
	}
	return out
}
