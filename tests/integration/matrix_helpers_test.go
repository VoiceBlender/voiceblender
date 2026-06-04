//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mautrix "maunium.net/go/mautrix"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// mockWriteJSON writes a JSON response. Local to this file to avoid clashing
// with package-private helpers in other integration test files.
func mockWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// mockHomeserver is an httptest.Server implementing the bare minimum
// Client-Server API surface needed to drive a mautrix.Client through one full
// m.call.* call lifecycle. It is not a faithful homeserver — only the
// endpoints the matrix Client/Listener actually call are wired.
type mockHomeserver struct {
	srv *httptest.Server

	mu        sync.Mutex
	sent      []sentEvent
	pendingEv []*mevent.Event // events injected by tests to deliver on the next /sync
	nextBatch int
	closed    bool
	pokeCh    chan struct{}
}

type sentEvent struct {
	RoomID    string
	EventType string
	TxnID     string
	Body      json.RawMessage
}

func newMockHomeserver(t *testing.T) *mockHomeserver {
	t.Helper()
	m := &mockHomeserver{pokeCh: make(chan struct{}, 16)}
	mux := http.NewServeMux()

	// Capabilities — mautrix probes this. Return empty.
	mux.HandleFunc("/_matrix/client/versions", func(w http.ResponseWriter, r *http.Request) {
		mockWriteJSON(w, http.StatusOK, map[string]any{"versions": []string{"v1.6"}})
	})
	mux.HandleFunc("/_matrix/client/v3/capabilities", func(w http.ResponseWriter, r *http.Request) {
		mockWriteJSON(w, http.StatusOK, map[string]any{"capabilities": map[string]any{}})
	})

	// CreateFilter — return any filter ID.
	mux.HandleFunc("/_matrix/client/v3/user/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/filter") {
			mockWriteJSON(w, http.StatusOK, map[string]any{"filter_id": "f1"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	// Send room event: PUT /_matrix/client/v3/rooms/{roomID}/send/{eventType}/{txnID}
	mux.HandleFunc("/_matrix/client/v3/rooms/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"), "/")
		// parts: [{roomID}, "send", {eventType}, {txnID}]
		if len(parts) < 4 || parts[1] != "send" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		m.mu.Lock()
		m.sent = append(m.sent, sentEvent{
			RoomID:    parts[0],
			EventType: parts[2],
			TxnID:     parts[3],
			Body:      json.RawMessage(body),
		})
		m.mu.Unlock()
		mockWriteJSON(w, http.StatusOK, map[string]any{"event_id": fmt.Sprintf("$ev%d", time.Now().UnixNano())})
	})

	// Sync long-poll — drain pendingEv, return as timeline events for the room.
	mux.HandleFunc("/_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		timeoutMs := 30000
		if q := r.URL.Query().Get("timeout"); q != "" {
			if v, err := time.ParseDuration(q + "ms"); err == nil {
				timeoutMs = int(v / time.Millisecond)
			}
		}
		deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

		for time.Now().Before(deadline) {
			m.mu.Lock()
			if m.closed {
				m.mu.Unlock()
				mockWriteJSON(w, http.StatusOK, m.emptySync())
				return
			}
			pending := m.pendingEv
			m.pendingEv = nil
			batch := m.nextBatch
			m.nextBatch++
			m.mu.Unlock()
			if len(pending) > 0 {
				mockWriteJSON(w, http.StatusOK, m.syncResponseWithEvents(batch, pending))
				return
			}
			// Short-poll wait for poke or timeout slice.
			select {
			case <-m.pokeCh:
				continue
			case <-time.After(100 * time.Millisecond):
				continue
			case <-r.Context().Done():
				return
			}
		}
		mockWriteJSON(w, http.StatusOK, m.emptySync())
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.pokeCh)
		m.srv.Close()
	})
	return m
}

func (m *mockHomeserver) URL() string { return m.srv.URL }

func (m *mockHomeserver) emptySync() *mautrix.RespSync {
	return &mautrix.RespSync{NextBatch: fmt.Sprintf("s%d", m.nextBatch)}
}

func (m *mockHomeserver) syncResponseWithEvents(batch int, events []*mevent.Event) *mautrix.RespSync {
	resp := &mautrix.RespSync{NextBatch: fmt.Sprintf("s%d", batch)}
	resp.Rooms.Join = make(map[id.RoomID]*mautrix.SyncJoinedRoom)
	for _, ev := range events {
		jr, ok := resp.Rooms.Join[ev.RoomID]
		if !ok {
			jr = &mautrix.SyncJoinedRoom{}
			resp.Rooms.Join[ev.RoomID] = jr
		}
		jr.Timeline.Events = append(jr.Timeline.Events, ev)
	}
	return resp
}

// InjectEvent queues an event to be delivered on the next /sync response.
// The event should already have RoomID, Sender, Type, Content set.
func (m *mockHomeserver) InjectEvent(ev *mevent.Event) {
	m.mu.Lock()
	m.pendingEv = append(m.pendingEv, ev)
	m.mu.Unlock()
	select {
	case m.pokeCh <- struct{}{}:
	default:
	}
}

// Sent returns a snapshot of all events sent so far.
func (m *mockHomeserver) Sent() []sentEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sentEvent, len(m.sent))
	copy(out, m.sent)
	return out
}

// WaitForSent blocks until at least one event of the given type has been sent,
// or the deadline expires. Returns the first matching event.
func (m *mockHomeserver) WaitForSent(t *testing.T, eventType string, timeout time.Duration) sentEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		for _, e := range m.sent {
			if e.EventType == eventType {
				m.mu.Unlock()
				return e
			}
		}
		m.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for sent event %s", eventType)
	return sentEvent{}
}

// withCtxTimeout wraps a context with a deadline derived from t.
func withCtxTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), d)
}
