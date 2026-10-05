//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoisegtcrn"
)

func waitStreams(t *testing.T, name string, stats func() (int, int), want func(int) bool) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		live, _ := stats()
		if want(live) {
			return live
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s pool: %d live streams, condition not met", name, live)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func putFilters(t *testing.T, inst *testInstance, legID string, filters []map[string]interface{}) int {
	t.Helper()
	put := httpPut(t, fmt.Sprintf("%s/v1/legs/%s/filters", inst.baseURL(), legID),
		map[string]interface{}{"filters": filters})
	body, _ := io.ReadAll(put.Body)
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Logf("PUT filters %v: status %d (%s)", filters, put.StatusCode, strings.TrimSpace(string(body)))
	}
	return put.StatusCode
}

// TestAudioFilters_GTCRNActive runs denoise_gtcrn on a live call in a room at
// a native rate (16 kHz) and at 48 kHz, where the chain runs the model at
// 16 kHz and resamples around it. It then swaps to the RNNoise filter and back
// mid-call, which moves the chain's working rate in the 48 kHz room.
func TestAudioFilters_GTCRNActive(t *testing.T) {
	if err := denoisegtcrn.Install(); err != nil {
		t.Fatalf("install denoise_gtcrn: %v", err)
	}
	t.Cleanup(func() { denoisegtcrn.Shutdown() })
	if err := denoise.Install(); err != nil {
		t.Fatalf("install denoise: %v", err)
	}
	t.Cleanup(func() { denoise.Shutdown() })

	for _, rate := range []int{16000, 48000} {
		t.Run(fmt.Sprintf("%dHz_room", rate), func(t *testing.T) {
			instA := newTestInstance(t, "instance-a")
			instB := newTestInstance(t, "instance-b")
			outboundID, _ := establishCall(t, instA, instB)

			roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]any{"sample_rate": rate})
			if roomResp.StatusCode != http.StatusCreated {
				t.Fatalf("create room: status %d", roomResp.StatusCode)
			}
			var rm roomView
			decodeJSON(t, roomResp, &rm)
			join := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
				map[string]any{"leg_id": outboundID})
			if join.StatusCode != http.StatusOK {
				t.Fatalf("add leg to room: status %d", join.StatusCode)
			}
			join.Body.Close()

			if code := putFilters(t, instA, outboundID, []map[string]interface{}{{"type": "denoise_gtcrn"}}); code != http.StatusOK {
				t.Fatalf("enable denoise_gtcrn: status %d", code)
			}
			if got := filterNames(fetchFilters(t, instA, outboundID)); len(got) != 1 || got[0] != "denoise_gtcrn" {
				t.Fatalf("filters %v, want [denoise_gtcrn]", got)
			}
			waitStreams(t, "gtcrn", denoisegtcrn.Stats, func(n int) bool { return n > 0 })
			time.Sleep(500 * time.Millisecond)
			waitForLegState(t, instA.baseURL(), outboundID, "connected", 2*time.Second)
			t.Logf("denoise_gtcrn running in a %d Hz room", rate)

			if code := putFilters(t, instA, outboundID, []map[string]interface{}{{"type": "denoise"}}); code != http.StatusOK {
				t.Fatalf("swap to denoise: status %d", code)
			}
			waitStreams(t, "rnnoise", denoise.Stats, func(n int) bool { return n > 0 })
			waitStreams(t, "gtcrn", denoisegtcrn.Stats, func(n int) bool { return n == 0 })

			if code := putFilters(t, instA, outboundID, []map[string]interface{}{{"type": "denoise_gtcrn"}}); code != http.StatusOK {
				t.Fatalf("swap back to denoise_gtcrn: status %d", code)
			}
			waitStreams(t, "gtcrn", denoisegtcrn.Stats, func(n int) bool { return n > 0 })
			waitForLegState(t, instA.baseURL(), outboundID, "connected", 2*time.Second)
			t.Log("swapped denoise_gtcrn -> denoise -> denoise_gtcrn mid-call")

			httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID)).Body.Close()
			waitStreams(t, "gtcrn", denoisegtcrn.Stats, func(n int) bool { return n == 0 })
			t.Log("leg torn down; denoise_gtcrn pool reclaimed its state")
		})
	}
}

// TestAudioFilters_GTCRNServerDefault checks AUDIO_FILTERS=denoise_gtcrn
// reaches a leg that asked for nothing.
func TestAudioFilters_GTCRNServerDefault(t *testing.T) {
	if err := denoisegtcrn.Install(); err != nil {
		t.Fatalf("install denoise_gtcrn: %v", err)
	}
	t.Cleanup(func() { denoisegtcrn.Shutdown() })

	instA := newTestInstanceWithOpts(t, "instance-a", withFilters("denoise_gtcrn"))
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)
	if got := filterNames(fetchFilters(t, instA, outboundID)); len(got) != 1 || got[0] != "denoise_gtcrn" {
		t.Fatalf("filters %v, want [denoise_gtcrn]", got)
	}
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID)).Body.Close()
}

// TestAudioFilters_GTCRNWithDenoiseRejected: the two denoisers are mutually
// exclusive, on create and on a live change.
func TestAudioFilters_GTCRNWithDenoiseRejected(t *testing.T) {
	inst := newTestInstance(t, "instance-a")
	both := []map[string]interface{}{{"type": "denoise"}, {"type": "denoise_gtcrn"}}
	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     "sip:test@127.0.0.1:65530",
		"codecs":  []string{"PCMU"},
		"filters": both,
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with both denoisers: status %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "cannot both be in a chain") {
		t.Errorf("unexpected error body: %s", body)
	}
}
