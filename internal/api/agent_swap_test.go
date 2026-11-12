package api

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/agent"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// swapBot stands in for a Pipecat bot. It holds every WebSocket open and
// records, in arrival order, whether the client has closed it.
type swapBot struct {
	url string

	mu     sync.Mutex
	conns  []net.Conn
	closed []bool
}

func newSwapBot(t *testing.T) *swapBot {
	t.Helper()
	b := &swapBot{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		b.mu.Lock()
		i := len(b.conns)
		b.conns = append(b.conns, conn)
		b.closed = append(b.closed, false)
		b.mu.Unlock()
		for {
			if _, _, err := wsutil.ReadClientData(conn); err != nil {
				break
			}
		}
		b.mu.Lock()
		b.closed[i] = true
		b.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	b.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return b
}

func (b *swapBot) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.conns)
}

func (b *swapBot) isClosed(i int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed[i]
}

// hangUp closes connection i from the bot's side.
func (b *swapBot) hangUp(i int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.conns[i].Close()
}

// pausedAudio is leg audio that arrives only once resumed, as on a leg whose
// far end has stopped sending. From then on it yields silence in real time.
type pausedAudio struct {
	resumed chan struct{}
	done    chan struct{}
	readers atomic.Int32
}

func newPausedAudio(t *testing.T) *pausedAudio {
	t.Helper()
	a := &pausedAudio{resumed: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(func() { close(a.done) })
	return a
}

func (a *pausedAudio) resume() { close(a.resumed) }

func (a *pausedAudio) Read(p []byte) (int, error) {
	a.readers.Add(1)
	defer a.readers.Add(-1)
	select {
	case <-a.resumed:
	case <-a.done:
		return 0, io.EOF
	}
	select {
	case <-time.After(20 * time.Millisecond):
	case <-a.done:
		return 0, io.EOF
	}
	clear(p)
	return len(p), nil
}

func swapEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// assertAgentStays fails if the attached agent changes within the time the
// replaced session's clean-up needs to run.
func assertAgentStays(t *testing.T, bot *swapBot, current func() *agentInfo, want *agentInfo) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if current() != want {
			t.Fatal("the replaced agent's exit detached the agent attached after it")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !want.session.Running() {
		t.Fatal("the replaced agent's exit stopped the new agent's session")
	}
	if bot.isClosed(1) {
		t.Fatal("the new agent's connection was closed")
	}
}

func legAgentFor(legID string) *agentInfo {
	legAgents.Lock()
	defer legAgents.Unlock()
	return legAgents.m[legID]
}

func roomAgentFor(roomID string) *agentInfo {
	roomAgents.Lock()
	defer roomAgents.Unlock()
	return roomAgents.m[roomID]
}

// TestLegAgentSwap_OldSessionExitLeavesNewAgent detaches a leg's agent and
// attaches another before the first session has returned. A session returns
// only when its send loop next reads leg audio, so the audio is paused across
// the swap.
func TestLegAgentSwap_OldSessionExitLeavesNewAgent(t *testing.T) {
	const legID = "agent-swap-leg"
	s := newTestServer(t)
	bot := newSwapBot(t)
	audio := newPausedAudio(t)
	s.LegMgr.Add(&apiMockLeg{id: legID, reader: audio, writer: io.Discard, createdAt: time.Now()})
	t.Cleanup(func() { s.cleanupLegAgent(legID, true) })

	var exits atomic.Int32
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type == events.AgentDisconnected && e.Data.GetLegID() == legID {
			exits.Add(1)
		}
	})

	opts := agent.Options{AgentID: bot.url}
	if _, err := s.doStartLegAgent(legID, "pipecat", "", opts); err != nil {
		t.Fatalf("attach first agent: %v", err)
	}
	swapEventually(t, "first agent to read leg audio", func() bool {
		return bot.count() == 1 && audio.readers.Load() == 1
	})
	if _, err := s.doStopAgentLeg(legID); err != nil {
		t.Fatalf("detach first agent: %v", err)
	}
	if _, err := s.doStartLegAgent(legID, "pipecat", "", opts); err != nil {
		t.Fatalf("attach second agent: %v", err)
	}
	swapEventually(t, "second agent to connect", func() bool { return bot.count() == 2 })
	second := legAgentFor(legID)
	if second == nil {
		t.Fatal("second agent is not attached")
	}
	if exits.Load() != 0 {
		t.Fatal("first session returned before the audio resumed; the test no longer forces the order")
	}

	audio.resume()
	swapEventually(t, "first session to exit", func() bool { return exits.Load() >= 1 })
	assertAgentStays(t, bot, func() *agentInfo { return legAgentFor(legID) }, second)

	if _, err := s.doStopAgentLeg(legID); err != nil {
		t.Fatalf("detach second agent: %v", err)
	}
	swapEventually(t, "second agent's connection to close", func() bool { return bot.isClosed(1) })
}

