package sip

import (
	"strings"

	"github.com/emiago/sipgo/sip"
)

// OptionTagSIPREC is the option tag an SRC puts in Require to demand a
// recording session (RFC 7866 §6.1).
const OptionTagSIPREC = "siprec"

// srcFeatureTag is the Contact media feature tag identifying the sender as a
// session recording client (RFC 7866 §6.1).
const srcFeatureTag = "+sip.src"

// OptionTags returns the option tags carried by every instance of a header,
// lowercased and trimmed.
func OptionTags(msg BodyCarrier, header string) []string {
	var out []string
	for _, h := range msg.GetHeaders(header) {
		for _, tok := range strings.Split(h.Value(), ",") {
			if tok = strings.ToLower(strings.TrimSpace(tok)); tok != "" {
				out = append(out, tok)
			}
		}
	}
	return out
}

// HasOptionTag reports whether Require or Proxy-Require demands tag.
func HasOptionTag(msg BodyCarrier, tag string) bool {
	for _, header := range []string{"Require", "Proxy-Require"} {
		for _, got := range OptionTags(msg, header) {
			if got == tag {
				return true
			}
		}
	}
	return false
}

// HasSRCFeatureTag reports whether the Contact header identifies a session
// recording client.
func HasSRCFeatureTag(req *sip.Request) bool {
	if req == nil {
		return false
	}
	for _, h := range req.GetHeaders("Contact") {
		if strings.Contains(strings.ToLower(h.Value()), srcFeatureTag) {
			return true
		}
	}
	return false
}

// SIPRECSignals describes how an INVITE announced itself as a recording
// session. Conformance varies between SBCs, so each signal is reported
// separately and the caller decides what to do with a partial claim.
type SIPRECSignals struct {
	Required    bool // Require/Proxy-Require: siprec
	FeatureTag  bool // Contact carries +sip.src
	HasMetadata bool // the body carries an rs-metadata part
}

// Claimed reports whether the INVITE claims to be a recording session at all.
func (s SIPRECSignals) Claimed() bool {
	return s.Required || s.FeatureTag || s.HasMetadata
}

// DetectSIPREC classifies an inbound INVITE against the three RFC 7866 signals.
func DetectSIPREC(call *InboundCall) SIPRECSignals {
	if call == nil || call.Request == nil {
		return SIPRECSignals{}
	}
	var s SIPRECSignals
	s.Required = HasOptionTag(call.Request, OptionTagSIPREC)
	s.FeatureTag = HasSRCFeatureTag(call.Request)
	if call.Body != nil {
		_, s.HasMetadata = call.Body.RSMetadata()
	}
	return s
}

// IsSIPRECInvite reports whether an inbound INVITE opens a recording session.
func IsSIPRECInvite(call *InboundCall) bool {
	return DetectSIPREC(call).Claimed()
}

// UnsupportedHeader builds the Unsupported header that must accompany a
// 420 Bad Extension (RFC 3261 §8.2.2.3).
func UnsupportedHeader(tags ...string) sip.Header {
	return sip.NewHeader("Unsupported", strings.Join(tags, ", "))
}
