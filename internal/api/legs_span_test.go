package api

import (
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// spanEnderLeg is an apiMockLeg that records the reasons EndRootSpan is
// called with. apiMockLeg deliberately does not implement leg.RootSpanEnder
// (only SIP legs carry a span), so publishDisconnect's type assertion skips
// it silently — hence the wrapper.
//
// The assertion is at the CALL boundary rather than through a span exporter
// on purpose: the real SIPLeg's spanEndOnce swallows every call after the
// first, so an exporter-based test could not tell which reason won the race
// — the same reason the countingSpan tests in internal/leg assert here.
type spanEnderLeg struct {
	*apiMockLeg
	mu      sync.Mutex
	reasons []string
}

func (l *spanEnderLeg) EndRootSpan(reason string) {
	l.mu.Lock()
	l.reasons = append(l.reasons, reason)
	l.mu.Unlock()
}

func (l *spanEnderLeg) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reasons...)
}

func newSpanEnderLeg(id string) *spanEnderLeg {
	return &spanEnderLeg{apiMockLeg: &apiMockLeg{id: id, createdAt: time.Now()}}
}

// TestPublishDisconnectEndsRootSpan pins the span-close funnel every
// API-driven disconnect flows through. Deleting the RootSpanEnder block from
// publishDisconnect leaves every SIP leg's span unended and unexported —
// the feature's headline claim silently becomes zero spans on the normal
// path.
func TestPublishDisconnectEndsRootSpan(t *testing.T) {
	var _ leg.RootSpanEnder = (*spanEnderLeg)(nil)

	s := newTestServer(t)
	l := newSpanEnderLeg("leg-1")

	s.publishDisconnect(l, "api_hangup")

	got := l.recorded()
	if len(got) != 1 || got[0] != "api_hangup" {
		t.Fatalf("EndRootSpan reasons = %v, want [api_hangup]", got)
	}
}

// TestPublishDisconnectSpanReasonIsClaimWinner is the invariant API.md
// documents: the span's leg.disconnect_reason is "the same reason string as
// the leg.disconnected event". Only the ClaimDisconnect winner publishes an
// event, so only the winner may stamp the span. A racing loser (DELETE
// /legs/{id} against the RTP-timeout callback is the real shape) must not
// overwrite the reason with one no event ever carried.
func TestPublishDisconnectSpanReasonIsClaimWinner(t *testing.T) {
	s := newTestServer(t)
	l := newSpanEnderLeg("leg-1")

	s.publishDisconnect(l, "api_hangup")  // wins the CAS
	s.publishDisconnect(l, "rtp_timeout") // loses; must not touch the span

	got := l.recorded()
	if len(got) != 1 || got[0] != "api_hangup" {
		t.Fatalf("EndRootSpan reasons = %v, want [api_hangup] — the span's reason must be the ClaimDisconnect winner's, matching the leg.disconnected event", got)
	}
}
