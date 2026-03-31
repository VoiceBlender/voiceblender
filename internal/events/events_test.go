package events

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewBus(t *testing.T) {
	bus := NewBus("test")
	if bus == nil {
		t.Fatal("expected non-nil bus")
	}
}

func TestBus_Subscribe_Publish(t *testing.T) {
	bus := NewBus("test")
	received := make(chan Event, 1)
	bus.Subscribe(func(e Event) {
		received <- e
	})

	bus.Publish(LegRinging, &LegRingingData{LegScope: LegScope{LegID: "leg-1"}})

	select {
	case e := <-received:
		if e.Type != LegRinging {
			t.Errorf("type = %q, want %q", e.Type, LegRinging)
		}
		if e.InstanceID != "test" {
			t.Errorf("instance_id = %q, want test", e.InstanceID)
		}
		if e.Data.GetLegID() != "leg-1" {
			t.Errorf("leg_id = %q, want leg-1", e.Data.GetLegID())
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestBus_MultipleSubscribers(t *testing.T) {
	bus := NewBus("test")
	var count atomic.Int32

	for i := 0; i < 3; i++ {
		bus.Subscribe(func(e Event) {
			count.Add(1)
		})
	}

	bus.Publish(RoomCreated, &RoomCreatedData{RoomScope: RoomScope{RoomID: "r1"}})

	if got := count.Load(); got != 3 {
		t.Errorf("count = %d, want 3", got)
	}
}

func TestBus_PublishSetsTimestamp(t *testing.T) {
	bus := NewBus("inst")
	var got Event
	bus.Subscribe(func(e Event) { got = e })

	before := time.Now().UTC()
	bus.Publish(RoomDeleted, &RoomDeletedData{RoomScope: RoomScope{RoomID: "r1"}})
	after := time.Now().UTC()

	if got.Timestamp.Before(before) || got.Timestamp.After(after) {
		t.Errorf("timestamp %v not between %v and %v", got.Timestamp, before, after)
	}
}

func TestEvent_MarshalJSON(t *testing.T) {
	e := Event{
		Type:       LegConnected,
		Timestamp:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		InstanceID: "inst-1",
		Data:       &LegConnectedData{LegScope: LegScope{LegID: "leg-1"}, LegType: "sip_inbound"},
	}

	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if m["type"] != "leg.connected" {
		t.Errorf("type = %v", m["type"])
	}
	if m["instance_id"] != "inst-1" {
		t.Errorf("instance_id = %v", m["instance_id"])
	}
	if m["leg_id"] != "leg-1" {
		t.Errorf("leg_id = %v", m["leg_id"])
	}
	if m["leg_type"] != "sip_inbound" {
		t.Errorf("leg_type = %v", m["leg_type"])
	}
}

func TestEvent_MarshalJSON_NilData(t *testing.T) {
	e := Event{Type: RoomCreated, Timestamp: time.Now()}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "room.created" {
		t.Errorf("type = %v", m["type"])
	}
}

// --- Scope tests ---

func TestLegScope(t *testing.T) {
	s := LegScope{LegID: "l1"}
	if s.GetLegID() != "l1" {
		t.Errorf("GetLegID = %q", s.GetLegID())
	}
	if s.GetRoomID() != "" {
		t.Errorf("GetRoomID = %q", s.GetRoomID())
	}
}

func TestRoomScope(t *testing.T) {
	s := RoomScope{RoomID: "r1"}
	if s.GetLegID() != "" {
		t.Errorf("GetLegID = %q", s.GetLegID())
	}
	if s.GetRoomID() != "r1" {
		t.Errorf("GetRoomID = %q", s.GetRoomID())
	}
}

func TestLegRoomScope(t *testing.T) {
	s := LegRoomScope{LegID: "l1", RoomID: "r1"}
	if s.GetLegID() != "l1" {
		t.Errorf("GetLegID = %q", s.GetLegID())
	}
	if s.GetRoomID() != "r1" {
		t.Errorf("GetRoomID = %q", s.GetRoomID())
	}
}

// --- WebhookRegistry tests ---

func TestWebhookRegistry_LegWebhook(t *testing.T) {
	bus := NewBus("test")
	log := slog.Default()
	reg := NewWebhookRegistry(bus, log, "", "")
	defer reg.Stop()

	reg.SetLegWebhook("leg-1", "http://example.com/hook", "secret")

	// Verify it's set by publishing an event and checking the dispatch path
	// (internal, but we can verify the webhook is cleared properly)
	reg.ClearLegWebhook("leg-1")
}

func TestWebhookRegistry_RoomWebhook(t *testing.T) {
	bus := NewBus("test")
	log := slog.Default()
	reg := NewWebhookRegistry(bus, log, "", "")
	defer reg.Stop()

	reg.SetRoomWebhook("room-1", "http://example.com/hook", "secret")
	reg.ClearRoomWebhook("room-1")
}

func TestWebhookRegistry_GlobalWebhook(t *testing.T) {
	bus := NewBus("test")
	log := slog.Default()
	reg := NewWebhookRegistry(bus, log, "http://global.example.com", "global-secret")
	defer reg.Stop()

	if reg.globalWebhook == nil {
		t.Fatal("expected global webhook")
	}
	if reg.globalWebhook.URL != "http://global.example.com" {
		t.Errorf("URL = %q", reg.globalWebhook.URL)
	}
}

func TestWebhookRegistry_NoGlobalWebhook(t *testing.T) {
	bus := NewBus("test")
	log := slog.Default()
	reg := NewWebhookRegistry(bus, log, "", "")
	defer reg.Stop()

	if reg.globalWebhook != nil {
		t.Error("expected nil global webhook")
	}
}

func TestWebhookRegistry_Delivery(t *testing.T) {
	received := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf [4096]byte
		n, _ := r.Body.Read(buf[:])
		received <- buf[:n]
		w.WriteHeader(200)
	}))
	defer srv.Close()

	bus := NewBus("test")
	log := slog.Default()
	reg := NewWebhookRegistry(bus, log, srv.URL, "")
	defer reg.Stop()

	bus.Publish(RoomCreated, &RoomCreatedData{RoomScope: RoomScope{RoomID: "r1"}})

	select {
	case body := <-received:
		var m map[string]interface{}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m["type"] != "room.created" {
			t.Errorf("type = %v", m["type"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for webhook delivery")
	}
}

func TestWebhookRegistry_HMAC(t *testing.T) {
	var sigHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigHeader = r.Header.Get("X-Signature-256")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	bus := NewBus("test")
	log := slog.Default()
	reg := NewWebhookRegistry(bus, log, srv.URL, "test-secret")
	defer reg.Stop()

	bus.Publish(RoomDeleted, &RoomDeletedData{RoomScope: RoomScope{RoomID: "r1"}})

	time.Sleep(500 * time.Millisecond)

	if sigHeader == "" {
		t.Fatal("expected X-Signature-256 header")
	}
	if len(sigHeader) < 10 || sigHeader[:7] != "sha256=" {
		t.Errorf("invalid signature header: %q", sigHeader)
	}
}

func TestWebhookRegistry_Stop(t *testing.T) {
	bus := NewBus("test")
	reg := NewWebhookRegistry(bus, slog.Default(), "", "")
	reg.Stop()
	reg.Stop() // double-stop should not panic
}
