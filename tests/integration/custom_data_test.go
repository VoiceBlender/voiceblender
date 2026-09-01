//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
)

// collectCustomData reads event frames until the wanted event type arrives,
// returning that frame's custom_data. Frames for other legs are skipped.
func waitForLegEvent(t *testing.T, conn net.Conn, legID, eventType string, timeout time.Duration) wsEventFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, ok := tryReadWSFrame(conn, time.Until(deadline))
		if !ok {
			break
		}
		if f.Type == eventType && f.LegID == legID {
			return f
		}
	}
	t.Fatalf("timed out waiting for %s on leg %s", eventType, legID)
	return wsEventFrame{}
}

// An outbound leg created with custom_data must carry it on every event,
// including leg.disconnected, which is published after the leg has already
// been removed from the manager.
func TestCustomData_OutboundLegEvents(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	conn := dialVSI(t, instA)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // "connected"

	want := `{"order_id":"A-991","tenant":42}`
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":        "sip",
		"uri":         fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":      []string{"PCMU"},
		"custom_data": json.RawMessage(want),
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	// Echoed on the create response.
	if string(outbound.CustomData) != want {
		t.Fatalf("create response custom_data = %s, want %s", outbound.CustomData, want)
	}

	// And on GET /v1/legs/{id}.
	var fetched legView
	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID))
	decodeJSON(t, getResp, &fetched)
	if string(fetched.CustomData) != want {
		t.Fatalf("GET custom_data = %s, want %s", fetched.CustomData, want)
	}

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	answerResp.Body.Close()
	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)

	for _, evt := range []string{"leg.ringing", "leg.connected"} {
		f := waitForLegEvent(t, conn, outbound.ID, evt, 5*time.Second)
		if string(f.CustomData) != want {
			t.Fatalf("%s custom_data = %s, want %s", evt, f.CustomData, want)
		}
	}

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID))
	delResp.Body.Close()

	f := waitForLegEvent(t, conn, outbound.ID, "leg.disconnected", 5*time.Second)
	if string(f.CustomData) != want {
		t.Fatalf("leg.disconnected custom_data = %s, want %s", f.CustomData, want)
	}
}

// An inbound leg has no custom_data at leg.ringing — the app has not seen the
// call yet — but carries it from the answer onwards.
func TestCustomData_InboundAttachedAtAnswer(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	conn := dialVSI(t, instB)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	ringing := waitForLegEvent(t, conn, inbound.ID, "leg.ringing", 5*time.Second)
	if len(ringing.CustomData) != 0 {
		t.Fatalf("inbound leg.ringing carried custom_data: %s", ringing.CustomData)
	}

	want := `{"crm":"case-7"}`
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
		map[string]interface{}{"custom_data": json.RawMessage(want)})
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	f := waitForLegEvent(t, conn, inbound.ID, "leg.connected", 5*time.Second)
	if string(f.CustomData) != want {
		t.Fatalf("leg.connected custom_data = %s, want %s", f.CustomData, want)
	}

	var fetched legView
	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inbound.ID))
	decodeJSON(t, getResp, &fetched)
	if string(fetched.CustomData) != want {
		t.Fatalf("GET custom_data = %s, want %s", fetched.CustomData, want)
	}
}

func TestCustomData_OversizeRejected(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "instance-a", func(c *config.Config) {
		c.CustomDataMaxBytes = 32
	})

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":        "sip",
		"uri":         "sip:test@127.0.0.1:5060",
		"custom_data": map[string]string{"pad": strings.Repeat("x", 200)},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if !strings.Contains(body.Error, "exceeds limit of 32") {
		t.Fatalf("error = %q", body.Error)
	}
}

// create_leg over VSI shares CreateLegRequest with the REST route, so the
// field and its limit must behave identically on both transports.
func TestCustomData_VSICreateLeg(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "instance-a", func(c *config.Config) {
		c.CustomDataMaxBytes = 64
	})
	instB := newTestInstance(t, "instance-b")

	conn := dialVSI(t, instA)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	want := `{"order_id":"V-1"}`
	res := vsiSend(t, conn, "create_leg", "req-1", map[string]interface{}{
		"type":        "sip",
		"uri":         fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":      []string{"PCMU"},
		"custom_data": json.RawMessage(want),
	})
	if res.Type != "create_leg.result" {
		t.Fatalf("type = %s, data = %s", res.Type, res.Data)
	}
	var view legView
	if err := json.Unmarshal(res.Data, &view); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if string(view.CustomData) != want {
		t.Fatalf("custom_data = %s, want %s", view.CustomData, want)
	}

	// Same cap over VSI, surfaced as an error frame rather than a result.
	res = vsiSend(t, conn, "create_leg", "req-2", map[string]interface{}{
		"type":        "sip",
		"uri":         fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"custom_data": map[string]string{"pad": strings.Repeat("x", 200)},
	})
	if res.Type != "error" {
		t.Fatalf("type = %s, want error (data %s)", res.Type, res.Data)
	}
	if !strings.Contains(string(res.Data), "exceeds limit of 64") {
		t.Fatalf("data = %s", res.Data)
	}
}
