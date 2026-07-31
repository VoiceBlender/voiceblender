package events

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// eventLogCanary appears nowhere else in the tree.
const eventLogCanary = "zz-pii-canary-6f2a-123-45-6789"

// renderedRecord is one slog record rendered through a JSON handler, matching
// what production writes: cmd/voiceblender/main.go builds a
// slog.NewJSONHandler on os.Stdout, and it is the JSON marshalling of the
// KindAny data attr that materialises the transcript.
type renderedRecord struct {
	Level   slog.Level
	Message string
	JSON    string
}

type renderingHandler struct {
	mu   *sync.Mutex
	recs *[]renderedRecord
}

func newRenderingLogger() (*slog.Logger, func() []renderedRecord) {
	var mu sync.Mutex
	recs := &[]renderedRecord{}
	h := &renderingHandler{mu: &mu, recs: recs}
	return slog.New(h), func() []renderedRecord {
		mu.Lock()
		defer mu.Unlock()
		out := make([]renderedRecord, len(*recs))
		copy(out, *recs)
		return out
	}
}

func (h *renderingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *renderingHandler) Handle(ctx context.Context, r slog.Record) error {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	if err := inner.Handle(ctx, r); err != nil {
		return err
	}
	h.mu.Lock()
	*h.recs = append(*h.recs, renderedRecord{Level: r.Level, Message: r.Message, JSON: buf.String()})
	h.mu.Unlock()
	return nil
}

func (h *renderingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *renderingHandler) WithGroup(string) slog.Handler      { return h }

func assertPayloadOnlyAtDebug(t *testing.T, records []renderedRecord) {
	t.Helper()

	for _, r := range records {
		if r.Level >= slog.LevelInfo && strings.Contains(r.JSON, eventLogCanary) {
			t.Errorf("event payload leaked at %s: %s", r.Level, r.JSON)
		}
	}
	for _, r := range records {
		if r.Level == slog.LevelDebug && r.Message == "event payload" && strings.Contains(r.JSON, eventLogCanary) {
			return
		}
	}
	t.Errorf("no debug %q record carrying the payload; it was dropped rather than demoted. Captured: %v", "event payload", records)
}

// TestLogEvent_PayloadOnlyAtDebug covers the subscriber that dumped leg STT,
// room STT, every agent provider and DTMF simultaneously.
func TestLogEvent_PayloadOnlyAtDebug(t *testing.T) {
	cases := []struct {
		name string
		e    Event
	}{
		{
			name: "stt text",
			e: Event{
				Type:    STTText,
				EventID: "evt-1",
				Data:    &STTTextData{LegRoomScope: LegRoomScope{LegID: "leg-1"}, Text: eventLogCanary, IsFinal: true},
			},
		},
		{
			name: "agent transcript",
			e: Event{
				Type:    AgentUserTranscript,
				EventID: "evt-2",
				Data:    &AgentTranscriptData{LegRoomScope: LegRoomScope{LegID: "leg-1"}, Text: eventLogCanary},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, captured := newRenderingLogger()
			LogEvent(log, tc.e)
			assertPayloadOnlyAtDebug(t, captured())
		})
	}
}

// TestLogEvent_InfoKeepsEnvelope guards against the over-correction of moving
// the whole subscriber to debug. The info envelope is what preserves STT
// liveness on the room path, whose callback logs nothing of its own.
func TestLogEvent_InfoKeepsEnvelope(t *testing.T) {
	log, captured := newRenderingLogger()
	LogEvent(log, Event{
		Type:    STTText,
		EventID: "evt-1",
		Data:    &STTTextData{LegRoomScope: LegRoomScope{LegID: "leg-1"}, Text: eventLogCanary, IsFinal: true},
	})

	for _, r := range captured() {
		if r.Level != slog.LevelInfo || r.Message != "event" {
			continue
		}
		if !strings.Contains(r.JSON, `"type":"stt.text"`) {
			t.Errorf("info envelope missing the event type: %s", r.JSON)
		}
		if !strings.Contains(r.JSON, `"event_id":"evt-1"`) {
			t.Errorf("info envelope missing the event id: %s", r.JSON)
		}
		return
	}
	t.Errorf("no info record %q; operators lost the per-event liveness line. Captured: %v", "event", captured())
}
