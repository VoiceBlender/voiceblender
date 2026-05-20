package room

import (
	"reflect"
	"sort"
	"testing"
)

// silenceReader returns silent PCM bytes forever.
type silenceReader struct{}

func (silenceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// discardWriter discards all writes (and never blocks).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// newAudioMockLeg returns a mockLeg whose AudioReader/AudioWriter are
// non-nil so it actually enters the room mixer (and Hears can be inspected).
func newAudioMockLeg(id string) *mockLeg {
	m := newMockLeg(id)
	m.reader = silenceReader{}
	m.writer = discardWriter{}
	return m
}

// sortedKeys returns the keys of a string set as a sorted slice for stable
// comparisons.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestRoom_AddLegWithRole_Supervisor_NoBleed(t *testing.T) {
	r := NewRoom("r", "", 16000, newTestLog())
	r.SetRoutingMatrix(map[string][]string{
		"customer":   {"agent"},
		"agent":      {"customer", "supervisor"},
		"supervisor": {"customer", "agent"},
	})

	cust := newAudioMockLeg("cust")
	agent := newAudioMockLeg("agent")
	sup := newAudioMockLeg("sup")

	r.AddLegWithRole(cust, "customer")
	r.AddLegWithRole(agent, "agent")
	r.AddLegWithRole(sup, "supervisor")

	// Verify each leg's mixer-side allow-set matches the matrix resolution.
	checkHears := func(legID string, want []string) {
		t.Helper()
		hears, ok := r.Mixer().ParticipantHears(legID)
		if !ok {
			t.Fatalf("%s: not in mixer", legID)
		}
		got := sortedKeys(hears)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s.Hears = %v, want %v", legID, got, want)
		}
	}

	// customer hears only the agent — supervisor must NOT be in the set.
	checkHears("cust", []string{"agent"})
	// agent hears customer + supervisor.
	checkHears("agent", []string{"cust", "sup"})
	// supervisor hears customer + agent.
	checkHears("sup", []string{"cust", "agent"})
}

func TestRoom_SetLegRole_RecomputesAllowSets(t *testing.T) {
	r := NewRoom("r", "", 16000, newTestLog())
	r.SetRoutingMatrix(map[string][]string{
		"agent":      {"customer"},
		"supervisor": {"customer", "agent"},
		"customer":   {"agent"},
	})

	r.AddLegWithRole(newAudioMockLeg("cust"), "customer")
	r.AddLegWithRole(newAudioMockLeg("a"), "agent")

	// Initially agent("a") only hears "cust" (no supervisor in the room).
	hears, _ := r.Mixer().ParticipantHears("a")
	if !reflect.DeepEqual(sortedKeys(hears), []string{"cust"}) {
		t.Fatalf("agent initial Hears = %v, want [cust]", sortedKeys(hears))
	}

	// Promote agent("a") to "supervisor" mid-call. Now "a" should hear
	// both customer and any other agent (none yet) — so still just cust.
	r.AddLegWithRole(newAudioMockLeg("a2"), "agent")
	if _, ok := r.SetLegRole("a", "supervisor"); !ok {
		t.Fatal("SetLegRole returned not-found")
	}

	hears, _ = r.Mixer().ParticipantHears("a")
	want := []string{"a2", "cust"}
	if got := sortedKeys(hears); !reflect.DeepEqual(got, want) {
		t.Errorf("after role change a.Hears = %v, want %v", got, want)
	}
}

func TestRoom_UnroledLegIsFullMesh(t *testing.T) {
	r := NewRoom("r", "", 16000, newTestLog())
	r.SetRoutingMatrix(map[string][]string{
		"agent": {"customer"},
	})

	r.AddLegWithRole(newAudioMockLeg("cust"), "customer")
	r.AddLegWithRole(newAudioMockLeg("a"), "agent")
	// "obs" has no role — it must default to full mesh (nil Hears).
	r.AddLegWithRole(newAudioMockLeg("obs"), "")

	hears, ok := r.Mixer().ParticipantHears("obs")
	if !ok {
		t.Fatal("obs not in mixer")
	}
	if hears != nil {
		t.Errorf("unroled leg should have nil Hears (full mesh); got %v", hears)
	}

	// "a" with role "agent" has matrix["agent"]=["customer"], so it hears
	// only the customer — explicitly NOT the unroled observer.
	hears, _ = r.Mixer().ParticipantHears("a")
	if !reflect.DeepEqual(sortedKeys(hears), []string{"cust"}) {
		t.Errorf("agent should hear customer only (matrix-routed); got %v", sortedKeys(hears))
	}
}

func TestRoom_RemoveLegPrunesAllowSets(t *testing.T) {
	r := NewRoom("r", "", 16000, newTestLog())
	r.SetRoutingMatrix(map[string][]string{
		"agent": {"customer", "supervisor"},
	})
	r.AddLegWithRole(newAudioMockLeg("cust"), "customer")
	r.AddLegWithRole(newAudioMockLeg("a"), "agent")
	r.AddLegWithRole(newAudioMockLeg("sup"), "supervisor")

	hears, _ := r.Mixer().ParticipantHears("a")
	if !reflect.DeepEqual(sortedKeys(hears), []string{"cust", "sup"}) {
		t.Fatalf("agent.Hears = %v, want [cust sup]", sortedKeys(hears))
	}

	r.RemoveLeg("sup")
	hears, _ = r.Mixer().ParticipantHears("a")
	if !reflect.DeepEqual(sortedKeys(hears), []string{"cust"}) {
		t.Errorf("after supervisor leaves, agent.Hears = %v, want [cust]", sortedKeys(hears))
	}
}

func TestRoom_UpdateRoutingRow_ClearRestoresFullMesh(t *testing.T) {
	r := NewRoom("r", "", 16000, newTestLog())
	r.SetRoutingMatrix(map[string][]string{
		"agent": {"customer"},
	})
	r.AddLegWithRole(newAudioMockLeg("cust"), "customer")
	r.AddLegWithRole(newAudioMockLeg("a"), "agent")

	// Initially agent has a whitelist.
	hears, _ := r.Mixer().ParticipantHears("a")
	if hears == nil {
		t.Fatal("agent should have an explicit whitelist initially")
	}

	// Clearing the agent row returns it to full-mesh (nil) behavior.
	r.UpdateRoutingRow("agent", nil)
	hears, _ = r.Mixer().ParticipantHears("a")
	if hears != nil {
		t.Errorf("after clearing the agent row, agent.Hears should be nil (full mesh); got %v", hears)
	}
}

func TestRoom_RoutingMatrix_RoundTrip(t *testing.T) {
	r := NewRoom("r", "", 16000, newTestLog())
	r.SetRoutingMatrix(map[string][]string{
		"customer":   {"agent"},
		"agent":      {"customer", "supervisor"},
		"supervisor": {"customer", "agent"},
	})

	got := r.RoutingMatrix()
	for role, sources := range got {
		sort.Strings(sources)
		got[role] = sources
	}
	want := map[string][]string{
		"customer":   {"agent"},
		"agent":      {"customer", "supervisor"},
		"supervisor": {"agent", "customer"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RoutingMatrix() = %v, want %v", got, want)
	}
}
