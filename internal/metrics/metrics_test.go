package metrics

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestCollector_ImplementsObserver is an interface/registration conformance
// check: it proves each observer method reaches its counter and that every new
// series renders in the /metrics body.
//
// Scope: it calls the methods directly, so it CANNOT prove any production call
// site is wired. The webhook call sites are proven in internal/events
// (TestWebhookRegistry_*) and the VSI call site in internal/api
// (TestVSI_BufferFullIncrementsDroppedCounter). A registered-but-never-
// incremented counter would still satisfy this test.
func TestCollector_ImplementsObserver(t *testing.T) {
	bus := events.NewBus("test")
	c := New(bus)

	// Compile-time conformance is asserted in metrics.go; this pins the
	// runtime behavior of each method.
	var obs events.MetricsObserver = c
	obs.OnWebhookEnqueued("leg-1", "leg.ringing")
	obs.OnWebhookEnqueued("leg-1", "leg.ringing")
	obs.OnWebhookDropped("leg-1", "leg.ringing")
	obs.OnWebhookDelivered("leg-1", "leg.ringing", "success")
	obs.OnWebhookDelivered("leg-1", "leg.ringing", "request_error")
	c.ObserveVSIDropped()

	// Assert by value off the rendered body. (prometheus/testutil would read
	// the counters directly, but it pulls in module requirements this repo
	// does not have; the body is the same source of truth and needs no new
	// dependency.)
	body := getMetrics(t, c)
	for _, tc := range []struct {
		sample string
		want   string
	}{
		{"voiceblender_webhook_enqueued_total", "2"},
		{"voiceblender_webhook_dropped_total", "1"},
		{`voiceblender_webhook_deliveries_total{outcome="success"}`, "1"},
		{`voiceblender_webhook_deliveries_total{outcome="request_error"}`, "1"},
		{"voiceblender_vsi_events_dropped_total", "1"},
	} {
		want := tc.sample + " " + tc.want
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in /metrics body:\n%s", want, body)
		}
	}
}

// TestCollector_ObservesRealWebhookRegistry joins the two halves of the feature
// that every other test leaves apart: TestWebhookRegistry_* prove webhook.go
// calls *an interface* (they pass a fake observer), and
// TestCollector_ImplementsObserver proves each method reaches its counter (it
// calls the methods directly). Neither binds a real *Collector to a real
// WebhookRegistry, so a Collector that satisfies the interface but never gets
// attached would satisfy both. This drives a real event through a real registry
// into a real counter and reads the value back off /metrics.
//
// Scope, honestly: this closes the composition gap, not the main() gap. It does
// not make the SetMetricsObserver call in cmd/voiceblender deletable-and-red —
// no main() wiring in this repo is test-covered.
func TestCollector_ObservesRealWebhookRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bus := events.NewBus("test")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The global-webhook argument is what routes the event, so no SetLegWebhook.
	reg := events.NewWebhookRegistry(bus, log, srv.URL, "")
	defer reg.Stop()

	c := New(bus)
	reg.SetMetricsObserver(c)

	bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: "leg-1"},
	})

	// enqueue runs inline on the publisher's goroutine, so enqueued is already
	// counted here; only the delivery leg is handed to a worker goroutine.
	body := getMetrics(t, c)
	if !strings.Contains(body, "voiceblender_webhook_enqueued_total 1") {
		t.Errorf("enqueued counter did not move, body:\n%s", body)
	}

	const wantDelivered = `voiceblender_webhook_deliveries_total{outcome="success"} 1`
	deadline := time.Now().Add(5 * time.Second)
	for {
		body = getMetrics(t, c)
		if strings.Contains(body, wantDelivered) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("deliveries counter did not move: want %q, body:\n%s", wantDelivered, body)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getMetrics(t *testing.T, c *Collector) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
