package stt

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// Only Deepgram flushes mid-stream today; the interface exists so the other
// providers are not forced to pretend they can.
var _ Finalizer = (*DeepgramTranscriber)(nil)

// dgEchoServer spins a WebSocket server that forwards every frame it receives
// to textCh (OpText) or binaryCh (OpBinary), and returns a live client conn
// dialed against it. Both are torn down by t.Cleanup.
func dgEchoServer(t *testing.T) (client net.Conn, textCh, binaryCh chan []byte) {
	t.Helper()
	textCh = make(chan []byte, 8)
	binaryCh = make(chan []byte, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			data, op, err := wsutil.ReadClientData(conn)
			if err != nil {
				return
			}
			switch op {
			case ws.OpText:
				textCh <- data
			case ws.OpBinary:
				binaryCh <- data
			}
		}
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, _, err := ws.Dialer{}.Dial(context.Background(), wsURL)
	if err != nil {
		t.Fatalf("dial test websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, textCh, binaryCh
}

func dgRecv(t *testing.T, ch chan []byte, what string) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s frame", what)
		return nil
	}
}

// The whole point of the feature: the exact Deepgram control frame goes out as
// TEXT, and the session survives it — audio written afterwards still lands.
func TestDeepgramFinalize_SendsExactFrameAndKeepsSocketOpen(t *testing.T) {
	conn, textCh, binaryCh := dgEchoServer(t)

	tr := &DeepgramTranscriber{log: slog.Default()}
	lw := wsutilx.NewLockedWriter(conn)
	tr.setWriter(lw)

	if err := tr.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// Hardcoded on purpose: mutating the production frame must not move this.
	want := []byte(`{"type":"Finalize"}`)
	if got := dgRecv(t, textCh, "finalize"); !bytes.Equal(got, want) {
		t.Errorf("finalize frame = %q, want %q", got, want)
	}

	// The socket is still usable: a later audio frame reaches the server.
	audio := []byte{0x01, 0x02, 0x03, 0x04}
	if err := lw.WriteBinary(audio); err != nil {
		t.Fatalf("WriteBinary after finalize: %v", err)
	}
	if got := dgRecv(t, binaryCh, "audio"); !bytes.Equal(got, audio) {
		t.Errorf("audio frame = %q, want %q", got, audio)
	}
}

func TestDeepgramFinalize_RefusesWithoutLiveWriter(t *testing.T) {
	t.Run("never_started", func(t *testing.T) {
		tr := &DeepgramTranscriber{log: slog.Default()}
		if err := tr.Finalize(context.Background()); err == nil {
			t.Error("Finalize on a transcriber that never started returned nil, want error")
		}
	})

	// A caller whose connection is already gone must not put a frame on the
	// provider socket just to throw the answer away.
	t.Run("caller_context_cancelled", func(t *testing.T) {
		conn, textCh, _ := dgEchoServer(t)
		tr := &DeepgramTranscriber{log: slog.Default()}
		tr.setWriter(wsutilx.NewLockedWriter(conn))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := tr.Finalize(ctx); err == nil {
			t.Error("Finalize with a cancelled context returned nil, want error")
		}
		select {
		case got := <-textCh:
			t.Errorf("Finalize wrote %q despite a cancelled context", got)
		case <-time.After(200 * time.Millisecond):
		}
	})
}

// The writer must genuinely be released when the session ends, or a late
// Finalize writes to a dead conn. The socket is deliberately left OPEN so the
// only thing that can produce the error is the cleared field.
func TestDeepgramFinalize_AfterSessionEndsErrors(t *testing.T) {
	conn, textCh, _ := dgEchoServer(t)

	tr := &DeepgramTranscriber{log: slog.Default()}
	tr.setWriter(wsutilx.NewLockedWriter(conn))
	if err := tr.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize while connected: %v", err)
	}
	dgRecv(t, textCh, "finalize")

	tr.clearWriter()
	if err := tr.Finalize(context.Background()); err == nil {
		t.Fatal("Finalize after the session ended returned nil, want error")
	}
}

// The capability stays honestly scoped: internal/stt holds exactly three
// providers (azure.go, deepgram.go, elevenlabs.go) and only one can flush.
func TestSTTFinalizerConformance(t *testing.T) {
	log := slog.Default()

	if _, ok := Provider(NewDeepgram(log)).(Finalizer); !ok {
		t.Error("*DeepgramTranscriber does not implement Finalizer")
	}
	if _, ok := Provider(NewAzure("region", log)).(Finalizer); ok {
		t.Error("*AzureTranscriber implements Finalizer; VoiceBlender's Azure " +
			"integration has no mid-stream flush, so it must not claim one")
	}
	if _, ok := Provider(NewElevenLabs(log)).(Finalizer); ok {
		t.Error("*ElevenLabsTranscriber implements Finalizer; VoiceBlender's " +
			"ElevenLabs integration has no mid-stream flush, so it must not claim one")
	}
}
