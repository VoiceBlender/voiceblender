//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
)

type byteCountTap struct{ n atomic.Int64 }

func (b *byteCountTap) Write(p []byte) (int, error) {
	b.n.Add(int64(len(p)))
	return len(p), nil
}

// TestAMR_DTX places an AMR call between two instances. While the caller is
// silent its encoder runs DTX, so the far leg receives only SID updates, yet
// still decodes one frame per 20 ms (comfort noise). Playing audio brings the
// packet rate back to one per frame.
func TestAMR_DTX(t *testing.T) {
	for _, tc := range []struct {
		name        string
		codec       string
		newInstance func(*testing.T, string, bool) *testInstance
		pcmPerSec   int64
	}{
		{"AMR-WB", "AMR-WB", newAMRWBInstance, 16000 * 2},
		{"AMR-NB", "AMR-NB", newAMRNBInstance, 8000 * 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instA := tc.newInstance(t, "dtx-tx-"+tc.name, true)
			instB := tc.newInstance(t, "dtx-rx-"+tc.name, true)

			createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
				"type":   "sip",
				"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
				"codecs": []string{tc.codec},
			})
			if createResp.StatusCode != http.StatusCreated {
				t.Fatalf("create leg: status %d", createResp.StatusCode)
			}
			var outbound legView
			decodeJSON(t, createResp, &outbound)

			inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
			answerResp := httpPost(t,
				fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
				map[string]interface{}{"codec": tc.codec},
			)
			if answerResp.StatusCode != http.StatusAccepted {
				body, _ := io.ReadAll(answerResp.Body)
				answerResp.Body.Close()
				t.Fatalf("answer: status %d, body=%s", answerResp.StatusCode, body)
			}
			answerResp.Body.Close()
			waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
			waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

			rawLeg, ok := instB.legMgr.Get(inbound.ID)
			if !ok {
				t.Fatalf("inbound leg %s not found", inbound.ID)
			}
			sipLeg, ok := rawLeg.(*leg.SIPLeg)
			if !ok {
				t.Fatalf("inbound leg is %T, want *leg.SIPLeg", rawLeg)
			}

			// Let the caller's VAD hangover run out.
			time.Sleep(time.Second)

			tap := &byteCountTap{}
			sipLeg.SetInTap(tap)
			t.Cleanup(sipLeg.ClearInTap)
			const window = 2 * time.Second
			startPkts := sipLeg.RTPStats().PacketsReceived
			time.Sleep(window)
			silentPkts := sipLeg.RTPStats().PacketsReceived - startPkts
			silentBytes := tap.n.Load()

			// 100 frames in the window: DTX sends a SID update every 8th.
			if silentPkts > 30 {
				t.Errorf("silent caller: %d packets in %v, want DTX (SID updates only)", silentPkts, window)
			}
			wantBytes := tc.pcmPerSec * int64(window/time.Millisecond) / 1000
			if silentBytes < wantBytes*8/10 {
				t.Errorf("silent caller: far leg decoded %d PCM bytes in %v, want ~%d (one frame per 20 ms)",
					silentBytes, window, wantBytes)
			}
			if lost := sipLeg.RTPStats().PacketsLost; lost != 0 {
				t.Errorf("DTX reported as %d lost packets", lost)
			}

			playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instA.baseURL(), outbound.ID),
				map[string]interface{}{"tone": "us_dial", "repeat": -1, "volume": 0})
			if playResp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(playResp.Body)
				playResp.Body.Close()
				t.Fatalf("play tone: status %d, body=%s", playResp.StatusCode, body)
			}
			playResp.Body.Close()

			time.Sleep(300 * time.Millisecond)
			startPkts = sipLeg.RTPStats().PacketsReceived
			time.Sleep(time.Second)
			if active := sipLeg.RTPStats().PacketsReceived - startPkts; active < 35 {
				t.Errorf("playing caller: %d packets in 1 s, want ~50", active)
			}
		})
	}
}
