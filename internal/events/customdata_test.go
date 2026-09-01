package events

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestCustomData_MarshalRoundTripIsVerbatim(t *testing.T) {
	// A float64 round trip would mangle this integer; CustomData must not.
	raw := `{"tenant":12345678901234567890,"zzz":1,"aaa":2}`
	var cd CustomData
	if err := json.Unmarshal([]byte(raw), &cd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(cd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != raw {
		t.Fatalf("got %s, want %s", out, raw)
	}
}

func TestCustomData_EmptyMarshalsToNull(t *testing.T) {
	out, err := json.Marshal(CustomData(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != "null" {
		t.Fatalf("got %s, want null", out)
	}
}

func TestCustomData_NonObjectValues(t *testing.T) {
	for _, raw := range []string{`[1,2,3]`, `"hello"`, `42`, `true`, `null`} {
		var cd CustomData
		if err := json.Unmarshal([]byte(raw), &cd); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if string(cd) != raw {
			t.Fatalf("got %s, want %s", cd, raw)
		}
	}
}

func TestCustomData_IsNull(t *testing.T) {
	if !CustomData(`null`).IsNull() {
		t.Fatal("null should report IsNull")
	}
	if CustomData(`{}`).IsNull() || CustomData(nil).IsNull() {
		t.Fatal("non-null values must not report IsNull")
	}
}

func TestCustomDataRegistry_SetGetClear(t *testing.T) {
	r := NewCustomDataRegistry()
	if got := r.Leg("leg-1"); got != nil {
		t.Fatalf("unset leg returned %s", got)
	}
	r.SetLeg("leg-1", CustomData(`{"a":1}`))
	if got := string(r.Leg("leg-1")); got != `{"a":1}` {
		t.Fatalf("got %s", got)
	}
	r.SetLeg("leg-1", CustomData(`{"a":2}`))
	if got := string(r.Leg("leg-1")); got != `{"a":2}` {
		t.Fatalf("replace: got %s", got)
	}
	r.ClearLeg("leg-1")
	if got := r.Leg("leg-1"); got != nil {
		t.Fatalf("after clear: got %s", got)
	}
	// Empty IDs are ignored rather than creating a shared bucket.
	r.SetLeg("", CustomData(`{"a":1}`))
	if got := r.Leg(""); got != nil {
		t.Fatalf("empty id returned %s", got)
	}
}

// SetLeg must copy, so a caller reusing its buffer cannot mutate stored data.
func TestCustomDataRegistry_SetLegCopies(t *testing.T) {
	r := NewCustomDataRegistry()
	buf := CustomData(`{"a":1}`)
	r.SetLeg("leg-1", buf)
	buf[2] = 'X'
	if got := string(r.Leg("leg-1")); got != `{"a":1}` {
		t.Fatalf("stored data aliased caller buffer: %s", got)
	}
}

func TestCustomDataRegistry_Concurrent(t *testing.T) {
	r := NewCustomDataRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "leg-" + strings.Repeat("x", i%5)
			r.SetLeg(id, CustomData(`{"a":1}`))
			_ = r.Leg(id)
			r.ClearLeg(id)
		}(i)
	}
	wg.Wait()
}

func TestBus_PublishStampsCustomData(t *testing.T) {
	b := NewBus("inst-1")
	b.CustomData.SetLeg("leg-1", CustomData(`{"order":"A-1"}`))

	var got []Event
	b.Subscribe(func(e Event) { got = append(got, e) })

	b.Publish(LegConnected, &LegConnectedData{LegScope: LegScope{LegID: "leg-1"}})
	b.Publish(LegConnected, &LegConnectedData{LegScope: LegScope{LegID: "leg-2"}})
	b.Publish(RoomCreated, &RoomCreatedData{RoomScope: RoomScope{RoomID: "room-1"}})

	if len(got) != 3 {
		t.Fatalf("got %d events", len(got))
	}
	if s := string(got[0].CustomData); s != `{"order":"A-1"}` {
		t.Fatalf("registered leg: got %q", s)
	}
	if got[1].CustomData != nil {
		t.Fatalf("unregistered leg: got %s", got[1].CustomData)
	}
	if got[2].CustomData != nil {
		t.Fatalf("room-scoped event: got %s", got[2].CustomData)
	}
}

func TestEvent_MarshalJSON_CustomData(t *testing.T) {
	e := Event{
		Type:       LegConnected,
		Data:       &LegConnectedData{LegScope: LegScope{LegID: "leg-1"}, LegType: "sip_inbound"},
		CustomData: CustomData(`{"order":"A-1"}`),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(m["custom_data"]) != `{"order":"A-1"}` {
		t.Fatalf("custom_data: got %s", m["custom_data"])
	}
	if string(m["leg_id"]) != `"leg-1"` {
		t.Fatalf("leg_id: got %s", m["leg_id"])
	}

	e.CustomData = nil
	b, _ = json.Marshal(e)
	if strings.Contains(string(b), "custom_data") {
		t.Fatalf("empty custom_data must be omitted: %s", b)
	}
}
