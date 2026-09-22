//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
	"github.com/VoiceBlender/voiceblender/internal/config"
)

// filterView is the slice of the leg view this file cares about.
type filterView struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Filters []struct {
		Type   string             `json:"type"`
		Params map[string]float64 `json:"params,omitempty"`
	} `json:"filters,omitempty"`
}

func fetchFilters(t *testing.T, inst *testInstance, legID string) filterView {
	t.Helper()
	resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get leg %s: status %d", legID, resp.StatusCode)
	}
	var v filterView
	decodeJSON(t, resp, &v)
	return v
}

func filterNames(v filterView) []string {
	out := make([]string, 0, len(v.Filters))
	for _, f := range v.Filters {
		out = append(out, f.Type)
	}
	return out
}

func withFilters(chain string) func(*config.Config) {
	return func(c *config.Config) { c.AudioFilters = chain }
}

// TestAudioFilters_ServerDefault checks AUDIO_FILTERS reaches a leg that did
// not ask for anything, on both sides of a call.
func TestAudioFilters_ServerDefault(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "instance-a", withFilters("gain:volume=1"))
	instB := newTestInstanceWithOpts(t, "instance-b", withFilters("gain:volume=1"))
	outbound, inbound := establishCall(t, instA, instB)

	for _, c := range []struct {
		name string
		inst *testInstance
		id   string
	}{{"outbound", instA, outbound}, {"inbound", instB, inbound}} {
		v := fetchFilters(t, c.inst, c.id)
		if got := filterNames(v); len(got) != 1 || got[0] != "gain" {
			t.Errorf("%s leg: filters %v, want [gain]", c.name, got)
			continue
		}
		if v.Filters[0].Params["volume"] != 1 {
			t.Errorf("%s leg: params %v, want volume=1", c.name, v.Filters[0].Params)
			continue
		}
		t.Logf("%-8s leg reports %v with params %v", c.name, filterNames(v), v.Filters[0].Params)
	}
}

// TestAudioFilters_PerLegOverride checks a leg's own chain beats the default,
// and that an explicitly empty chain is an opt-out rather than "use default".
func TestAudioFilters_PerLegOverride(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "instance-a", withFilters("gain:volume=1"))
	instB := newTestInstanceWithOpts(t, "instance-b", withFilters("gain:volume=1"))

	cases := []struct {
		name    string
		filters interface{}
		want    []string
	}{
		{"own chain replaces the default", []map[string]interface{}{
			{"type": "bandpass", "params": map[string]float64{"low_hz": 400, "high_hz": 3000}},
		}, []string{"bandpass"}},
		{"empty array opts out", []map[string]interface{}{}, nil},
	}
	for _, c := range cases {
		body := map[string]interface{}{
			"type":    "sip",
			"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
			"codecs":  []string{"PCMU"},
			"filters": c.filters,
		}
		resp := httpPost(t, instA.baseURL()+"/v1/legs", body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s: create leg status %d", c.name, resp.StatusCode)
		}
		var lv legView
		decodeJSON(t, resp, &lv)

		got := filterNames(fetchFilters(t, instA, lv.ID))
		if len(got) != len(c.want) {
			t.Errorf("%s: filters %v, want %v", c.name, got, c.want)
		} else {
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("%s: filters %v, want %v", c.name, got, c.want)
					break
				}
			}
		}
		t.Logf("%-32s -> %v", c.name, got)
		httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), lv.ID)).Body.Close()
	}
}

// TestAudioFilters_RejectsBadChain checks validation happens at the request,
// not when the first audio frame arrives.
func TestAudioFilters_RejectsBadChain(t *testing.T) {
	inst := newTestInstance(t, "instance-a")
	for _, c := range []struct {
		name    string
		filters []map[string]interface{}
	}{
		{"unknown filter", []map[string]interface{}{{"type": "nosuchfilter"}}},
		{"chain too long", []map[string]interface{}{
			{"type": "gain"}, {"type": "gain"}, {"type": "gain"}, {"type": "gain"}, {"type": "gain"},
		}},
		{"duplicate single-use filter", []map[string]interface{}{{"type": "denoise"}, {"type": "denoise"}}},
		{"bad parameter", []map[string]interface{}{
			{"type": "gain", "params": map[string]float64{"volume": 99}},
		}},
	} {
		resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
			"type":    "sip",
			"uri":     "sip:test@127.0.0.1:65530",
			"codecs":  []string{"PCMU"},
			"filters": c.filters,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", c.name, resp.StatusCode)
		} else {
			t.Logf("%-28s -> 400", c.name)
		}
		resp.Body.Close()
	}
}

// TestAudioFilters_DenoiseDegrades is the degraded-mode contract end to end:
// with no kernel installed, requesting denoise must still place the call, and
// the leg view must report that denoise is not running.
func TestAudioFilters_DenoiseDegrades(t *testing.T) {
	if denoise.Available() {
		t.Skip("a kernel is installed in this process; degraded mode cannot be observed")
	}
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":  []string{"PCMU"},
		"filters": []map[string]interface{}{{"type": "denoise"}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("requesting an unavailable filter must not fail call setup; status %d", resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)

	if got := filterNames(fetchFilters(t, instA, lv.ID)); len(got) != 0 {
		t.Errorf("leg view should report no filters when the kernel is down, got %v", got)
	}
	t.Log("denoise requested with no kernel: call placed, leg reports no filtering")
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), lv.ID)).Body.Close()
}

