package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws"
)

// Binds the drop branch in vsi() to voiceblender_vsi_events_dropped_total.
// A metrics-package test that calls ObserveVSIDropped directly would still pass
// with the call site deleted; this one drives the real handler.
func TestVSI_BufferFullIncrementsDroppedCounter(t *testing.T) {
	s := newTestServer(t)
	// Defeat the 256-event fallback so a burst overflows immediately.
	s.Config.VSIEventBufferSize = 1

	srv := httptest.NewServer(s.Router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/vsi"
	conn, _, _, err := ws.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial vsi: %v", err)
	}
	defer conn.Close()
	// Deliberately never read: the send loop stalls once the socket buffer
	// fills, so the cap-1 channel backs up and the drop branch runs.

	time.Sleep(100 * time.Millisecond) // let the handler subscribe

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for i := 0; i < 2000; i++ {
			s.Bus.Publish(events.DTMFReceived, &events.DTMFReceivedData{
				LegScope: events.LegScope{LegID: "leg-1"},
				Digit:    "1",
			})
		}
		if !strings.Contains(scrapeMetrics(t, s), "voiceblender_vsi_events_dropped_total 0") {
			return
		}
	}
	t.Fatalf("counter never left zero:\n%s", scrapeMetrics(t, s))
}

func scrapeMetrics(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
