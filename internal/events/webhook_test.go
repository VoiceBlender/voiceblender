package events

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeObserver records MetricsObserver calls. The observer is invoked from the
// webhook worker goroutines, so every field is mutex-guarded.
type fakeObserver struct {
	mu        sync.Mutex
	enqueued  int
	dropped   int
	delivered []string // outcome values, in call order

	// deliveredCh signals each OnWebhookDelivered call so tests can
	// synchronize instead of sleeping.
	deliveredCh chan string
}

func newFakeObserver() *fakeObserver {
	return &fakeObserver{deliveredCh: make(chan string, 16)}
}

func (f *fakeObserver) OnWebhookEnqueued(webhookID, eventType string) {
	f.mu.Lock()
	f.enqueued++
	f.mu.Unlock()
}

func (f *fakeObserver) OnWebhookDropped(webhookID, eventType string) {
	f.mu.Lock()
	f.dropped++
	f.mu.Unlock()
}

func (f *fakeObserver) OnWebhookDelivered(webhookID, eventType, outcome string) {
	f.mu.Lock()
	f.delivered = append(f.delivered, outcome)
	f.mu.Unlock()
	select {
	case f.deliveredCh <- outcome:
	default:
	}
}

func (f *fakeObserver) counts() (enqueued, dropped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enqueued, f.dropped
}

// waitDelivered blocks for one OnWebhookDelivered call and returns its outcome.
func (f *fakeObserver) waitDelivered(t *testing.T) string {
	t.Helper()
	select {
	case o := <-f.deliveredCh:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnWebhookDelivered")
		return ""
	}
}

// newTestRegistry builds a registry without starting workers or subscribing to
// a bus, so enqueue/deliver can be driven directly and deterministically.
func newTestRegistry(t *testing.T, queueCap int) *WebhookRegistry {
	t.Helper()
	r := &WebhookRegistry{
		legWebhooks:  make(map[string]*Webhook),
		roomWebhooks: make(map[string]*Webhook),
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		client:       &http.Client{Timeout: 2 * time.Second},
		workCh:       make(chan deliveryJob, queueCap),
		stopCh:       make(chan struct{}),
	}
	t.Cleanup(r.Stop)
	return r
}

func testEvent() Event {
	return Event{Type: LegRinging, Data: &LegRingingData{LegScope: LegScope{LegID: "leg-1"}}}
}

// TestWebhookRegistry_ObserverOnDrop proves the queue-full drop branch reports
// OnWebhookDropped and not OnWebhookEnqueued.
func TestWebhookRegistry_ObserverOnDrop(t *testing.T) {
	r := newTestRegistry(t, 1)
	obs := newFakeObserver()
	r.SetMetricsObserver(obs)

	hook := &Webhook{ID: "leg-1", URL: "http://example.invalid/hook"}

	// First fills the cap-1 queue; the rest must hit the default: drop branch.
	for i := 0; i < 4; i++ {
		r.enqueue(hook, testEvent())
	}

	enqueued, dropped := obs.counts()
	if enqueued != 1 {
		t.Errorf("enqueued = %d, want 1", enqueued)
	}
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3", dropped)
	}
}

// TestWebhookRegistry_NilObserverSafe proves the nil-guards hold: a registry
// with no observer wired must behave exactly as before.
func TestWebhookRegistry_NilObserverSafe(t *testing.T) {
	r := newTestRegistry(t, 1)

	hook := &Webhook{ID: "leg-1", URL: "http://example.invalid/hook"}
	r.enqueue(hook, testEvent()) // accepted
	r.enqueue(hook, testEvent()) // dropped

	// deliver with no observer must not panic either. Use a request-error URL
	// so this returns immediately with no retry sleep.
	r.deliver(deliveryJob{hook: &Webhook{ID: "leg-1", URL: "http://exa mple.com/hook"}, event: testEvent()})
}

// TestWebhookRegistry_DeliverSuccess proves the success terminal exit is
// counted. The test server answers 2xx on the first attempt, so no backoff
// sleep is incurred.
func TestWebhookRegistry_DeliverSuccess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestRegistry(t, 1)
	obs := newFakeObserver()
	r.SetMetricsObserver(obs)

	r.deliver(deliveryJob{hook: &Webhook{ID: "leg-1", URL: srv.URL}, event: testEvent()})

	if got := obs.waitDelivered(t); got != "success" {
		t.Errorf("outcome = %q, want success", got)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server hits = %d, want 1", n)
	}
}

