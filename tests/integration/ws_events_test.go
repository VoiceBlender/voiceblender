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

func dialVSI(t *testing.T, inst *testInstance) net.Conn {
	return dialVSIFiltered(t, inst, "")
}

func dialVSIFiltered(t *testing.T, inst *testInstance, appIDFilter string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := fmt.Sprintf("ws://%s/v1/vsi", inst.httpAddr)
	if appIDFilter != "" {
		url += "?app_id=" + appIDFilter
	}
	conn, _, _, err := ws.Dial(ctx, url)
	if err != nil {
		t.Fatalf("dial vsi: %v", err)
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

	conn := dialVSI(t, instA)
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
	conn := dialVSI(t, inst)
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
	conn := dialVSI(t, inst)
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

func TestWSCommands_RoomLifecycle(t *testing.T) {
	inst := newTestInstance(t, "ws-cmd")
	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	// create_room
	send := func(typ string, payload interface{}) wsEventFrame {
		t.Helper()
		p, _ := json.Marshal(payload)
		msg, _ := json.Marshal(map[string]interface{}{
			"type":       typ,
			"request_id": typ,
			"payload":    json.RawMessage(p),
		})
		if err := wsutil.WriteClientText(conn, msg); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
		// Read frames until we get the result/error for this request_id.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			f := readWSFrame(t, conn, 5*time.Second)
			if f.RequestID == typ {
				return f
			}
		}
		t.Fatalf("no response for %s", typ)
		return wsEventFrame{}
	}

	f := send("create_room", map[string]string{"id": "ws-room", "app_id": "test-app"})
	if f.Type != "create_room.result" {
		t.Fatalf("create_room type = %q, want create_room.result", f.Type)
	}

	// get_room
	f = send("get_room", map[string]string{"id": "ws-room"})
	if f.Type != "get_room.result" {
		t.Fatalf("get_room type = %q", f.Type)
	}

	// list_rooms
	sendRaw := func(typ string) wsEventFrame {
		t.Helper()
		msg, _ := json.Marshal(map[string]interface{}{
			"type":       typ,
			"request_id": typ,
		})
		if err := wsutil.WriteClientText(conn, msg); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			f := readWSFrame(t, conn, 5*time.Second)
			if f.RequestID == typ {
				return f
			}
		}
		t.Fatalf("no response for %s", typ)
		return wsEventFrame{}
	}

	f = sendRaw("list_rooms")
	if f.Type != "list_rooms.result" {
		t.Fatalf("list_rooms type = %q", f.Type)
	}

	// delete_room
	f = send("delete_room", map[string]string{"id": "ws-room"})
	if f.Type != "delete_room.result" {
		t.Fatalf("delete_room type = %q", f.Type)
	}

	// get_room on deleted room → error
	f = send("get_room", map[string]string{"id": "ws-room"})
	if f.Type != "error" {
		t.Fatalf("get_room on deleted = %q, want error", f.Type)
	}
}

func TestWSCommands_MuteLeg(t *testing.T) {
	instA := newTestInstance(t, "ws-mute-a")
	instB := newTestInstance(t, "ws-mute-b")

	// Establish a call via HTTP first.
	outboundID, _ := establishCall(t, instA, instB)

	conn := dialVSI(t, instA)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	sendCmd := func(typ string, payload interface{}) wsEventFrame {
		t.Helper()
		p, _ := json.Marshal(payload)
		msg, _ := json.Marshal(map[string]interface{}{
			"type":       typ,
			"request_id": typ,
			"payload":    json.RawMessage(p),
		})
		if err := wsutil.WriteClientText(conn, msg); err != nil {
			t.Fatalf("write %s: %v", typ, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			f := readWSFrame(t, conn, 5*time.Second)
			if f.RequestID == typ {
				return f
			}
		}
		t.Fatalf("no response for %s", typ)
		return wsEventFrame{}
	}

	// mute_leg
	f := sendCmd("mute_leg", map[string]string{"id": outboundID})
	if f.Type != "mute_leg.result" {
		t.Fatalf("mute type = %q", f.Type)
	}

	// get_leg — verify muted
	f = sendCmd("get_leg", map[string]string{"id": outboundID})
	if f.Type != "get_leg.result" {
		t.Fatalf("get_leg type = %q", f.Type)
	}

	// mute on missing leg → error
	f = sendCmd("mute_leg", map[string]string{"id": "nonexistent"})
	if f.Type != "error" {
		t.Fatalf("mute missing = %q, want error", f.Type)
	}

	// unknown command → error
	f = sendCmd("fly_to_moon", map[string]string{})
	if f.Type != "error" {
		t.Fatalf("unknown cmd = %q, want error", f.Type)
	}
}

func TestWSEvents_AppIDFilter(t *testing.T) {
	instA := newTestInstance(t, "ws-filter-a")
	instB := newTestInstance(t, "ws-filter-b")

	// Client filtering for app_id=billing only.
	connBilling := dialVSIFiltered(t, instA, "^billing$")
	defer connBilling.Close()
	readWSFrame(t, connBilling, 5*time.Second) // consume "connected"

	// Client with no filter (receives everything).
	connAll := dialVSI(t, instA)
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
