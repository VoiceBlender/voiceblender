package api

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/agent"
)

type stubProvider struct{}

func (stubProvider) Start(context.Context, io.Reader, io.Writer, string, agent.Options, agent.Callbacks) error {
	return nil
}
func (stubProvider) Stop()                                       {}
func (stubProvider) Running() bool                               { return true }
func (stubProvider) ConversationID() string                      { return "" }
func (stubProvider) InjectMessage(context.Context, string) error { return nil }

// realtimeWriter stands in for a leg writer that accepts one frame per ptime.
type realtimeWriter struct{ frames atomic.Int64 }

func (w *realtimeWriter) Write(p []byte) (int, error) {
	time.Sleep(20 * time.Millisecond)
	w.frames.Add(1)
	return len(p), nil
}

func attachTestLegAgent(t *testing.T, legID string, frames int) (*realtimeWriter, chan struct{}) {
	t.Helper()
	sb := newStreamBuffer()
	writeFrames(t, sb, frames, 0x11)
	w := &realtimeWriter{}
	done := make(chan struct{})
	go forwardSpeech(sb, w, sbTestFrame, done)

	legAgents.Lock()
	legAgents.m[legID] = &agentInfo{session: stubProvider{}, speakBuf: sb, fwdDone: done}
	legAgents.Unlock()
	t.Cleanup(func() {
		legAgents.Lock()
		delete(legAgents.m, legID)
		legAgents.Unlock()
	})
	return w, done
}

func testServer() *Server {
	return &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestCleanupLegAgentDiscardStopsForwarder(t *testing.T) {
	w, done := attachTestLegAgent(t, "leg-discard", 250) // 5s of buffered speech
	time.Sleep(60 * time.Millisecond)

	start := time.Now()
	testServer().cleanupLegAgent("leg-discard", true)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("detach took %v", elapsed)
	}
	select {
	case <-done:
	default:
		t.Fatal("forwarder still running after detach returned")
	}

	after := w.frames.Load()
	time.Sleep(100 * time.Millisecond)
	if got := w.frames.Load(); got != after {
		t.Fatalf("forwarder wrote %d frames after detach returned", got-after)
	}
	if after > 20 {
		t.Fatalf("forwarded %d of 250 frames; buffered speech was not discarded", after)
	}
}

func TestCleanupLegAgentNaturalEndDrains(t *testing.T) {
	w, done := attachTestLegAgent(t, "leg-drain", 5)

	testServer().cleanupLegAgent("leg-drain", false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not exit after draining")
	}
	if got := w.frames.Load(); got != 5 {
		t.Fatalf("forwarded %d frames, want all 5", got)
	}
}
