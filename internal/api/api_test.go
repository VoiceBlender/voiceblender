package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/metrics"
	"github.com/VoiceBlender/voiceblender/internal/room"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	bus := events.NewBus("test")
	log := slog.Default()
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	t.Cleanup(func() { webhooks.Stop() })
	m := metrics.New(bus)

	cfg := config.Config{
		InstanceID:        "test-instance",
		DefaultSampleRate: 16000,
	}

	s := NewServer(legMgr, roomMgr, nil, bus, webhooks, nil, nil, nil, m, cfg, log)
	return s
}

func doRequest(s *Server, method, path string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	s.Router.ServeHTTP(w, r)
	return w
}

// --- Helper tests ---

func TestWriteJSON(t *testing.T) {
	// Save and restore package-level instanceID.
	old := instanceID
	instanceID = "test-inst"
	defer func() { instanceID = old }()

	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"key": "value"})

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"instance_id":"test-inst"`) {
		t.Errorf("missing instance_id injection: %s", body)
	}
	if !strings.Contains(body, `"key":"value"`) {
		t.Errorf("missing key/value: %s", body)
	}
}

func TestWriteError(t *testing.T) {
	old := instanceID
	instanceID = "test-inst"
	defer func() { instanceID = old }()

	w := httptest.NewRecorder()
	writeError(w, http.StatusBadRequest, "something went wrong")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"error":"something went wrong"`) {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestDecodeJSON(t *testing.T) {
	body := `{"name":"test","value":42}`
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	var result struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}
	if err := decodeJSON(r, &result); err != nil {
		t.Fatalf("decodeJSON: %v", err)
	}
	if result.Name != "test" || result.Value != 42 {
		t.Errorf("got %+v", result)
	}
}

func TestDecodeJSON_Invalid(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not json"))
	var result struct{}
	if err := decodeJSON(r, &result); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// --- Server creation ---

func TestNewServer_HasRouter(t *testing.T) {
	s := newTestServer(t)
	if s.Router == nil {
		t.Fatal("expected non-nil router")
	}
}

// --- Leg endpoints ---

func TestListLegs_Empty(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodGet, "/v1/legs", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "[]") {
		t.Errorf("expected empty array, got: %s", body)
	}
}

func TestGetLeg_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodGet, "/v1/legs/nonexistent", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDeleteLeg_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodDelete, "/v1/legs/nonexistent", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestAnswerLeg_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/legs/nonexistent/answer", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestMuteLeg_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/legs/nonexistent/mute", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// --- Room endpoints ---

func TestListRooms_Empty(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodGet, "/v1/rooms", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "[]") {
		t.Errorf("expected empty array, got: %s", body)
	}
}

func TestCreateRoom(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"test-room"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", w.Code, w.Body.String())
	}

	var resp RoomView
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "test-room" {
		t.Errorf("ID = %q, want test-room", resp.ID)
	}
}

func TestCreateRoom_AutoID(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/rooms", "")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", w.Code, w.Body.String())
	}
}

func TestCreateRoom_SampleRate(t *testing.T) {
	s := newTestServer(t)

	// Explicit 48kHz
	w := doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r-48k","sample_rate":48000}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", w.Code, w.Body.String())
	}
	var resp RoomView
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.SampleRate != 48000 {
		t.Errorf("SampleRate = %d, want 48000", resp.SampleRate)
	}

	// Default (omitted) → 16000
	w2 := doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r-default"}`)
	if w2.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w2.Code)
	}
	var resp2 RoomView
	json.NewDecoder(w2.Body).Decode(&resp2)
	if resp2.SampleRate != 16000 {
		t.Errorf("SampleRate = %d, want 16000", resp2.SampleRate)
	}

	// 8kHz
	w3 := doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r-8k","sample_rate":8000}`)
	if w3.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w3.Code)
	}
	var resp3 RoomView
	json.NewDecoder(w3.Body).Decode(&resp3)
	if resp3.SampleRate != 8000 {
		t.Errorf("SampleRate = %d, want 8000", resp3.SampleRate)
	}
}

func TestCreateRoom_InvalidSampleRate(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r-bad","sample_rate":44100}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
	}
}

func TestCreateRoom_Duplicate(t *testing.T) {
	s := newTestServer(t)
	doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r1"}`)
	w := doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r1"}`)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestGetRoom_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodGet, "/v1/rooms/nonexistent", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestGetRoom_Found(t *testing.T) {
	s := newTestServer(t)
	doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r1"}`)
	w := doRequest(s, http.MethodGet, "/v1/rooms/r1", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestDeleteRoom_NotFound(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodDelete, "/v1/rooms/nonexistent", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDeleteRoom(t *testing.T) {
	s := newTestServer(t)
	doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r1"}`)
	w := doRequest(s, http.MethodDelete, "/v1/rooms/r1", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	w = doRequest(s, http.MethodGet, "/v1/rooms/r1", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("room should be deleted, got status %d", w.Code)
	}
}

func TestCreateLeg_UnsupportedType(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/legs", `{"type":"webrtc"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestCreateLeg_InvalidJSON(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/legs", "not json")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// --- JSON response format ---

func TestJSON_IncludesInstanceID(t *testing.T) {
	s := newTestServer(t)
	// Use a room endpoint that returns an object, not an array.
	doRequest(s, http.MethodPost, "/v1/rooms", `{"id":"r1"}`)
	w := doRequest(s, http.MethodGet, "/v1/rooms/r1", "")

	body := w.Body.String()
	if !strings.Contains(body, `"instance_id":"test-instance"`) {
		t.Errorf("expected instance_id in response: %s", body)
	}
}

// --- Schema enrichments ---

func TestSchemaEnrichments(t *testing.T) {
	enrichments := SchemaEnrichments()
	if len(enrichments) == 0 {
		t.Fatal("expected non-empty enrichments")
	}
	// Spot-check a few entries.
	if _, ok := enrichments["CreateLegRequest.type"]; !ok {
		t.Error("missing CreateLegRequest.type")
	}
	if _, ok := enrichments["LegView.id"]; !ok {
		t.Error("missing LegView.id")
	}
	if _, ok := enrichments["RoomView.id"]; !ok {
		t.Error("missing RoomView.id")
	}
}
