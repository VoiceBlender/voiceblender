package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

func collectEvents(s *Server) *[]events.Event {
	got := &[]events.Event{}
	s.Bus.Subscribe(func(e events.Event) { *got = append(*got, e) })
	return got
}

func TestValidateCustomData_Limit(t *testing.T) {
	s := newTestServer(t)
	s.Config.CustomDataMaxBytes = 16

	if err := s.validateCustomData(events.CustomData(`{"a":1}`)); err != nil {
		t.Fatalf("under limit rejected: %v", err)
	}
	err := s.validateCustomData(events.CustomData(`{"aaaaaaaaaaaaaaaaaaaa":1}`))
	if err == nil {
		t.Fatal("over limit accepted")
	}
	ae, ok := err.(*apiError)
	if !ok || ae.Code != http.StatusBadRequest {
		t.Fatalf("want 400 apiError, got %#v", err)
	}
	if !strings.Contains(ae.Message, "exceeds limit of 16") {
		t.Fatalf("message lacks the limit: %q", ae.Message)
	}

	// 0 disables the cap.
	s.Config.CustomDataMaxBytes = 0
	if err := s.validateCustomData(events.CustomData(strings.Repeat("a", 100_000))); err != nil {
		t.Fatalf("unlimited rejected: %v", err)
	}
}

func TestApplyCustomData_OmitNullReplace(t *testing.T) {
	s := newTestServer(t)

	// Omitted leaves the registry untouched.
	s.applyCustomData("leg-1", nil)
	if got := s.Bus.CustomData.Leg("leg-1"); got != nil {
		t.Fatalf("omitted wrote %s", got)
	}

	s.applyCustomData("leg-1", events.CustomData(`{"a":1}`))
	if got := string(s.Bus.CustomData.Leg("leg-1")); got != `{"a":1}` {
		t.Fatalf("set: got %s", got)
	}

	s.applyCustomData("leg-1", nil)
	if got := string(s.Bus.CustomData.Leg("leg-1")); got != `{"a":1}` {
		t.Fatalf("omitted must not clear: got %s", got)
	}

	s.applyCustomData("leg-1", events.CustomData(`{"a":2}`))
	if got := string(s.Bus.CustomData.Leg("leg-1")); got != `{"a":2}` {
		t.Fatalf("replace: got %s", got)
	}

	s.applyCustomData("leg-1", events.CustomData(`null`))
	if got := s.Bus.CustomData.Leg("leg-1"); got != nil {
		t.Fatalf("null must clear: got %s", got)
	}
}

func TestParseCustomDataQuery(t *testing.T) {
	s := newTestServer(t)
	s.Config.CustomDataMaxBytes = 1024

	cd, err := s.parseCustomDataQuery("")
	if err != nil || cd != nil {
		t.Fatalf("empty: %v %s", err, cd)
	}

	cd, err = s.parseCustomDataQuery(`{"a":1}`)
	if err != nil || string(cd) != `{"a":1}` {
		t.Fatalf("valid: %v %s", err, cd)
	}

	if _, err := s.parseCustomDataQuery(`{not json`); err == nil {
		t.Fatal("invalid JSON accepted")
	}

	s.Config.CustomDataMaxBytes = 4
	if _, err := s.parseCustomDataQuery(`{"a":1}`); err == nil {
		t.Fatal("over limit accepted")
	}
}

func TestCreateLeg_RejectsOversizeCustomData(t *testing.T) {
	s := newTestServer(t)
	s.Config.CustomDataMaxBytes = 32

	body := `{"type":"sip","to":"sip:a@b","custom_data":{"pad":"` + strings.Repeat("x", 100) + `"}}`
	w := doRequest(s, http.MethodPost, "/v1/legs", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "exceeds limit of 32") {
		t.Fatalf("body: %s", w.Body.String())
	}
}

// The size check must run before the leg-type switch, so it applies to every
// leg type rather than only the one path that happens to validate it.
func TestCreateLeg_CustomDataCheckedBeforeTypeDispatch(t *testing.T) {
	s := newTestServer(t)
	s.Config.CustomDataMaxBytes = 8

	for _, typ := range []string{"sip", "websocket", "whatsapp", "livekit_room", "bogus"} {
		body := `{"type":"` + typ + `","custom_data":{"pad":"` + strings.Repeat("x", 50) + `"}}`
		w := doRequest(s, http.MethodPost, "/v1/legs", body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "exceeds limit") {
			t.Fatalf("type=%s: got %d %s", typ, w.Code, w.Body.String())
		}
	}
}

func TestCustomData_AnyJSONValueAccepted(t *testing.T) {
	s := newTestServer(t)
	for _, raw := range []string{`{"a":1}`, `[1,2,3]`, `"hi"`, `42`, `true`} {
		var req CreateLegRequest
		if err := json.Unmarshal([]byte(`{"type":"sip","custom_data":`+raw+`}`), &req); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if string(req.CustomData) != raw {
			t.Fatalf("got %s, want %s", req.CustomData, raw)
		}
		if err := s.validateCustomData(req.CustomData); err != nil {
			t.Fatalf("%s rejected: %v", raw, err)
		}
	}
}

