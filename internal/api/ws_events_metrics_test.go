package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws"
)

// TestVSI_BufferFullIncrementsDroppedCounter drives the real s.vsi handler and
// proves that its buffer-full drop branch increments
// voiceblender_vsi_events_dropped_total. Nothing else in the suite binds that
// call site to the series: ObserveVSIDropped is not part of
// events.MetricsObserver, so the compile-time assert gives this path no
// protection, and a metrics-package test that calls ObserveVSIDropped directly
// would still pass with the call site deleted. This test must go red if the
// ObserveVSIDropped call is removed from the drop branch in ws_events.go.
func TestVSI_BufferFullIncrementsDroppedCounter(t *testing.T) {
	s := newTestServer(t)
	// Defeat the 256-event fallback so a small burst overflows the buffer.
	s.Config.VSIEventBufferSize = 1

	srv := httptest.NewServer(s.Router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/vsi"
	conn, _, _, err := ws.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial vsi: %v", err)
	}
	defer conn.Close()
	// Deliberately never read from conn: the send loop stalls once the socket
	// buffer fills, so the cap-1 channel backs up and the drop branch runs.

	// Bus.Publish is synchronous, so the subscriber's select runs on this
	// goroutine. Publish in bounded batches until the counter moves or the
	// deadline elapses. The drop count is nondeterministic (the send loop
	// drains concurrently), hence "> 0" rather than an exact value.
	deadline := time.Now().Add(2 * time.Second)
	for {
		for i := 0; i < 200; i++ {
			s.Bus.Publish(events.LegRinging, &events.LegRingingData{
				LegScope: events.LegScope{LegID: "leg-1"},
				From:     "alice",
				To:       "bob",
			})
		}

		got := parseCounter(t, metricsBody(t, s), "voiceblender_vsi_events_dropped_total")
		if got > 0 {
			return // Counter moved on the real drop path.
		}
		if time.Now().After(deadline) {
			t.Fatalf("voiceblender_vsi_events_dropped_total never exceeded 0; "+
				"the drop branch in ws_events.go did not increment the counter\n%s",
				metricsBody(t, s))
		}
	}
}

// metricsBody scrapes the live /metrics endpoint through the router. The
// Collector's counters and registry are unexported, so testutil.ToFloat64 is
// unusable from package api; the handler is the only reachable accessor.
func metricsBody(t *testing.T, s *Server) string {
	t.Helper()
	w := doRequest(s, http.MethodGet, "/metrics", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", w.Code)
	}
	return w.Body.String()
}

// parseCounter returns the value of an unlabelled counter sample, or 0 if the
// series has not been emitted yet.
func parseCounter(t *testing.T, body, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok || key != name {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			t.Fatalf("parse %s value %q: %v", name, val, err)
		}
		return f
	}
	return 0
}
