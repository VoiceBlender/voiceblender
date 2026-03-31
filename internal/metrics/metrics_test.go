package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

func TestNew(t *testing.T) {
	bus := events.NewBus("test")
	c := New(bus)
	if c == nil {
		t.Fatal("expected non-nil collector")
	}
}

func TestHandler_ReturnsMetrics(t *testing.T) {
	bus := events.NewBus("test")
	c := New(bus)

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "voiceblender_active_legs") {
		t.Error("missing voiceblender_active_legs metric")
	}
	if !strings.Contains(body, "voiceblender_active_rooms") {
		t.Error("missing voiceblender_active_rooms metric")
	}
}

func TestMetrics_LegRinging(t *testing.T) {
	bus := events.NewBus("test")
	c := New(bus)

	bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: "leg-1"},
		URI:      "sip:alice@example.com",
	})

	body := getMetrics(t, c)
	if !strings.Contains(body, `voiceblender_legs_total{state="ringing",type="sip_outbound"} 1`) {
		t.Error("expected sip_outbound ringing counter")
	}
}

func TestMetrics_LegRinging_Inbound(t *testing.T) {
	bus := events.NewBus("test")
	c := New(bus)

	bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: "leg-1"},
		From:     "alice",
		To:       "bob",
	})

	body := getMetrics(t, c)
	if !strings.Contains(body, `voiceblender_legs_total{state="ringing",type="sip_inbound"} 1`) {
		t.Error("expected sip_inbound ringing counter")
	}
}

func TestMetrics_LegConnected(t *testing.T) {
	bus := events.NewBus("test")
	c := New(bus)

	bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: "leg-1"},
		LegType:  "sip_inbound",
	})

	body := getMetrics(t, c)
	if !strings.Contains(body, `voiceblender_legs_total{state="connected",type="sip_inbound"} 1`) {
		t.Error("expected connected counter")
	}
}

func TestMetrics_LegDisconnected(t *testing.T) {
	bus := events.NewBus("test")
	c := New(bus)

	// First ringing to set leg type
	bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: "leg-1"},
		From:     "alice",
	})

	bus.Publish(events.LegDisconnected, &events.LegDisconnectedData{
		LegScope: events.LegScope{LegID: "leg-1"},
		CDR: events.CallCDR{
			Reason:           "remote_bye",
			DurationTotal:    30.5,
			DurationAnswered: 25.0,
		},
	})

	body := getMetrics(t, c)
	if !strings.Contains(body, `voiceblender_disconnect_reasons_total{reason="remote_bye",type="sip_inbound"} 1`) {
		t.Error("expected disconnect reason counter")
	}
	if !strings.Contains(body, `voiceblender_legs_total{state="disconnected",type="sip_inbound"} 1`) {
		t.Error("expected disconnected counter")
	}
}

func TestMetrics_RoomCreatedDeleted(t *testing.T) {
	bus := events.NewBus("test")
	c := New(bus)

	bus.Publish(events.RoomCreated, &events.RoomCreatedData{RoomScope: events.RoomScope{RoomID: "r1"}})
	bus.Publish(events.RoomCreated, &events.RoomCreatedData{RoomScope: events.RoomScope{RoomID: "r2"}})
	bus.Publish(events.RoomDeleted, &events.RoomDeletedData{RoomScope: events.RoomScope{RoomID: "r1"}})

	body := getMetrics(t, c)
	if !strings.Contains(body, "voiceblender_active_rooms 1") {
		t.Errorf("expected active_rooms=1, body:\n%s", body)
	}
}

func getMetrics(t *testing.T, c *Collector) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
