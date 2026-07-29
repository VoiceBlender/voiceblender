//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws/wsutil"
)

type webhookCapture struct {
	header string
	body   map[string]interface{}
}

// TestEventID_WebhookHeaderAndBody registers a leg webhook against a live
// instance and checks that the delivered POST carries an X-Event-Id matching
// the event_id in the flattened envelope.
func TestEventID_WebhookHeaderAndBody(t *testing.T) {
	inst := newTestInstanceWithMetrics(t, "event-id-webhook")

	got := make(chan webhookCapture, 4)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(raw, &body)
		select {
		case got <- webhookCapture{header: r.Header.Get("X-Event-Id"), body: body}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	inst.webhooks.SetLegWebhook("leg-evtid", sink.URL, "")
	inst.bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: "leg-evtid"},
		LegType:  "sip_inbound",
	})

	select {
	case c := <-got:
		if c.header == "" {
			t.Fatal("X-Event-Id header missing on webhook delivery")
		}
		bodyID, _ := c.body["event_id"].(string)
		if bodyID == "" {
			t.Fatalf("event_id missing from webhook body: %v", c.body)
		}
		if bodyID != c.header {
			t.Errorf("body event_id = %q, X-Event-Id = %q; want equal", bodyID, c.header)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
	}

	body := metricsBody(t, inst.baseURL())
	if v := parseGaugeValue(t, body, "voiceblender_webhook_enqueued_total"); v < 1 {
		t.Errorf("voiceblender_webhook_enqueued_total = %v, want >= 1", v)
	}
	if v := parseGaugeValue(t, body, "voiceblender_webhook_deliveries_total"); v < 1 {
		t.Errorf("voiceblender_webhook_deliveries_total = %v, want >= 1", v)
	}
}

// TestEventID_SharedAcrossWebhookAndVSI is the cross-subscriber guarantee: the
// id is stamped once at publish, so a consumer running both transports can
// dedupe one event across them.
func TestEventID_SharedAcrossWebhookAndVSI(t *testing.T) {
	inst := newTestInstanceWithMetrics(t, "event-id-shared")

	got := make(chan webhookCapture, 4)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(raw, &body)
		select {
		case got <- webhookCapture{header: r.Header.Get("X-Event-Id"), body: body}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	conn := dialVSI(t, inst)
	defer conn.Close()
	if f := readWSFrame(t, conn, 5*time.Second); f.Type != "connected" {
		t.Fatalf("first frame = %q, want connected", f.Type)
	}

	inst.webhooks.SetLegWebhook("leg-shared", sink.URL, "")
	inst.bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: "leg-shared"},
		LegType:  "sip_inbound",
	})

	vsiID := readVSIEventID(t, conn, "leg.ringing")
	if vsiID == "" {
		t.Fatal("event_id missing from VSI event frame")
	}

	select {
	case c := <-got:
		if c.header != vsiID {
			t.Errorf("webhook X-Event-Id = %q, VSI event_id = %q; want equal", c.header, vsiID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
	}
}

// readVSIEventID reads frames until one of the given type arrives and returns
// its event_id, skipping keepalives and lifecycle frames.
func readVSIEventID(t *testing.T, conn net.Conn, eventType string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		data, err := wsutil.ReadServerText(conn)
		if err != nil {
			t.Fatalf("read vsi frame: %v", err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal vsi frame: %v (%s)", err, data)
		}
		if m["type"] == eventType {
			id, _ := m["event_id"].(string)
			return id
		}
	}
	t.Fatalf("no %s frame within deadline", eventType)
	return ""
}
