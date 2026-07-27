//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// panicWriter panics on the first Write, driving the mixer's write loop into
// recoverParticipant. Later writes block so a retry cannot spin.
type panicWriter struct {
	once  sync.Once
	fired chan struct{}
}

func (w *panicWriter) Write([]byte) (int, error) {
	first := false
	w.once.Do(func() { first = true; close(w.fired) })
	if first {
		panic("simulated audio-path panic")
	}
	select {}
}

// blockedReader parks the read loop so only the write loop panics.
type blockedReader struct{ release chan struct{} }

func (r *blockedReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// panicLeg is a leg whose audio path panics. Everything else is the minimum
// leg.Leg surface the room and API layers touch during teardown.
type panicLeg struct {
	id      string
	ctx     context.Context
	cancel  context.CancelFunc
	writer  *panicWriter
	reader  *blockedReader
	mu      sync.Mutex
	roomID  string
	appID   string
	claimed bool
	hungUp  chan struct{}
	once    sync.Once
}

func newPanicLeg(id string) *panicLeg {
	ctx, cancel := context.WithCancel(context.Background())
	return &panicLeg{
		id:     id,
		ctx:    ctx,
		cancel: cancel,
		writer: &panicWriter{fired: make(chan struct{})},
		reader: &blockedReader{release: make(chan struct{})},
		hungUp: make(chan struct{}),
	}
}

func (l *panicLeg) ID() string             { return l.id }
func (l *panicLeg) Type() leg.LegType      { return leg.TypeWebSocketInbound }
func (l *panicLeg) State() leg.LegState    { return leg.StateConnected }
func (l *panicLeg) SampleRate() int        { return 16000 }
func (l *panicLeg) AudioReader() io.Reader { return l.reader }
func (l *panicLeg) AudioWriter() io.Writer { return l.writer }

func (l *panicLeg) OnDTMF(func(rune))                      {}
func (l *panicLeg) SendDTMF(context.Context, string) error { return nil }
func (l *panicLeg) AcceptDTMF() bool                       { return false }
func (l *panicLeg) SetAcceptDTMF(bool)                     {}
func (l *panicLeg) OnTextReceived(func(string, bool))      {}
func (l *panicLeg) SendText(context.Context, string) error { return nil }
func (l *panicLeg) AcceptText() bool                       { return false }
func (l *panicLeg) SetAcceptText(bool)                     {}
func (l *panicLeg) RTTNegotiated() bool                    { return false }
func (l *panicLeg) Answer(context.Context) error           { return nil }
func (l *panicLeg) Context() context.Context               { return l.ctx }
func (l *panicLeg) AppID() string                          { return l.appID }
func (l *panicLeg) SetAppID(id string)                     { l.appID = id }
func (l *panicLeg) IsMuted() bool                          { return false }
func (l *panicLeg) SetMuted(bool)                          {}
func (l *panicLeg) IsDeaf() bool                           { return false }
func (l *panicLeg) SetDeaf(bool)                           {}
func (l *panicLeg) Role() string                           { return "" }
func (l *panicLeg) SetRole(string)                         {}
func (l *panicLeg) SetSpeakingTap(io.Writer)               {}
func (l *panicLeg) ClearSpeakingTap()                      {}
func (l *panicLeg) IsHeld() bool                           { return false }
func (l *panicLeg) CreatedAt() time.Time                   { return time.Now() }
func (l *panicLeg) AnsweredAt() time.Time                  { return time.Now() }
func (l *panicLeg) SIPHeaders() map[string]string          { return nil }
func (l *panicLeg) Headers() map[string]string             { return nil }
func (l *panicLeg) RTPStats() leg.RTPStats                 { return leg.RTPStats{} }

func (l *panicLeg) RoomID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.roomID
}

func (l *panicLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *panicLeg) Hangup(context.Context) error {
	l.once.Do(func() { close(l.hungUp); close(l.reader.release); l.cancel() })
	return nil
}

func (l *panicLeg) ClaimDisconnect() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claimed {
		return false
	}
	l.claimed = true
	return true
}

