//go:build integration && interop

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

const interopDigits = "0123456789*#"

// vbDTMF is every digit legID has reported, in order.
func vbDTMF(inst *testInstance, legID string) string {
	var b strings.Builder
	for _, e := range inst.collector.matchAll(events.DTMFReceived, func(e events.Event) bool { return e.Data.GetLegID() == legID }) {
		b.WriteString(e.Data.(*events.DTMFReceivedData).Digit)
	}
	return b.String()
}

func waitVBDTMF(t *testing.T, inst *testInstance, legID string, n int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := vbDTMF(inst, legID); len(got) >= n {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	return vbDTMF(inst, legID)
}

func sendDTMFOnVB(t *testing.T, inst *testInstance, legID, digits string) {
	t.Helper()
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/dtmf", inst.baseURL(), legID), map[string]string{"digits": digits})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send DTMF: status %d", resp.StatusCode)
	}
}

// assertDTMFBothWaysInterop sends the full digit set each way and checks it
// arrives once, in order.
func assertDTMFBothWaysInterop(t *testing.T, inst *testInstance, legID string, call peers.Call) {
	t.Helper()
	if err := call.SendDTMF(interopDigits); err != nil {
		t.Fatal(err)
	}
	got := waitVBDTMF(t, inst, legID, len(interopDigits), 10*time.Second)
	time.Sleep(500 * time.Millisecond)
	if got = vbDTMF(inst, legID); got != interopDigits {
		t.Errorf("peer -> VoiceBlender: got %q, want %q", got, interopDigits)
	}

	sendDTMFOnVB(t, inst, legID, interopDigits)
	got, err := call.WaitDTMF(interopCtx(t, 10*time.Second), len(interopDigits))
	if err != nil {
		t.Errorf("VoiceBlender -> peer: %v (got %q)", err, got)
		return
	}
	time.Sleep(500 * time.Millisecond)
	if got, _ = call.WaitDTMF(interopCtx(t, time.Second), 0); got != interopDigits {
		t.Errorf("VoiceBlender -> peer: got %q, want %q", got, interopDigits)
	}
}

func TestInterop_DTMF_RFC2833(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		for _, codec := range interopCodecs(p) {
			for _, setup := range interopCallSetups {
				t.Run(codec+"/"+setup.name, func(t *testing.T) {
					legID, call := setup.connect(t, inst, p, []string{codec}, 0)
					assertDTMFBothWaysInterop(t, inst, legID, call)
				})
			}
		}
	})
}
