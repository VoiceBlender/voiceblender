//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// wsEventFrame is a generic JSON envelope received over the event WebSocket.
type wsEventFrame struct {
	Type       string `json:"type"`
	LegID      string `json:"leg_id,omitempty"`
	AppID      string `json:"app_id,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

func dialEventsWS(t *testing.T, inst *testInstance) net.Conn {
	return dialEventsWSFiltered(t, inst, "")
}

func dialEventsWSFiltered(t *testing.T, inst *testInstance, appIDFilter string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := fmt.Sprintf("ws://%s/v1/events/ws", inst.httpAddr)
	if appIDFilter != "" {
		url += "?app_id=" + appIDFilter
	}
	conn, _, _, err := ws.Dial(ctx, url)
	if err != nil {
		t.Fatalf("dial events ws: %v", err)
	}
	return conn
}

func readWSFrame(t *testing.T, conn net.Conn, timeout time.Duration) wsEventFrame {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	data, err := wsutil.ReadServerText(conn)
	if err != nil {
		t.Fatalf("read ws frame: %v", err)
	}
	var f wsEventFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal ws frame: %v (%s)", err, data)
	}
	return f
}

func TestWSEvents_ConnectedAndEvents(t *testing.T) {
	instA := newTestInstance(t, "ws-a")
	instB := newTestInstance(t, "ws-b")

	conn := dialEventsWS(t, instA)
	defer conn.Close()

	// First frame should be {"type":"connected"}.
	f := readWSFrame(t, conn, 5*time.Second)
	if f.Type != "connected" {
		t.Fatalf("first frame type = %q, want connected", f.Type)
	}

	// Originate a call to trigger events.
	resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)

	// Read frames until we get a leg.ringing event.
	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		f = readWSFrame(t, conn, 5*time.Second)
		if f.Type == "leg.ringing" && f.LegID == lv.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("did not receive leg.ringing event over WebSocket")
	}
}

func TestWSEvents_UnknownCommand(t *testing.T) {
	inst := newTestInstance(t, "ws-cmd")
	conn := dialEventsWS(t, inst)
	defer conn.Close()

	// Consume the "connected" frame.
	readWSFrame(t, conn, 5*time.Second)

	// Send an unknown command with request_id.
	msg, _ := json.Marshal(map[string]string{
		"type":       "do_something",
		"request_id": "req-1",
	})
	if err := wsutil.WriteClientText(conn, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read back the error response.
	f := readWSFrame(t, conn, 5*time.Second)
	if f.Type != "error" {
		t.Errorf("type = %q, want error", f.Type)
	}
	if f.RequestID != "req-1" {
		t.Errorf("request_id = %q, want req-1", f.RequestID)
	}
}

func TestWSEvents_StopCommand(t *testing.T) {
	inst := newTestInstance(t, "ws-stop")
	conn := dialEventsWS(t, inst)
	defer conn.Close()

	readWSFrame(t, conn, 5*time.Second)

	msg, _ := json.Marshal(map[string]string{"type": "stop"})
	if err := wsutil.WriteClientText(conn, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	// After stop, the server should close the connection. Next read should fail.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := wsutil.ReadServerText(conn)
	if err == nil {
		t.Fatal("expected read error after stop, got nil")
	}
}

func TestWSEvents_AppIDFilter(t *testing.T) {
	instA := newTestInstance(t, "ws-filter-a")
	instB := newTestInstance(t, "ws-filter-b")

	// Client filtering for app_id=billing only.
	connBilling := dialEventsWSFiltered(t, instA, "^billing$")
	defer connBilling.Close()
	readWSFrame(t, connBilling, 5*time.Second) // consume "connected"

	// Client with no filter (receives everything).
	connAll := dialEventsWS(t, instA)
	defer connAll.Close()
	readWSFrame(t, connAll, 5*time.Second) // consume "connected"

	// Originate a leg with app_id=billing.
	resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"app_id": "billing",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create billing leg: status %d", resp.StatusCode)
	}
	var billingLeg legView
	decodeJSON(t, resp, &billingLeg)

	// Originate a leg with app_id=support.
	resp2 := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"app_id": "support",
	})
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("create support leg: status %d", resp2.StatusCode)
	}
	var supportLeg legView
	decodeJSON(t, resp2, &supportLeg)

	// The unfiltered client should see events for both legs.
	allSeen := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(allSeen) < 2 {
		f := readWSFrame(t, connAll, 5*time.Second)
		if f.Type == "leg.ringing" {
			allSeen[f.LegID] = true
		}
	}
	if !allSeen[billingLeg.ID] {
		t.Error("unfiltered client missed billing leg.ringing")
	}
	if !allSeen[supportLeg.ID] {
		t.Error("unfiltered client missed support leg.ringing")
	}

	// The billing-filtered client should only see billing events.
	billingSeen := false
	supportSeen := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connBilling.SetReadDeadline(time.Now().Add(1 * time.Second))
		data, err := wsutil.ReadServerText(connBilling)
		if err != nil {
			break
		}
		var f wsEventFrame
		if json.Unmarshal(data, &f) == nil {
			if f.LegID == billingLeg.ID {
				billingSeen = true
			}
			if f.LegID == supportLeg.ID {
				supportSeen = true
			}
		}
	}
	if !billingSeen {
		t.Error("filtered client did not receive billing events")
	}
	if supportSeen {
		t.Error("filtered client should NOT receive support events")
	}
}
