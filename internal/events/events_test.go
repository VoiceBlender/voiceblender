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
	_ = bus.Subscribe(func(e Event) {
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
		_ = bus.Subscribe(func(e Event) {
			count.Add(1)
		})
	}

	bus.Publish(RoomCreated, &RoomCreatedData{RoomScope: RoomScope{RoomID: "r1"}})

	if got := count.Load(); got != 3 {
		t.Errorf("count = %d, want 3", got)
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	bus := NewBus("test")
	var count1, count2 atomic.Int32

	unsub1 := bus.Subscribe(func(e Event) { count1.Add(1) })
	_ = bus.Subscribe(func(e Event) { count2.Add(1) })

	bus.Publish(RoomCreated, &RoomCreatedData{RoomScope: RoomScope{RoomID: "r1"}})
	if count1.Load() != 1 || count2.Load() != 1 {
		t.Fatalf("before unsub: count1=%d count2=%d, want 1,1", count1.Load(), count2.Load())
	}

	unsub1()
	bus.Publish(RoomCreated, &RoomCreatedData{RoomScope: RoomScope{RoomID: "r2"}})
	if count1.Load() != 1 {
		t.Errorf("after unsub: count1=%d, want 1 (handler should be removed)", count1.Load())
	}
	if count2.Load() != 2 {
		t.Errorf("after unsub: count2=%d, want 2", count2.Load())
	}
}

func TestBus_PublishSetsTimestamp(t *testing.T) {
	bus := NewBus("inst")
	var got Event
	_ = bus.Subscribe(func(e Event) { got = e })

	before := time.Now().UTC()
	bus.Publish(RoomDeleted, &RoomDeletedData{RoomScope: RoomScope{RoomID: "r1"}})
	after := time.Now().UTC()

	if got.Timestamp.Before(before) || got.Timestamp.After(after) {
		t.Errorf("timestamp %v not between %v and %v", got.Timestamp, before, after)
	}
}

func TestBus_PublishAssignsUniqueEventID(t *testing.T) {
	bus := NewBus("inst")
	var got []string
	_ = bus.Subscribe(func(e Event) { got = append(got, e.EventID) })

	for i := 0; i < 3; i++ {
		bus.Publish(RoomDeleted, &RoomDeletedData{RoomScope: RoomScope{RoomID: "r1"}})
	}

	seen := make(map[string]bool)
	for i, id := range got {
		if id == "" {
			t.Fatalf("publish %d: empty event_id", i)
		}
		if seen[id] {
			t.Errorf("publish %d: duplicate event_id %q", i, id)
		}
		seen[id] = true
	}
}

// The id is stamped once before fan-out, so every subscriber must see the same
// value for a given event.
func TestBus_EventIDIdenticalAcrossSubscribers(t *testing.T) {
	bus := NewBus("inst")
	var a, b string
	_ = bus.Subscribe(func(e Event) { a = e.EventID })
	_ = bus.Subscribe(func(e Event) { b = e.EventID })

	bus.Publish(RoomDeleted, &RoomDeletedData{RoomScope: RoomScope{RoomID: "r1"}})

	if a == "" || a != b {
		t.Errorf("subscriber ids = %q / %q, want equal and non-empty", a, b)
	}
}

func TestEvent_MarshalJSON_EventID(t *testing.T) {
	e := Event{
		Type:      LegRinging,
		EventID:   "8f14e45f-ceea-467a-9575-9b0ba1f0e3a1",
		Timestamp: time.Now().UTC(),
		Data:      &LegRingingData{LegScope: LegScope{LegID: "leg-1"}},
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["event_id"] != e.EventID {
		t.Errorf("event_id = %v, want %q", m["event_id"], e.EventID)
	}

	// A zero Event (not published through the bus) omits the key entirely.
	b, err = json.Marshal(Event{Type: LegRinging})
	if err != nil {
		t.Fatalf("marshal zero event: %v", err)
	}
	m = nil
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal zero event: %v", err)
	}
	if _, ok := m["event_id"]; ok {
		t.Error("event_id present on an event with no id")
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

func TestDTMFReceivedData_SeqField(t *testing.T) {
	bus := NewBus("test")
	var got Event
	_ = bus.Subscribe(func(e Event) { got = e })

	bus.Publish(DTMFReceived, &DTMFReceivedData{
		LegScope: LegScope{LegID: "leg-1"},
		Digit:    "5",
		Seq:      42,
	})

	d, ok := got.Data.(*DTMFReceivedData)
	if !ok {
		t.Fatal("expected *DTMFReceivedData")
	}
	if d.Seq != 42 {
		t.Errorf("Seq = %d, want 42", d.Seq)
	}

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["seq"] != float64(42) {
		t.Errorf("JSON seq = %v, want 42", m["seq"])
	}
}

func TestDTMFReceivedData_SeqIndependentPerClosure(t *testing.T) {
	bus := NewBus("test")
	var collected []Event
	_ = bus.Subscribe(func(e Event) { collected = append(collected, e) })

	var seqA, seqB atomic.Uint64
	emitA := func(digit string) {
		seq := seqA.Add(1)
		bus.Publish(DTMFReceived, &DTMFReceivedData{
			LegScope: LegScope{LegID: "leg-A"},
			Digit:    digit,
			Seq:      seq,
		})
	}
	emitB := func(digit string) {
		seq := seqB.Add(1)
		bus.Publish(DTMFReceived, &DTMFReceivedData{
			LegScope: LegScope{LegID: "leg-B"},
			Digit:    digit,
			Seq:      seq,
		})
	}

	emitA("1")
	emitA("2")
	emitB("5")
	emitA("3")
	emitB("6")

	if len(collected) != 5 {
		t.Fatalf("got %d events, want 5", len(collected))
	}

	wantSeqs := []uint64{1, 2, 1, 3, 2}
	for i, e := range collected {
		d := e.Data.(*DTMFReceivedData)
		if d.Seq != wantSeqs[i] {
			t.Errorf("event[%d] Seq = %d, want %d", i, d.Seq, wantSeqs[i])
		}
	}
}

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
	// The signature is captured on the httptest server goroutine and asserted
	// on the test goroutine; hand it over via a channel so the read is
	// synchronized with the write (buffered + non-blocking send tolerates retries).
	sigCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sigCh <- r.Header.Get("X-Signature-256"):
		default:
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	bus := NewBus("test")
	log := slog.Default()
	reg := NewWebhookRegistry(bus, log, srv.URL, "test-secret")
	defer reg.Stop()

	bus.Publish(RoomDeleted, &RoomDeletedData{RoomScope: RoomScope{RoomID: "r1"}})

	var sigHeader string
	select {
	case sigHeader = <-sigCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for webhook delivery")
	}

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