// TestWebhookRegistry_DeliverRequestError proves the fourth terminal exit:
// http.NewRequest failing on a malformed URL. Without it,
// webhook_deliveries_total silently under-counts on exactly the path a
// user-supplied webhook URL can reach. The URL below fails inside
// http.NewRequest ("invalid character \" \" in host name"), so no HTTP request
// is attempted and no retry sleep occurs.
func TestWebhookRegistry_DeliverRequestError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	r := newTestRegistry(t, 1)
	obs := newFakeObserver()
	r.SetMetricsObserver(obs)

	start := time.Now()
	r.deliver(deliveryJob{hook: &Webhook{ID: "leg-1", URL: "http://exa mple.com/hook"}, event: testEvent()})

	if got := obs.waitDelivered(t); got != "request_error" {
		t.Errorf("outcome = %q, want request_error", got)
	}

	obs.mu.Lock()
	n := len(obs.delivered)
	obs.mu.Unlock()
	if n != 1 {
		t.Errorf("delivered calls = %d, want exactly 1", n)
	}
	if hits.Load() != 0 {
		t.Error("expected no HTTP request to be attempted")
	}
	// The request-error exit is terminal on attempt 0, so it must not sleep
	// through the retry backoff.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("deliver took %v; request_error should return without retrying", elapsed)
	}
}

// TestWebhookRegistry_DeliverExhausted proves the exhausted terminal exit.
// It costs the real 2s+4s backoff (webhook.go), so it is skipped in -short.
func TestWebhookRegistry_DeliverExhausted(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises the real retry backoff (~6s)")
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := newTestRegistry(t, 1)
	obs := newFakeObserver()
	r.SetMetricsObserver(obs)

	r.deliver(deliveryJob{hook: &Webhook{ID: "leg-1", URL: srv.URL}, event: testEvent()})

	if got := obs.waitDelivered(t); got != "exhausted" {
		t.Errorf("outcome = %q, want exhausted", got)
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("server hits = %d, want 3 attempts", n)
	}
}

// TestWebhookRegistry_DeliverMarshalError proves the marshal_error terminal
// exit via event data whose MarshalJSON always fails.
func TestWebhookRegistry_DeliverMarshalError(t *testing.T) {
	r := newTestRegistry(t, 1)
	obs := newFakeObserver()
	r.SetMetricsObserver(obs)

	r.deliver(deliveryJob{
		hook:  &Webhook{ID: "leg-1", URL: "http://example.invalid/hook"},
		event: Event{Type: LegRinging, Data: unmarshalableData{}},
	})

	if got := obs.waitDelivered(t); got != "marshal_error" {
		t.Errorf("outcome = %q, want marshal_error", got)
	}

	// Pin deliver's documented exactly-one-outcome invariant, not just the
	// first outcome: without a return after outcome("marshal_error"), execution
	// falls into the retry loop and also reports "exhausted", which
	// waitDelivered alone cannot see. deliver runs synchronously here
	// (newTestRegistry starts no workers), so every increment has landed.
	obs.mu.Lock()
	n := len(obs.delivered)
	obs.mu.Unlock()
	if n != 1 {
		t.Errorf("delivered calls = %d, want exactly 1", n)
	}
}

// unmarshalableData is EventData whose JSON encoding always fails, driving
// deliver's marshal_error exit.
type unmarshalableData struct{}

func (unmarshalableData) GetLegID() string  { return "leg-1" }
func (unmarshalableData) GetRoomID() string { return "" }
func (unmarshalableData) GetAppID() string  { return "" }
func (unmarshalableData) MarshalJSON() ([]byte, error) {
	return nil, io.ErrUnexpectedEOF
}

// TestWebhookRegistry_ObserverRace exercises the observer from the real worker
// goroutines concurrently with SetMetricsObserver, which is how startup wires
// it (the registry subscribes and starts workers before the collector exists).
func TestWebhookRegistry_ObserverRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bus := NewBus("test")
	r := NewWebhookRegistry(bus, slog.New(slog.NewTextHandler(io.Discard, nil)), srv.URL, "")
	defer r.Stop()

	obs := newFakeObserver()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			bus.Publish(LegRinging, &LegRingingData{LegScope: LegScope{LegID: "leg-1"}})
		}
	}()
	r.SetMetricsObserver(obs)
	wg.Wait()

	// Only assert no race/panic: how many events land before the observer is
	// attached is inherently timing-dependent.
	obs.waitDelivered(t)
}
