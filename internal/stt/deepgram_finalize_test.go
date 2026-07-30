package stt

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	tr.setWriter(&dgLockedWriter{conn: conn})

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
	lw := &dgLockedWriter{conn: conn}
	if err := lw.WriteBinary(audio); err != nil {
		t.Fatalf("WriteBinary after finalize: %v", err)
	}
	if got := dgRecv(t, binaryCh, "audio"); !bytes.Equal(got, audio) {
		t.Errorf("audio frame = %q, want %q", got, audio)
	}
}

func TestDeepgramFinalize_RefusesWithoutLiveWrite(t *testing.T) {
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
		tr.setWriter(&dgLockedWriter{conn: conn})

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
	tr.setWriter(&dgLockedWriter{conn: conn})
	if err := tr.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize while connected: %v", err)
	}
	dgRecv(t, textCh, "finalize")

	tr.clearWriter()
	if err := tr.Finalize(context.Background()); err == nil {
		t.Fatal("Finalize after the session ended returned nil, want error")
	}
}

// dgDeadlineConn records write deadlines and swallows writes, so the bound on
// a writer hold can be asserted without a peer that stops draining.
type dgDeadlineConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (c *dgDeadlineConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *dgDeadlineConn) Close() error                { return nil }

func (c *dgDeadlineConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *dgDeadlineConn) writeDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

// Finalize is called from the VSI connection's recv loop, so it may acquire
// the writer lock. That is only safe while every hold is bounded — an
// unbounded hold anywhere on this writer stalls the loop and every later
// command from that client.
func TestDeepgramWriter_BoundsEveryHold(t *testing.T) {
	cases := []struct {
		name  string
		write func(lw *dgLockedWriter) error
	}{
		{"text", func(lw *dgLockedWriter) error { return lw.WriteText([]byte("x")) }},
		{"binary", func(lw *dgLockedWriter) error { return lw.WriteBinary([]byte("x")) }},
		{"control", func(lw *dgLockedWriter) error { return lw.WriteControl(ws.OpPong, nil) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &dgDeadlineConn{}
			before := time.Now()
			if err := tc.write(&dgLockedWriter{conn: conn}); err != nil {
				t.Fatalf("write: %v", err)
			}
			got := conn.writeDeadline()
			if got.IsZero() {
				t.Fatal("no write deadline set: this hold is unbounded")
			}
			if !got.After(before) {
				t.Errorf("write deadline %v is not in the future (now %v)", got, before)
			}
			if got.After(before.Add(30 * time.Second)) {
				t.Errorf("write deadline %v is too far out to bound anything", got)
			}
		})
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
