package siprec

import (
	"fmt"
	"sort"
	"strings"
)

// MediaSection is one m= section of a recording session's SDP, reduced to the
// fields a metadata document can be checked against.
type MediaSection struct {
	// Label is the a=label value binding this section to a <stream> element.
	Label string
	// CNAME is the a=ssrc cname: value, empty when the offer carries none.
	CNAME string
}

// IssueKind classifies a disagreement between a metadata document and the SDP
// it arrived with.
type IssueKind string

const (
	// IssueDuplicateLabel means two streams claim the same label, so neither
	// resolves to a single participant.
	IssueDuplicateLabel IssueKind = "duplicate_label"
	// IssueUnknownLabel means the metadata labels a stream the SDP never offers.
	IssueUnknownLabel IssueKind = "unknown_label"
	// IssueUnclaimedLabel means the metadata declares a stream that no
	// participant sends on, so its audio cannot be attributed to anyone.
	IssueUnclaimedLabel IssueKind = "unclaimed_label"
	// IssueParticipantMismatch means the SDP and the metadata name different
	// parties as the sender of the same section.
	IssueParticipantMismatch IssueKind = "participant_mismatch"
	// IssueAmbiguousSender means two participants claim to send on the same
	// section, so it cannot be attributed to either.
	IssueAmbiguousSender IssueKind = "ambiguous_sender"
)

// Issue is one metadata/SDP disagreement, identified by the label it concerns.
type Issue struct {
	Kind   IssueKind
	Label  string
	Detail string
}

func (i Issue) String() string {
	if i.Label == "" {
		return fmt.Sprintf("%s: %s", i.Kind, i.Detail)
	}
	return fmt.Sprintf("%s (label %s): %s", i.Kind, i.Label, i.Detail)
}

