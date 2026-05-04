//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestConcurrentDelete_ConnectedLeg hammers DELETE on a connected leg from
// many goroutines concurrently and asserts:
//   - Exactly one leg.disconnected event fires on each side.
//   - At most one DELETE returns 202; the rest return 404 because the leg
//     has been atomically claimed by the winning request.
func TestConcurrentDelete_ConnectedLeg(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, inboundID := establishCall(t, instA, instB)

	const concurrent = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	statusCounts := map[int]int{}

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
			resp.Body.Close()
			mu.Lock()
			statusCounts[resp.StatusCode]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if statusCounts[http.StatusAccepted] != 1 {
		t.Errorf("expected exactly 1 x 202 Accepted, got status histogram=%v", statusCounts)
	}
	if total := statusCounts[http.StatusAccepted] + statusCounts[http.StatusNotFound]; total != concurrent {
		t.Errorf("expected all %d responses to be 202 or 404, got histogram=%v", concurrent, statusCounts)
	}

	// Exactly one leg.disconnected event on side A (the side we deleted from).
	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)
	time.Sleep(300 * time.Millisecond) // settle window for any racing duplicate
	if got := len(instA.collector.matchAll(events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	})); got != 1 {
		t.Errorf("A side leg.disconnected count = %d, want 1", got)
	}

	// Side B also sees exactly one disconnect (from the single BYE).
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inboundID
	}, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	if got := len(instB.collector.matchAll(events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inboundID
	})); got != 1 {
		t.Errorf("B side leg.disconnected count = %d, want 1", got)
	}
}

// TestConcurrentDelete_RingingLeg verifies the same dedup on an unanswered
// inbound leg. Many concurrent DELETEs (one with reason=busy, others without)
// produce one leg.disconnected event.
func TestConcurrentDelete_RingingLeg(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip",
		"uri":  fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	const concurrent = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	statusCounts := map[int]int{}

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var resp *http.Response
			if i%2 == 0 {
				resp = httpDeleteWithBody(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inbound.ID),
					map[string]interface{}{"reason": "busy"})
			} else {
				resp = httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inbound.ID))
			}
			resp.Body.Close()
			mu.Lock()
			statusCounts[resp.StatusCode]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if statusCounts[http.StatusAccepted] != 1 {
		t.Errorf("expected exactly 1 x 202 Accepted, got histogram=%v", statusCounts)
	}

	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	}, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	if got := len(instB.collector.matchAll(events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	})); got != 1 {
		t.Errorf("B side leg.disconnected count = %d, want 1", got)
	}
}