// TestLegAgent_SessionEndDetaches: a session that ends by itself must still
// free the leg for the next agent.
func TestLegAgent_SessionEndDetaches(t *testing.T) {
	const legID = "agent-end-leg"
	s := newTestServer(t)
	bot := newSwapBot(t)
	audio := newPausedAudio(t)
	audio.resume()
	s.LegMgr.Add(&apiMockLeg{id: legID, reader: audio, writer: io.Discard, createdAt: time.Now()})
	t.Cleanup(func() { s.cleanupLegAgent(legID, true) })

	if _, err := s.doStartLegAgent(legID, "pipecat", "", agent.Options{AgentID: bot.url}); err != nil {
		t.Fatalf("attach agent: %v", err)
	}
	swapEventually(t, "agent to connect", func() bool { return bot.count() == 1 })
	bot.hangUp(0)
	swapEventually(t, "ended agent to be detached", func() bool { return legAgentFor(legID) == nil })
}

// TestRoomAgentSwap_OldSessionExitLeavesNewAgent is the room form of the leg
// test. The first session's exit is held in its agent.disconnected event until
// the second agent is attached, which fixes the order.
func TestRoomAgentSwap_OldSessionExitLeavesNewAgent(t *testing.T) {
	const roomID = "agent-swap-room"
	s := newTestServer(t)
	bot := newSwapBot(t)
	if _, err := s.RoomMgr.Create(roomID, "", 0); err != nil {
		t.Fatalf("create room: %v", err)
	}
	t.Cleanup(func() { s.cleanupRoomAgent(roomID) })

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.AgentDisconnected || e.Data.GetRoomID() != roomID {
			return
		}
		once.Do(func() {
			close(entered)
			<-release
		})
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	opts := agent.Options{AgentID: bot.url}
	if _, err := s.doStartRoomAgent(roomID, "pipecat", "", opts); err != nil {
		t.Fatalf("attach first agent: %v", err)
	}
	swapEventually(t, "first agent to connect", func() bool { return bot.count() == 1 })
	if _, err := s.doStopAgentRoom(roomID); err != nil {
		t.Fatalf("detach first agent: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first session did not exit")
	}
	if _, err := s.doStartRoomAgent(roomID, "pipecat", "", opts); err != nil {
		t.Fatalf("attach second agent: %v", err)
	}
	swapEventually(t, "second agent to connect", func() bool { return bot.count() == 2 })
	second := roomAgentFor(roomID)
	if second == nil {
		t.Fatal("second agent is not attached")
	}

	close(release)
	assertAgentStays(t, bot, func() *agentInfo { return roomAgentFor(roomID) }, second)

	if _, err := s.doStopAgentRoom(roomID); err != nil {
		t.Fatalf("detach second agent: %v", err)
	}
	swapEventually(t, "second agent's connection to close", func() bool { return bot.isClosed(1) })
}