// TestAudioFilters_DenoiseActive runs a real call with denoise installed and
// the legs in a room, which is what actually builds the chain: a leg outside a
// room has a configured chain but no mixer participant to run it.
func TestAudioFilters_DenoiseActive(t *testing.T) {
	if err := denoise.Install(context.Background(), 0); err != nil {
		t.Fatalf("install denoise: %v", err)
	}
	t.Cleanup(func() { denoise.Shutdown() })

	instA := newTestInstanceWithOpts(t, "instance-a", withFilters("denoise"))
	instB := newTestInstanceWithOpts(t, "instance-b", withFilters("denoise"))

	resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":  []string{"PCMU"},
		"room_id": "denoise-room",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, resp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()
	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)

	if got := filterNames(fetchFilters(t, instA, outbound.ID)); len(got) != 1 || got[0] != "denoise" {
		t.Fatalf("outbound leg: filters %v, want [denoise]", got)
	}

	// The leg is in a room, so its chain is built and holding a pool state.
	deadline := time.Now().Add(5 * time.Second)
	var streams, instances int
	for {
		streams, instances = denoise.Stats()
		if streams > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if streams == 0 {
		t.Fatal("a denoising leg in a room should hold a live pool state")
	}
	t.Logf("denoise pool while the call is up: %d live stream(s) across %d wasm instance(s)", streams, instances)

	// Let real audio flow through the chain, then tear down and confirm the
	// pool reclaimed the state — leaked states walk an instance toward a heap
	// ceiling that nothing clears.
	time.Sleep(500 * time.Millisecond)
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID)).Body.Close()

	deadline = time.Now().Add(5 * time.Second)
	for {
		live, _ := denoise.Stats()
		if live == 0 {
			t.Log("leg torn down; denoise pool reclaimed its state")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("denoise state leaked: %d still live after the leg was deleted", live)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAudioFilters_ChangeMidCall covers changing a live leg's chain: the call
// keeps running, the leg view reports the new chain, and a change that would
// alter the chain's working rate is rejected rather than silently rebuilt.
func TestAudioFilters_ChangeMidCall(t *testing.T) {
	// The kernel has to be up for the rate-conflict case to arise at all:
	// without it denoise is resolved away, leaving an empty chain at the same
	// working rate, and the change would simply succeed.
	if err := denoise.Install(context.Background(), 0); err != nil {
		t.Fatalf("install denoise: %v", err)
	}
	t.Cleanup(func() { denoise.Shutdown() })

	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":  []string{"PCMU"},
		"room_id": "swap-room",
		"filters": []map[string]interface{}{{"type": "robotic"}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	answerResp.Body.Close()
	waitForLegState(t, instA.baseURL(), lv.ID, "connected", 5*time.Second)

	if got := filterNames(fetchFilters(t, instA, lv.ID)); len(got) != 1 || got[0] != "robotic" {
		t.Fatalf("before the change: %v", got)
	}

	// Swap to a different effect at the same working rate.
	put := httpPut(t, fmt.Sprintf("%s/v1/legs/%s/filters", instA.baseURL(), lv.ID),
		map[string]interface{}{"filters": []map[string]interface{}{
			{"type": "pitch", "params": map[string]float64{"semitones": 4}},
		}})
	if put.StatusCode != http.StatusOK {
		t.Fatalf("change filters: status %d", put.StatusCode)
	}
	put.Body.Close()
	if got := filterNames(fetchFilters(t, instA, lv.ID)); len(got) != 1 || got[0] != "pitch" {
		t.Errorf("after the change: %v, want [pitch]", got)
	}
	t.Log("robotic -> pitch swapped on a live call")

	// Clearing stops processing.
	put = httpPut(t, fmt.Sprintf("%s/v1/legs/%s/filters", instA.baseURL(), lv.ID),
		map[string]interface{}{"filters": []map[string]interface{}{}})
	if put.StatusCode != http.StatusOK {
		t.Fatalf("clear filters: status %d", put.StatusCode)
	}
	put.Body.Close()
	if got := filterNames(fetchFilters(t, instA, lv.ID)); len(got) != 0 {
		t.Errorf("after clearing: %v, want none", got)
	}
	t.Log("cleared to no processing on a live call")

	// The call must still be up after all that.
	waitForLegState(t, instA.baseURL(), lv.ID, "connected", 2*time.Second)

	// Enabling denoise moves the chain to 48 kHz. That rebuilds the resamplers
	// behind a fade rather than being refused, so the call survives it.
	put = httpPut(t, fmt.Sprintf("%s/v1/legs/%s/filters", instA.baseURL(), lv.ID),
		map[string]interface{}{"filters": []map[string]interface{}{{"type": "denoise"}}})
	body, _ := io.ReadAll(put.Body)
	put.Body.Close()
	if put.StatusCode != http.StatusOK {
		t.Fatalf("enabling denoise mid-call: status %d (body: %s)", put.StatusCode, strings.TrimSpace(string(body)))
	}
	if got := filterNames(fetchFilters(t, instA, lv.ID)); len(got) != 1 || got[0] != "denoise" {
		t.Errorf("after enabling denoise: %v, want [denoise]", got)
	}
	t.Log("denoise enabled mid-call, across a working-rate change")

	// And off again, back down to the leg's own rate.
	put = httpPut(t, fmt.Sprintf("%s/v1/legs/%s/filters", instA.baseURL(), lv.ID),
		map[string]interface{}{"filters": []map[string]interface{}{{"type": "robotic"}}})
	put.Body.Close()
	if got := filterNames(fetchFilters(t, instA, lv.ID)); len(got) != 1 || got[0] != "robotic" {
		t.Errorf("after disabling denoise: %v, want [robotic]", got)
	}
	t.Log("denoise disabled mid-call, back across the rate change")

	waitForLegState(t, instA.baseURL(), lv.ID, "connected", 2*time.Second)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), lv.ID)).Body.Close()
}