func TestLegView_CarriesCustomData(t *testing.T) {
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", createdAt: time.Now()}
	s.LegMgr.Add(l)
	s.Bus.CustomData.SetLeg("leg-1", events.CustomData(`{"order":"A-1"}`))

	w := doRequest(s, http.MethodGet, "/v1/legs/leg-1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	var view struct {
		CustomData json.RawMessage `json:"custom_data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(view.CustomData) != `{"order":"A-1"}` {
		t.Fatalf("got %s", view.CustomData)
	}

	// A leg with none must omit the key rather than emit null.
	s.LegMgr.Add(&apiMockLeg{id: "leg-2", createdAt: time.Now()})
	w = doRequest(s, http.MethodGet, "/v1/legs/leg-2", "")
	if strings.Contains(w.Body.String(), "custom_data") {
		t.Fatalf("unset leg emitted custom_data: %s", w.Body.String())
	}
}

// The registry entry must outlive the leg.disconnected publish: the CDR event
// is emitted after cleanupLeg has already removed the leg from the manager.
func TestPublishDisconnect_CustomDataSurvivesThenClears(t *testing.T) {
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", createdAt: time.Now()}
	s.LegMgr.Add(l)
	s.Bus.CustomData.SetLeg("leg-1", events.CustomData(`{"order":"A-1"}`))
	got := collectEvents(s)

	s.LegMgr.Remove(l.ID())
	s.publishDisconnect(l, "api_hangup")

	if len(*got) != 1 {
		t.Fatalf("got %d events", len(*got))
	}
	e := (*got)[0]
	if e.Type != events.LegDisconnected {
		t.Fatalf("got %s", e.Type)
	}
	if string(e.CustomData) != `{"order":"A-1"}` {
		t.Fatalf("leg.disconnected lost custom_data: %q", e.CustomData)
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"custom_data":{"order":"A-1"}`) {
		t.Fatalf("wire form: %s", b)
	}
	if leftover := s.Bus.CustomData.Leg("leg-1"); leftover != nil {
		t.Fatalf("registry not cleared: %s", leftover)
	}
}

func TestDoRingLeg_ValidatesAndRejects(t *testing.T) {
	s := newTestServer(t)
	s.Config.CustomDataMaxBytes = 8
	s.LegMgr.Add(&apiMockLeg{id: "leg-1", createdAt: time.Now()})

	err := s.doRingLeg("leg-1", events.CustomData(`{"a":"`+strings.Repeat("x", 50)+`"}`))
	if err == nil {
		t.Fatal("oversize accepted")
	}
	if ae, ok := err.(*apiError); !ok || ae.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %#v", err)
	}
	// Rejected input must not be stored.
	if got := s.Bus.CustomData.Leg("leg-1"); got != nil {
		t.Fatalf("stored despite rejection: %s", got)
	}

	// A non-SIP leg still fails, and still stores nothing.
	if err := s.doRingLeg("leg-1", events.CustomData(`{"a":1}`)); err == nil {
		t.Fatal("non-SIP leg accepted")
	}
	if got := s.Bus.CustomData.Leg("leg-1"); got != nil {
		t.Fatalf("stored on a failed precondition: %s", got)
	}
}

// A request that fails a precondition must not mutate stored data: the caller
// sees a 4xx and reasonably assumes nothing happened.
func TestDoAnswerLeg_RejectedRequestStoresNothing(t *testing.T) {
	s := newTestServer(t)
	// apiMockLeg is neither a SIP nor a WhatsApp leg, so answering it fails.
	s.LegMgr.Add(&apiMockLeg{id: "leg-1", createdAt: time.Now()})

	if err := s.doAnswerLeg("leg-1", nil, "", nil, events.CustomData(`{"order":"A-1"}`)); err == nil {
		t.Fatal("unanswerable leg accepted")
	}
	if got := s.Bus.CustomData.Leg("leg-1"); got != nil {
		t.Fatalf("stored on a failed precondition: %s", got)
	}

	if err := s.doAnswerLeg("missing", nil, "", nil, events.CustomData(`{"a":1}`)); err == nil {
		t.Fatal("unknown leg accepted")
	}
	if got := s.Bus.CustomData.Leg("missing"); got != nil {
		t.Fatalf("stored for an unknown leg: %s", got)
	}

	// Oversize is rejected before anything else runs.
	s.Config.CustomDataMaxBytes = 4
	err := s.doAnswerLeg("leg-1", nil, "", nil, events.CustomData(`{"a":1}`))
	if ae, ok := err.(*apiError); !ok || ae.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %#v", err)
	}
}

func TestRingLeg_HTTPBodyOptional(t *testing.T) {
	s := newTestServer(t)
	s.LegMgr.Add(&apiMockLeg{id: "leg-1", createdAt: time.Now()})

	// No body at all: still reaches the leg-type precondition (400), not a
	// decode error.
	w := doRequest(s, http.MethodPost, "/v1/legs/leg-1/ring", "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "only SIP inbound") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}

	w = doRequest(s, http.MethodPost, "/v1/legs/leg-1/ring", `{not json`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid request body") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}
