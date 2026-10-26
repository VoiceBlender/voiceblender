package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/providercfg"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// holdWritesConn collects the upgrader's handshake response instead of
// sending it.
type holdWritesConn struct {
	net.Conn
	out bytes.Buffer
}

func (c *holdWritesConn) Write(p []byte) (int, error) { return c.out.Write(p) }

// speakFirstServer completes the WebSocket handshake and sends its first
// frame in the same Write as the 101 response, so the frame always lands in
// the dialer's bufio.Reader rather than on the socket.
func speakFirstServer(t *testing.T, op ws.OpCode, first []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		hc := &holdWritesConn{Conn: conn}
		if _, err := ws.Upgrade(hc); err != nil {
			return
		}
		if err := wsutil.WriteServerMessage(&hc.out, op, first); err != nil {
			return
		}
		if _, err := conn.Write(hc.out.Bytes()); err != nil {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}()
	return "ws://" + ln.Addr().String()
}

func waitText(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first frame sent with the handshake was lost")
	}
}

func TestPipecatKeepsFrameSentWithHandshake(t *testing.T) {
	url := speakFirstServer(t, ws.OpBinary,
		pipecatWireMessage(`{"label":"rtvi-ai","type":"bot-transcription","data":{"text":"greeting"}}`))

	got := make(chan string, 1)
	cb := Callbacks{OnAgentResponse: func(s string) { got <- s }}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go NewPipecat(slog.New(slog.NewTextHandler(io.Discard, nil))).Start(ctx, silenceReader{}, io.Discard, "", Options{AgentID: url}, cb)
	waitText(t, got, "greeting")
}

func TestVAPIKeepsFrameSentWithHandshake(t *testing.T) {
	listenURL := speakFirstServer(t, ws.OpText,
		[]byte(`{"type":"transcript","role":"assistant","transcript":"greeting"}`))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/call" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "c1", "listenUrl": listenURL})
	}))
	defer api.Close()

	got := make(chan string, 1)
	cb := Callbacks{OnAgentResponse: func(s string) { got <- s }}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s := NewVAPI(providercfg.VAPI{BaseURL: api.URL}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go s.Start(ctx, silenceReader{}, io.Discard, "key", Options{AgentID: "a"}, cb)
	waitText(t, got, "greeting")
}
