package lkmedia

import (
	"fmt"

	"github.com/pion/sdp/v3"
)

// fixRejectedMids walks the answer's m-sections in order and, for any
// rejected section (port 0) missing `a=mid:`, injects the mid from the
// offer's corresponding (positionally aligned) m-section.
//
// pion/webrtc v4 — verified through v4.2.14 and current master — omits
// `a=mid:` from rejected m-sections in answer SDPs. LiveKit's
// server-side pion then fails its next CreateOffer() with
// "remoteDescription contained media section without mid value" and
// kicks the participant with STATE_MISMATCH. The rejected m-section
// happens when the browser publishes a video track and our MediaEngine
// (Opus-only) has no codec to match it.
//
// Returns the (possibly unmodified) SDP. On parse failures the original
// SDP is returned alongside the error so the caller can choose to log
// and proceed.
func fixRejectedMids(offerSDP, answerSDP string) (string, error) {
	var offer, answer sdp.SessionDescription
	if err := offer.Unmarshal([]byte(offerSDP)); err != nil {
		return answerSDP, fmt.Errorf("parse offer: %w", err)
	}
	if err := answer.Unmarshal([]byte(answerSDP)); err != nil {
		return answerSDP, fmt.Errorf("parse answer: %w", err)
	}
	changed := false
	for i, m := range answer.MediaDescriptions {
		if i >= len(offer.MediaDescriptions) {
			break
		}
		if m.MediaName.Port.Value != 0 {
			continue
		}
		if _, ok := m.Attribute("mid"); ok {
			continue
		}
		offMid, ok := offer.MediaDescriptions[i].Attribute("mid")
		if !ok || offMid == "" {
			continue
		}
		m.Attributes = append(m.Attributes, sdp.NewAttribute("mid", offMid))
		changed = true
	}
	if !changed {
		return answerSDP, nil
	}
	out, err := answer.Marshal()
	if err != nil {
		return answerSDP, fmt.Errorf("marshal answer: %w", err)
	}
	return string(out), nil
}
