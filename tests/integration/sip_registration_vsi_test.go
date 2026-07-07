//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestVSI_DeleteRegistration exercises the delete_sip_registration command over
// the /v1/vsi WebSocket: a raw client REGISTERs, list_sip_registrations shows
// the binding, delete_sip_registration unbinds it, the list goes empty, and a
// delete for an unknown AOR returns an error frame.
func TestVSI_DeleteRegistration(t *testing.T) {
	inst := newTestInstance(t, "vsi-delreg")
	cli := newRawSIPClient(t, "test-ua")

	if resp := cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600); resp.StatusCode != 200 {
		t.Fatalf("REGISTER status = %d", resp.StatusCode)
	}
	inst.collector.waitForMatch(t, events.SIPRegistrationActive, nil, time.Second)

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	// list shows the binding
	listed := vsiSend(t, conn, "list_sip_registrations", "l1", nil)
	if listed.Type != "list_sip_registrations.result" {
		t.Fatalf("list type = %q", listed.Type)
	}
	if !strings.Contains(string(listed.Data), "sip:alice@vb.test") {
		t.Fatalf("list result missing the binding: %s", listed.Data)
	}

	// delete the AOR
	del := vsiSend(t, conn, "delete_sip_registration", "d1", map[string]string{"aor": "sip:alice@vb.test"})
	if del.Type != "delete_sip_registration.result" {
		t.Fatalf("delete type = %q (data=%s)", del.Type, del.Data)
	}

	// list is now empty
	listed2 := vsiSend(t, conn, "list_sip_registrations", "l2", nil)
	var lr struct {
		Bindings []struct {
			AOR string `json:"aor"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(listed2.Data, &lr); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(lr.Bindings) != 0 {
		t.Errorf("bindings after delete = %d, want 0", len(lr.Bindings))
	}

	// unknown AOR → error frame with 404
	miss := vsiSend(t, conn, "delete_sip_registration", "d2", map[string]string{"aor": "sip:nobody@vb.test"})
	if miss.Type != "error" {
		t.Fatalf("delete-unknown type = %q, want error", miss.Type)
	}
	var e struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(miss.Data, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != 404 {
		t.Errorf("code = %d, want 404", e.Code)
	}
}
