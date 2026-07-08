//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestVSI_Trunk_Lifecycle drives the full outbound-trunk lifecycle over the
// VSI WebSocket (create → list → get → delete), mirroring the REST happy path.
// It guards against the trunk commands being advertised in the metadata /
// asyncapi spec without a working dispatch handler.
func TestVSI_Trunk_Lifecycle(t *testing.T) {
	inst := newTestInstance(t, "vsi-trunk")
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 120})

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	// create_sip_trunk
	created := vsiSend(t, conn, "create_sip_trunk", "create-1", map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri":   fmt.Sprintf("sip:127.0.0.1:%d", reg.port),
			"aor":             "sip:alice@vb.test",
			"password":        "secret",
			"expires_seconds": 600,
		},
	})
	if created.Type != "create_sip_trunk.result" {
		t.Fatalf("create type = %q, want create_sip_trunk.result", created.Type)
	}
	var createData struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(created.Data, &createData); err != nil {
		t.Fatalf("decode create data: %v", err)
	}
	if createData.ID == "" {
		t.Fatal("create_sip_trunk returned empty id")
	}
	if createData.Type != "sip_register" {
		t.Errorf("type = %q, want sip_register", createData.Type)
	}
	id := createData.ID

	// The trunk registers with the fake registrar and the lifecycle event fires.
	inst.collector.waitForMatch(t, events.SIPOutboundRegistrationActive, nil, 3*time.Second)
	if reg.registerCount() < 1 {
		t.Fatal("fake registrar did not receive REGISTER")
	}

	// list_sip_trunks contains the trunk and never leaks the password.
	listed := vsiSend(t, conn, "list_sip_trunks", "list-1", nil)
	if listed.Type != "list_sip_trunks.result" {
		t.Fatalf("list type = %q, want list_sip_trunks.result", listed.Type)
	}
	if !strings.Contains(string(listed.Data), id) {
		t.Errorf("list result missing trunk %s: %s", id, listed.Data)
	}
	if strings.Contains(strings.ToLower(string(listed.Data)), "\"password\"") {
		t.Errorf("list result leaks password: %s", listed.Data)
	}

	// get_sip_trunk returns the single trunk.
	got := vsiSend(t, conn, "get_sip_trunk", "get-1", map[string]string{"id": id})
	if got.Type != "get_sip_trunk.result" {
		t.Fatalf("get type = %q, want get_sip_trunk.result", got.Type)
	}
	if !strings.Contains(string(got.Data), id) {
		t.Errorf("get result missing trunk %s: %s", id, got.Data)
	}

	// delete_sip_trunk acknowledges, then a subsequent get returns an error.
	deleted := vsiSend(t, conn, "delete_sip_trunk", "del-1", map[string]string{"id": id})
	if deleted.Type != "delete_sip_trunk.result" {
		t.Fatalf("delete type = %q, want delete_sip_trunk.result", deleted.Type)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		missing := vsiSend(t, conn, "get_sip_trunk", "get-2", map[string]string{"id": id})
		if missing.Type == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("get after delete still succeeds: %s", missing.Type)
		}
		time.Sleep(75 * time.Millisecond)
	}
}

// TestVSI_Trunk_CreateValidationError verifies a bad create surfaces a VSI
// error frame rather than being silently accepted.
func TestVSI_Trunk_CreateValidationError(t *testing.T) {
	inst := newTestInstance(t, "vsi-trunk-badreq")

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	// Missing sip_register block for a sip_register trunk.
	f := vsiSend(t, conn, "create_sip_trunk", "bad-1", map[string]interface{}{
		"type": "sip_register",
	})
	if f.Type != "error" {
		t.Fatalf("type = %q, want error", f.Type)
	}
	var e struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(f.Data, &e); err != nil {
		t.Fatalf("decode error data: %v", err)
	}
	if e.Code != 400 {
		t.Errorf("code = %d, want 400", e.Code)
	}
}
