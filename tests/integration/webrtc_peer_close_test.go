//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestWebRTC_PeerCloseDisconnectsLeg proves a WebRTC leg is torn down when
// the peer closes its connection cleanly. The peer's DTLS close_notify never
// drives ICE to failed/disconnected, so nothing else would notice.
func TestWebRTC_PeerCloseDisconnectsLeg(t *testing.T) {
	inst := newTestInstance(t, "webrtc-peer-close")

	pc, cands, connected := newWebRTCClient(t)
	defer pc.Close()
	legID := offerWebRTCLeg(t, inst, pc, "")
	trickleICEUntilConnected(t, inst, pc, legID, cands, connected, 10*time.Second)

	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 5*time.Second)

	if err := pc.Close(); err != nil {
		t.Fatalf("client Close: %v", err)
	}

	disc := inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 5*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "peer_closed" {
		t.Errorf("cdr.reason = %q, want peer_closed", got)
	}

	resp := httpGet(t, inst.baseURL()+"/v1/legs/"+legID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET leg after peer close: status %d, want 404", resp.StatusCode)
	}
}
