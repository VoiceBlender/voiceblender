//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoisegtcrn"
	"github.com/VoiceBlender/voiceblender/internal/audiofilter/voxlock"
)

func installVoxlock(t *testing.T) {
	t.Helper()
	if !voxlock.Compiled {
		t.Skip("built without -tags voxlock")
	}
	model := os.Getenv("VOXLOCK_MODEL")
	if model == "" {
		t.Skip("set VOXLOCK_MODEL to voxlock.bin to run")
	}
	if err := voxlock.Install(voxlock.Config{Model: model, Int8: true, FrameMs: 16, GPU: true, GPUMaxCalls: 8}); err != nil {
		t.Fatalf("install voxlock: %v", err)
	}
	t.Cleanup(func() { voxlock.Shutdown() })
	t.Logf("voxlock backend: %s", voxlock.Backend())
}

func voxlockStats() (int, int) {
	s := voxlock.CurrentStats()
	return s.Streams, s.Instances
}

// TestAudioFilters_VoxlockActive runs voxlock on a live call in a 16 kHz and
// a 48 kHz room, where the chain runs it at 16 kHz, swaps to denoise_gtcrn
// and back mid-call, and checks deleting the leg returns its state.
func TestAudioFilters_VoxlockActive(t *testing.T) {
	installVoxlock(t)
	if err := denoisegtcrn.Install(); err != nil {
		t.Fatalf("install denoise_gtcrn: %v", err)
	}
	t.Cleanup(func() { denoisegtcrn.Shutdown() })

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

			if code := putFilters(t, instA, outboundID, []map[string]interface{}{{"type": "voxlock"}}); code != http.StatusOK {
				t.Fatalf("enable voxlock: status %d", code)
			}
			if got := filterNames(fetchFilters(t, instA, outboundID)); len(got) != 1 || got[0] != "voxlock" {
				t.Fatalf("filters %v, want [voxlock]", got)
			}
			waitStreams(t, "voxlock", voxlockStats, func(n int) bool { return n > 0 })
			time.Sleep(500 * time.Millisecond)
			waitForLegState(t, instA.baseURL(), outboundID, "connected", 2*time.Second)

			if code := putFilters(t, instA, outboundID, []map[string]interface{}{{"type": "denoise_gtcrn"}}); code != http.StatusOK {
				t.Fatalf("swap to denoise_gtcrn: status %d", code)
			}
			waitStreams(t, "gtcrn", denoisegtcrn.Stats, func(n int) bool { return n > 0 })
			waitStreams(t, "voxlock", voxlockStats, func(n int) bool { return n == 0 })

			if code := putFilters(t, instA, outboundID, []map[string]interface{}{{"type": "voxlock"}}); code != http.StatusOK {
				t.Fatalf("swap back to voxlock: status %d", code)
			}
			waitStreams(t, "voxlock", voxlockStats, func(n int) bool { return n > 0 })
			waitForLegState(t, instA.baseURL(), outboundID, "connected", 2*time.Second)

			httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID)).Body.Close()
			waitStreams(t, "voxlock", voxlockStats, func(n int) bool { return n == 0 })
			if s := voxlock.CurrentStats(); s.GPUStreams != 0 {
				t.Errorf("%d GPU streams after the leg went", s.GPUStreams)
			}
		})
	}
}

// TestAudioFilters_VoxlockServerDefault checks AUDIO_FILTERS=voxlock reaches
// a leg that asked for nothing, and that it chains with denoise.
func TestAudioFilters_VoxlockServerDefault(t *testing.T) {
	installVoxlock(t)
	instA := newTestInstanceWithOpts(t, "instance-a", withFilters("bandpass,voxlock"))
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)
	if got := filterNames(fetchFilters(t, instA, outboundID)); len(got) != 2 || got[1] != "voxlock" {
		t.Fatalf("filters %v, want [bandpass voxlock]", got)
	}
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID)).Body.Close()
}

// TestAudioFilters_VoxlockDegrades: with no kernel, as without VOXLOCK_MODEL,
// requesting voxlock still places the call and the leg reports no filtering.
func TestAudioFilters_VoxlockDegrades(t *testing.T) {
	if voxlock.Available() {
		t.Skip("a kernel is installed in this process; degraded mode cannot be observed")
	}
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":  []string{"PCMU"},
		"filters": []map[string]interface{}{{"type": "voxlock"}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("requesting an unavailable filter must not fail call setup; status %d", resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)
	if got := filterNames(fetchFilters(t, instA, lv.ID)); len(got) != 0 {
		t.Errorf("leg view should report no filters when the kernel is down, got %v", got)
	}
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), lv.ID)).Body.Close()
}
