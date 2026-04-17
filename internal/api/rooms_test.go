package api

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// apiMockLeg implements leg.Leg for addLegToRoom tests.
type apiMockLeg struct {
	id         string
	muted      bool
	deaf       bool
	acceptDTMF bool
	roomID     string
	createdAt  time.Time
}

func (m *apiMockLeg) ID() string                             { return m.id }
func (m *apiMockLeg) Type() leg.LegType                      { return leg.TypeSIPInbound }
func (m *apiMockLeg) State() leg.LegState                    { return leg.StateConnected }
func (m *apiMockLeg) SampleRate() int                        { return 8000 }
func (m *apiMockLeg) AudioReader() io.Reader                 { return nil }
func (m *apiMockLeg) AudioWriter() io.Writer                 { return nil }
func (m *apiMockLeg) OnDTMF(func(rune))                      {}
func (m *apiMockLeg) SendDTMF(context.Context, string) error { return nil }
func (m *apiMockLeg) Hangup(context.Context) error           { return nil }
func (m *apiMockLeg) Answer(context.Context) error           { return nil }
func (m *apiMockLeg) Context() context.Context               { return context.Background() }
func (m *apiMockLeg) RoomID() string                         { return m.roomID }
func (m *apiMockLeg) SetRoomID(id string)                    { m.roomID = id }
func (m *apiMockLeg) AppID() string                          { return "" }
func (m *apiMockLeg) SetAppID(string)                        {}
func (m *apiMockLeg) IsMuted() bool                          { return m.muted }
func (m *apiMockLeg) SetMuted(v bool)                        { m.muted = v }
func (m *apiMockLeg) IsDeaf() bool                           { return m.deaf }
func (m *apiMockLeg) SetDeaf(v bool)                         { m.deaf = v }
func (m *apiMockLeg) AcceptDTMF() bool                       { return m.acceptDTMF }
func (m *apiMockLeg) SetAcceptDTMF(v bool)                   { m.acceptDTMF = v }
func (m *apiMockLeg) SetSpeakingTap(io.Writer)               {}
func (m *apiMockLeg) ClearSpeakingTap()                      {}
func (m *apiMockLeg) IsHeld() bool                           { return false }
func (m *apiMockLeg) CreatedAt() time.Time                   { return m.createdAt }
func (m *apiMockLeg) AnsweredAt() time.Time                  { return time.Time{} }
func (m *apiMockLeg) SIPHeaders() map[string]string          { return nil }
func (m *apiMockLeg) RTPStats() leg.RTPStats                 { return leg.RTPStats{} }

func TestAddLegToRoom_InitialMuteDeaf(t *testing.T) {
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", createdAt: time.Now()}
	s.LegMgr.Add(l)

	w := doRequest(s, http.MethodPost, "/v1/rooms/r1/legs",
		`{"leg_id":"leg-1","mute":true,"deaf":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if !l.IsMuted() {
		t.Error("leg should be muted after join")
	}
	if !l.IsDeaf() {
		t.Error("leg should be deaf after join")
	}
}

func TestAddLegToRoom_DefaultsLeaveStateUntouched(t *testing.T) {
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", muted: true, deaf: false, createdAt: time.Now()}
	s.LegMgr.Add(l)

	w := doRequest(s, http.MethodPost, "/v1/rooms/r1/legs", `{"leg_id":"leg-1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if !l.IsMuted() {
		t.Error("pre-existing mute state should be preserved when mute field is omitted")
	}
	if l.IsDeaf() {
		t.Error("pre-existing deaf=false should be preserved when deaf field is omitted")
	}
}

func TestAddLegToRoom_AcceptDTMFFlag(t *testing.T) {
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", acceptDTMF: true, createdAt: time.Now()}
	s.LegMgr.Add(l)

	w := doRequest(s, http.MethodPost, "/v1/rooms/r1/legs",
		`{"leg_id":"leg-1","accept_dtmf":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	if l.AcceptDTMF() {
		t.Error("leg should have accept_dtmf=false after join")
	}
}

func TestAcceptRejectDTMFEndpoints(t *testing.T) {
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", acceptDTMF: true, createdAt: time.Now()}
	s.LegMgr.Add(l)

	if w := doRequest(s, http.MethodPost, "/v1/legs/leg-1/dtmf/reject", ""); w.Code != http.StatusOK {
		t.Fatalf("reject status = %d, body: %s", w.Code, w.Body.String())
	}
	if l.AcceptDTMF() {
		t.Error("AcceptDTMF should be false after reject")
	}

	if w := doRequest(s, http.MethodPost, "/v1/legs/leg-1/dtmf/accept", ""); w.Code != http.StatusOK {
		t.Fatalf("accept status = %d, body: %s", w.Code, w.Body.String())
	}
	if !l.AcceptDTMF() {
		t.Error("AcceptDTMF should be true after accept")
	}

	if w := doRequest(s, http.MethodPost, "/v1/legs/missing/dtmf/reject", ""); w.Code != http.StatusNotFound {
		t.Errorf("reject on missing leg = %d, want 404", w.Code)
	}
}

func TestAddLegToRoom_MoveAppliesOnlyProvidedFlag(t *testing.T) {
	s := newTestServer(t)
	l := &apiMockLeg{id: "leg-1", muted: false, deaf: true, createdAt: time.Now()}
	s.LegMgr.Add(l)

	if w := doRequest(s, http.MethodPost, "/v1/rooms/r1/legs", `{"leg_id":"leg-1"}`); w.Code != http.StatusOK {
		t.Fatalf("initial add status = %d, body: %s", w.Code, w.Body.String())
	}

	w := doRequest(s, http.MethodPost, "/v1/rooms/r2/legs", `{"leg_id":"leg-1","mute":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("move status = %d, body: %s", w.Code, w.Body.String())
	}
	if !l.IsMuted() {
		t.Error("mute=true on move should apply")
	}
	if !l.IsDeaf() {
		t.Error("omitting deaf on move should preserve existing deaf=true state")
	}
}
