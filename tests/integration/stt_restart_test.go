//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// TestSTT_LegRestartKeepsNewTranscriber stops STT on a leg and at once starts
// it again. The first transcriber's exit must not drop the second.
func TestSTT_LegRestartKeepsNewTranscriber(t *testing.T) {
	mock, instA, legID := newSwapCall(t, "stt-restart-leg")
	sttURL := fmt.Sprintf("%s/v1/legs/%s/stt", instA.baseURL(), legID)

	swapPost(t, sttURL, map[string]any{"provider": "deepgram"})
	waitForCond(t, 10*time.Second, "first transcriber to dial", func() bool {
		return mock.uriFor("/v1/listen") != ""
	})
	// Dialled is not yet reading audio; let the send loop start.
	time.Sleep(100 * time.Millisecond)
	swapDelete(t, sttURL, "stop first STT")

	swapPost(t, sttURL, map[string]any{"provider": "deepgram_flux"})
	waitForCond(t, 10*time.Second, "second transcriber to dial", func() bool {
		return mock.uriFor("/v2/listen") != ""
	})
	time.Sleep(swapSettle)
	swapDelete(t, sttURL, "second STT should still be running")
}
