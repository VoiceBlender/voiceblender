//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/agent"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
)

// silenceReader yields an endless stream of 16-bit PCM silence, standing in
// for a leg's audio tap.
type silenceReader struct{}

func (silenceReader) Read(p []byte) (int, error) { return len(p), nil }

// stuckWSServer accepts one WebSocket client, completes the handshake and
// then stops reading. The tiny receive buffer makes the client's send buffer
// fill in milliseconds instead of megabytes, which is what a real provider
// that stops draining looks like from our side.
func stuckWSServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(4096)
		}
		if _, err := ws.Upgrade(conn); err != nil {
			_ = conn.Close()
			return
		}
		// Hold the connection open and never drain it.
		<-t.Context().Done()
		_ = conn.Close()
	}()

	return "ws://" + ln.Addr().String()
}

// TestAgentSession_StuckPeerReleasesSession asserts a provider that stops
// draining cannot strand a leg: the write deadline breaks the send loop, the
// send loop's exit cancels the recv loop, and Start returns without waiting
// out the 60s read timeout. Before this, the session goroutine and its mixer
// tap leaked for the lifetime of the process.
func TestAgentSession_StuckPeerReleasesSession(t *testing.T) {
	prev := wsutilx.DefaultWriteTimeout.Load()
	wsutilx.DefaultWriteTimeout.Store(500 * time.Millisecond)
	t.Cleanup(func() { wsutilx.DefaultWriteTimeout.Store(prev) })

	url := stuckWSServer(t)
	sess := agent.NewPipecat(slog.Default())

	done := make(chan error, 1)
	go func() {
		done <- sess.Start(context.Background(), silenceReader{}, io.Discard, "",
			agent.Options{AgentID: url}, agent.Callbacks{})
	}()

	// Comfortably under wsutilx.DefaultReadTimeout (60s): if teardown waited
	// on the read side, this would time out.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Start did not return: the wedged write stranded the session")
	}

	if sess.Running() {
		t.Error("session still reports Running after teardown: a second attach would be rejected")
	}
}
