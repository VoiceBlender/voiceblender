package events

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeObserver struct {
	mu       sync.Mutex
	enqueued int
	dropped  int
	outcomes map[string]int
}

func newFakeObserver() *fakeObserver {
	return &fakeObserver{outcomes: make(map[string]int)}
}

func (f *fakeObserver) OnWebhookEnqueued() {
	f.mu.Lock()
	f.enqueued++
	f.mu.Unlock()
}

func (f *fakeObserver) OnWebhookDropped() {
	f.mu.Lock()
	f.dropped++
	f.mu.Unlock()
}

func (f *fakeObserver) OnWebhookDelivered(outcome string) {
	f.mu.Lock()
	f.outcomes[outcome]++
	f.mu.Unlock()
}

func (f *fakeObserver) snapshot() (int, int, map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.outcomes))
	for k, v := range f.outcomes {
		out[k] = v
	}
	return f.enqueued, f.dropped, out
}

func (f *fakeObserver) totalOutcomes() int {
	_, _, out := f.snapshot()
	n := 0
	for _, v := range out {
		n += v
	}
	return n
}

func newTestRegistry(t *testing.T, url string) (*WebhookRegistry, *fakeObserver) {
	t.Helper()
	bus := NewBus("test")
	r := NewWebhookRegistry(bus, slog.New(slog.NewTextHandler(io.Discard, nil)), url, "")
	t.Cleanup(r.Stop)
	obs := newFakeObserver()
	r.SetMetricsObserver(obs)
	return r, obs
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestWebhook_EventIDHeaderAndBody(t *testing.T) {
	type capture struct {
		header string
		body   string
	}
	got := make(chan capture, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- capture{header: r.Header.Get("X-Event-Id"), body: string(b)}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, obs := newTestRegistry(t, srv.URL)
	r.bus.Publish(LegRinging, &LegRingingData{LegScope: LegScope{LegID: "leg-1"}})

	select {
	case c := <-got:
		if c.header == "" {
			t.Fatal("X-Event-Id header not set")
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(c.body), &payload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if payload["event_id"] != c.header {
			t.Errorf("body event_id = %v, X-Event-Id = %q; want equal", payload["event_id"], c.header)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	if !waitFor(t, func() bool { return obs.totalOutcomes() == 1 }) {
		t.Fatalf("want exactly 1 outcome, got %d", obs.totalOutcomes())
	}
	enq, _, outcomes := obs.snapshot()
	if enq != 1 {
		t.Errorf("enqueued = %d, want 1", enq)
	}
	if outcomes["success"] != 1 {
		t.Errorf("outcomes = %v, want success=1", outcomes)
	}
}

// The retry loop rebuilds the request each attempt, so X-Event-Id must come off
// the shared event rather than being regenerated per attempt.
func TestWebhook_EventIDStableAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get("X-Event-Id"))
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r, obs := newTestRegistry(t, srv.URL)
	r.bus.Publish(LegRinging, &LegRingingData{LegScope: LegScope{LegID: "leg-1"}})

	// 3 attempts with 2s + 4s backoff between them.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(ids)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 3 {
		t.Fatalf("attempts = %d, want 3", len(ids))
	}
	for i, id := range ids {
		if id == "" {
			t.Fatalf("attempt %d had empty X-Event-Id", i+1)
		}
		if id != ids[0] {
			t.Errorf("attempt %d X-Event-Id = %q, want %q", i+1, id, ids[0])
		}
	}
	if !waitFor(t, func() bool { return obs.totalOutcomes() == 1 }) {
		t.Fatalf("want exactly 1 outcome, got %d", obs.totalOutcomes())
	}
	_, _, outcomes := obs.snapshot()
	if outcomes["exhausted"] != 1 {
		t.Errorf("outcomes = %v, want exhausted=1", outcomes)
	}
}

func TestWebhook_MalformedURLReportsRequestError(t *testing.T) {
	r, obs := newTestRegistry(t, "http://not a url/\x7f")
	r.bus.Publish(LegRinging, &LegRingingData{LegScope: LegScope{LegID: "leg-1"}})

	if !waitFor(t, func() bool { return obs.totalOutcomes() == 1 }) {
		t.Fatalf("want exactly 1 outcome, got %d", obs.totalOutcomes())
	}
	_, _, outcomes := obs.snapshot()
	if outcomes["request_error"] != 1 {
		t.Errorf("outcomes = %v, want request_error=1", outcomes)
	}
}

type unmarshalableData struct {
	LegScope
	Bad chan int `json:"bad"`
}

func TestWebhook_MarshalFailureReportsMarshalError(t *testing.T) {
	r, obs := newTestRegistry(t, "http://127.0.0.1:1/hook")
	r.deliver(deliveryJob{
		hook:  &Webhook{ID: "global", URL: "http://127.0.0.1:1/hook"},
		event: Event{Type: LegRinging, Data: &unmarshalableData{LegScope: LegScope{LegID: "leg-1"}}},
	})

	if obs.totalOutcomes() != 1 {
		t.Fatalf("want exactly 1 outcome, got %d", obs.totalOutcomes())
	}
	_, _, outcomes := obs.snapshot()
	if outcomes["marshal_error"] != 1 {
		t.Errorf("outcomes = %v, want marshal_error=1", outcomes)
	}
}

func TestWebhook_FullQueueCountsDrops(t *testing.T) {
	r, obs := newTestRegistry(t, "http://127.0.0.1:1/hook")
	// Pre-fill the queue so enqueue takes the default branch. Workers are
	// running, so allow for a few slots being drained concurrently.
	const burst = 1000 + 64
	hook := &Webhook{ID: "global", URL: "http://127.0.0.1:1/hook"}
	for i := 0; i < burst; i++ {
		r.enqueue(hook, Event{Type: LegRinging, EventID: "e"}, obs)
	}

	enq, dropped, _ := obs.snapshot()
	if dropped == 0 {
		t.Fatal("expected at least one drop once the 1000-slot queue filled")
	}
	if enq+dropped != burst {
		t.Errorf("enqueued+dropped = %d, want %d", enq+dropped, burst)
	}
}

func TestWebhook_NilObserverIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bus := NewBus("test")
	r := NewWebhookRegistry(bus, slog.New(slog.NewTextHandler(io.Discard, nil)), srv.URL, "")
	defer r.Stop()

	bus.Publish(LegRinging, &LegRingingData{LegScope: LegScope{LegID: "leg-1"}})
	time.Sleep(200 * time.Millisecond)
}