// Verify cross-checks a metadata document against the SDP it arrived with and
// returns every disagreement it can prove, ordered deterministically. A
// section's a=ssrc cname is the SDP's own statement of who sends on it; only the
// user part is compared, since the cname is written by whatever anchored the
// media and routinely carries a different host than the AOR.
//
// An empty result means nothing could be disproved, not that the document is
// right.
func Verify(r *Recording, sections []MediaSection) []Issue {
	if r == nil {
		return nil
	}

	var issues []Issue

	labelOfStream := make(map[string]string, len(r.Streams))
	seenLabel := make(map[string]string, len(r.Streams))
	duplicated := make(map[string]bool)
	for _, st := range r.Streams {
		if st.StreamID == "" || st.Label == "" {
			continue
		}
		labelOfStream[st.StreamID] = st.Label
		if first, dup := seenLabel[st.Label]; dup {
			if !duplicated[st.Label] {
				duplicated[st.Label] = true
				issues = append(issues, Issue{
					Kind:   IssueDuplicateLabel,
					Label:  st.Label,
					Detail: fmt.Sprintf("streams %s and %s both claim it", first, st.StreamID),
				})
			}
			continue
		}
		seenLabel[st.Label] = st.StreamID
	}

	aorOfParticipant := make(map[string]string, len(r.Participants))
	for i := range r.Participants {
		p := &r.Participants[i]
		if p.ParticipantID != "" {
			aorOfParticipant[p.ParticipantID] = p.AOR()
		}
	}

	senderOfLabel := make(map[string]string, len(r.ParticipantStreams))
	ambiguous := make(map[string]bool)
	for _, psa := range r.ParticipantStreams {
		if psa.DisassociateTime != "" {
			continue
		}
		for _, streamID := range psa.Send {
			label, ok := labelOfStream[streamID]
			if !ok {
				continue
			}
			// First claim wins, so the outcome does not depend on document order.
			if prev, seen := senderOfLabel[label]; seen && prev != psa.ParticipantID {
				if !duplicated[label] && !ambiguous[label] {
					ambiguous[label] = true
					issues = append(issues, Issue{
						Kind:   IssueAmbiguousSender,
						Label:  label,
						Detail: fmt.Sprintf("participants %s and %s both send on it", prev, psa.ParticipantID),
					})
				}
				continue
			}
			senderOfLabel[label] = psa.ParticipantID
		}
	}

	offered := make(map[string]MediaSection, len(sections))
	for _, sec := range sections {
		if sec.Label != "" {
			offered[sec.Label] = sec
		}
	}

	// An offer with no labelled sections is no evidence about any label.
	if len(offered) > 0 {
		for label := range seenLabel {
			if _, ok := offered[label]; !ok {
				issues = append(issues, Issue{
					Kind:   IssueUnknownLabel,
					Label:  label,
					Detail: "no m= section in the offer carries this label",
				})
			}
		}
	}

	// Only for a stream the document declares. A section the document says
	// nothing about is not a contradiction, and it is what a departure leaves
	// behind: the party's association is closed with a disassociate-time and
	// its stream drops out of the session, while the m= section stays in the
	// offer. Reporting that would flag every call somebody hangs up early on.
	for label := range seenLabel {
		if _, inOffer := offered[label]; len(offered) > 0 && !inOffer {
			continue // already reported as unknown_label
		}
		if _, ok := senderOfLabel[label]; !ok {
			issues = append(issues, Issue{
				Kind:   IssueUnclaimedLabel,
				Label:  label,
				Detail: "the metadata declares this stream but no participant sends on it",
			})
		}
	}

	for label, sec := range offered {
		// Neither binds the label to one participant, so nothing to compare.
		if duplicated[label] || ambiguous[label] {
			continue
		}
		sender := cnameUser(sec.CNAME)
		if sender == "" {
			continue
		}
		participantID, ok := senderOfLabel[label]
		if !ok {
			continue
		}
		metaUser := aorUser(aorOfParticipant[participantID])
		if metaUser == "" || strings.EqualFold(metaUser, sender) {
			continue
		}
		issues = append(issues, Issue{
			Kind:  IssueParticipantMismatch,
			Label: label,
			Detail: fmt.Sprintf("offer says %s sends on it, metadata assigns it to %s (%s)",
				sender, metaUser, participantID),
		})
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Label != issues[j].Label {
			return issues[i].Label < issues[j].Label
		}
		return issues[i].Kind < issues[j].Kind
	})
	return issues
}

// cnameUser returns the party an a=ssrc cname names, or "" when it names nobody
// in particular. Only a SIP or tel URI is a statement about identity: an
// RFC 3550 cname is ordinarily "<token>@<host>" with the token a synchronisation
// source identifier, and reading a party out of one would contradict the
// metadata of every correct session that does not write URIs there.
func cnameUser(v string) string {
	switch scheme, _, found := strings.Cut(strings.TrimSpace(v), ":"); {
	case !found:
		return ""
	case strings.EqualFold(scheme, "sip"),
		strings.EqualFold(scheme, "sips"),
		strings.EqualFold(scheme, "tel"):
		return aorUser(v)
	default:
		return ""
	}
}

// aorUser returns the user part of a SIP AOR or an RTCP cname shaped like one.
// A value with no user part yields "", which callers treat as "no claim made".
func aorUser(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	scheme := ""
	if i := strings.IndexByte(v, ':'); i >= 0 {
		switch s := strings.ToLower(v[:i]); s {
		case "sip", "sips", "tel":
			scheme, v = s, v[i+1:]
		}
	}
	if i := strings.IndexByte(v, '@'); i >= 0 {
		return v[:i]
	}
	// A tel URI has no host (RFC 3966), so the whole value up to the first
	// parameter is the number. A sip URI without a user part names only a
	// host, which identifies nobody.
	if scheme == "tel" {
		if i := strings.IndexByte(v, ';'); i >= 0 {
			return v[:i]
		}
		return v
	}
	return ""
}
