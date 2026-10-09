//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo/sip"
)

// TestSIPInbound_CustomHeadersAnyCase pins that X- and P- headers on an inbound
// INVITE are captured whatever their case, keyed as received, and that a
// lower-case x-app-id still scopes the leg.
func TestSIPInbound_CustomHeadersAnyCase(t *testing.T) {
	inst := newTestInstance(t, "sip-custom-headers")
	cli := newRawSIPClient(t, "custom-headers-ua")

	want := map[string]string{
		"X-Upper":             "upper",
		"x-lower":             "lower",
		"P-Asserted-Identity": "<sip:alice@carrier.example>",
		"p-charging-vector":   "icid-value=abc123",
		"x-app-id":            "tenant-a",
	}
	extra := []sip.Header{sip.NewHeader("Subject", "not captured")}
	for name, value := range want {
		extra = append(extra, sip.NewHeader(name, value))
	}
	sendRawInvite(t, cli, inst.sipPort, "headers", extra...)

	ev := inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegRingingData)
		return ok && strings.Contains(d.To, "headers")
	}, 5*time.Second)
	d := ev.Data.(*events.LegRingingData)
	t.Cleanup(func() {
		resp := httpDelete(t, inst.baseURL()+"/v1/legs/"+d.LegID)
		resp.Body.Close()
	})

	if d.AppID != "tenant-a" {
		t.Errorf("leg.ringing app_id = %q, want tenant-a", d.AppID)
	}
	assertCustomHeaders(t, "leg.ringing sip_headers", d.SIPHeaders, want)

	resp := httpGet(t, inst.baseURL()+"/v1/legs/"+d.LegID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET leg = %d, want 200", resp.StatusCode)
	}
	var view struct {
		AppID      string            `json:"app_id"`
		SIPHeaders map[string]string `json:"sip_headers"`
		Headers    map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode leg view: %v", err)
	}
	if view.AppID != "tenant-a" {
		t.Errorf("leg view app_id = %q, want tenant-a", view.AppID)
	}
	assertCustomHeaders(t, "leg view headers", view.Headers, want)
	assertCustomHeaders(t, "leg view sip_headers", view.SIPHeaders, want)
}

func assertCustomHeaders(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s[%q] = %q, want %q", what, name, got[name], value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s = %v, want exactly %v", what, got, want)
	}
}