// TestMixerPanic_TearsDownOnlyPanickedLeg drives a real audio-path panic in a
// room that also holds a live SIP call, and checks the blast radius: the
// panicked leg is hung up and reported as mixer_panic, the call is untouched,
// and the process is still serving.
func TestMixerPanic_TearsDownOnlyPanickedLeg(t *testing.T) {
	instA := newTestInstanceWithMetrics(t, "panic-a")
	instB := newTestInstanceWithMetrics(t, "panic-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"sample_rate": 16000})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
		map[string]interface{}{"leg_id": outboundID})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	bad := newPanicLeg("panic-leg-1")
	t.Cleanup(func() { bad.Hangup(context.Background()) })
	instA.legMgr.Add(bad)
	if err := instA.roomMgr.AddLeg(rm.ID, bad.ID()); err != nil {
		t.Fatalf("add panic leg to room: %v", err)
	}

	// The mix loop is running for the SIP leg, so the panic leg's write loop
	// gets a frame without any nudging from the test.
	select {
	case <-bad.writer.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("panic leg was never written to; the mixer is not running")
	}

	disc := instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == bad.ID()
	}, 5*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "mixer_panic" {
		t.Errorf("cdr.reason = %q, want mixer_panic", got)
	}

	select {
	case <-bad.hungUp:
	case <-time.After(2 * time.Second):
		t.Error("panicked leg was never hung up")
	}

	// Gone from the leg manager, so the REST surface stops serving it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), bad.ID()))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET panicked leg: status %d, want 404", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The blast radius stops there: the SIP call is still up and still in the
	// room, and the room itself still exists.
	legResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	legResp.Body.Close()
	if legResp.StatusCode != http.StatusOK {
		t.Errorf("surviving leg: status %d, want 200", legResp.StatusCode)
	}
	roomGet := httpGet(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), rm.ID))
	var after roomView
	decodeJSON(t, roomGet, &after)
	if len(after.Participants) != 1 || after.Participants[0].ID != outboundID {
		t.Errorf("room participants = %v, want [%s]", after.Participants, outboundID)
	}

	body := metricsBody(t, instA.baseURL())
	if !strings.Contains(body, `voiceblender_recovered_panics_total{component="mixer",site="writeLoop"}`) {
		t.Error("write-loop panic was not counted in /metrics")
	}
}

// TestMixerPanic_BadTapDoesNotStopTheRoom installs a tap that panics on every
// frame — the shape a broken recording or STT consumer takes — and checks that
// the mix loop keeps ticking underneath it.
func TestMixerPanic_BadTapDoesNotStopTheRoom(t *testing.T) {
	instA := newTestInstanceWithMetrics(t, "tickpanic-a")
	instB := newTestInstanceWithMetrics(t, "tickpanic-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"sample_rate": 16000})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
		map[string]interface{}{"leg_id": outboundID})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	r, ok := instA.roomMgr.Get(rm.ID)
	if !ok {
		t.Fatal("room disappeared")
	}

	ticks := make(chan struct{}, 64)
	r.Mixer().SetParticipantTap(outboundID, &tickPanicTap{ticks: ticks})
	t.Cleanup(func() { r.Mixer().ClearParticipantTap(outboundID) })

	// Every tick panics, so counting ticks proves the loop survived repeatedly
	// rather than limping through one recovery.
	for i := range 3 {
		select {
		case <-ticks:
		case <-time.After(3 * time.Second):
			t.Fatalf("mix loop stopped after %d panicking ticks", i)
		}
	}

	// The call is unaffected and the room is still serving.
	legResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	legResp.Body.Close()
	if legResp.StatusCode != http.StatusOK {
		t.Errorf("leg during tick panics: status %d, want 200", legResp.StatusCode)
	}

	if body := metricsBody(t, instA.baseURL()); !strings.Contains(body,
		`voiceblender_recovered_panics_total{component="mixer",site="mixTick"}`) {
		t.Error("tick panic was not counted in /metrics")
	}
}

type tickPanicTap struct{ ticks chan struct{} }

func (w *tickPanicTap) Write([]byte) (int, error) {
	select {
	case w.ticks <- struct{}{}:
	default:
	}
	panic("simulated tap panic")
}
