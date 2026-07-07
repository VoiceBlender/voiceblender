//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestVSI_CreateLeg_Originates drives an outbound SIP originate over the
// /v1/vsi WebSocket (create_leg) instead of POST /v1/legs, and confirms the
// call connects end-to-end. Guards against create_leg being advertised over
// VSI while the dispatcher returns 501.
func TestVSI_CreateLeg_Originates(t *testing.T) {
	caller := newTestInstance(t, "vsi-createleg-caller")
	callee := newTestInstance(t, "vsi-createleg-callee")

	conn := dialVSI(t, caller)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	f := vsiSend(t, conn, "create_leg", "orig-1", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", callee.sipPort),
		"codecs": []string{"PCMU"},
	})
	if f.Type != "create_leg.result" {
		t.Fatalf("type = %q, want create_leg.result (data=%s)", f.Type, f.Data)
	}
	var v legView
	if err := json.Unmarshal(f.Data, &v); err != nil {
		t.Fatalf("decode create_leg result: %v", err)
	}
	if v.ID == "" {
		t.Fatal("create_leg returned empty leg id")
	}

	in := waitForInboundLeg(t, callee.baseURL(), 5*time.Second)
	ans := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", callee.baseURL(), in.ID), nil)
	ans.Body.Close()

	waitForLegState(t, caller.baseURL(), v.ID, "connected", 5*time.Second)
	waitForLegState(t, callee.baseURL(), in.ID, "connected", 5*time.Second)
}

// TestVSI_CreateLeg_InvalidURI verifies a bad originate surfaces a VSI error
// frame (not a silent accept or a 501).
func TestVSI_CreateLeg_InvalidURI(t *testing.T) {
	inst := newTestInstance(t, "vsi-createleg-baduri")

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "create_leg", "bad-1", map[string]interface{}{
		"type": "sip",
		"uri":  "not-a-sip-uri",
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
