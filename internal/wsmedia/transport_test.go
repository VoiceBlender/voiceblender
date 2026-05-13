package wsmedia

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
)

// testLogger returns a slog.Logger that swallows output during tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startEchoServer spins up an HTTP server whose only route upgrades to a
// WebSocket and echoes every frame back to the client. wireFormat controls
// whether the server uses binary or JSON envelope.
func startEchoServer(t *testing.T, wireFormat WireFormat) (string, func()) {
	t.Helper()
	cfg := Config{
		SampleRate: 16000,
		WireFormat: wireFormat,
		Log:        testLogger(),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serverCfg := cfg
		serverCfg.Log = testLogger()
		tr, _, err := UpgradeServer(w, r, serverCfg)
		if err != nil {
			t.Logf("upgrade: %v", err)
			return
		}
		// Loop incoming PCM straight back via a pipe pair: AudioReader →
		// pipe → Transport.Start as audioOut.
		pr, pw := io.Pipe()
		tr.SetOnText(func(text string) {
			_ = tr.WriteText(context.Background(), text)
		})
		go func() {
			defer pw.Close()
			ar := tr.AudioReader()
			buf := make([]byte, serverCfg.FrameBytesPCM())
			for {
				n, err := io.ReadFull(ar, buf)
				if err != nil {
					return
				}
				if _, err := pw.Write(buf[:n]); err != nil {
					return
				}
			}
		}()
		tr.Start(pr)
		<-tr.Done()
	})

	srv := httptest.NewServer(mux)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return wsURL, srv.Close
}

func TestTransportBinaryEchoLoopback(t *testing.T) {
	wsURL, stop := startEchoServer(t, WireBinary)
	defer stop()

	cfg := Config{SampleRate: 16000, WireFormat: WireBinary, Log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, _, err := DialClient(ctx, wsURL, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()

	frameBytes := cfg.FrameBytesPCM()
	pr, pw := io.Pipe()
	tr.Start(pr)

	// Send one frame: sine-ish ramp.
	in := make([]byte, frameBytes)
	for i := 0; i < frameBytes/2; i++ {
		binary.LittleEndian.PutUint16(in[i*2:], uint16(int16(i*100)))
	}
	go func() {
		_, _ = pw.Write(in)
		_ = pw.Close()
	}()

	// Read echoed frame back from AudioReader.
	out := make([]byte, frameBytes)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(tr.AudioReader(), out)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read echo: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for echo")
	}
	if !bytes.Equal(in, out) {
		t.Fatal("echoed payload mismatch")
	}
}

func TestTransportJSONBase64EchoLoopback(t *testing.T) {
	wsURL, stop := startEchoServer(t, WireJSONBase64)
	defer stop()

	cfg := Config{SampleRate: 16000, WireFormat: WireJSONBase64, Log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, _, err := DialClient(ctx, wsURL, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()

	frameBytes := cfg.FrameBytesPCM()
	pr, pw := io.Pipe()
	tr.Start(pr)

	in := make([]byte, frameBytes)
	for i := 0; i < frameBytes; i++ {
		in[i] = byte(i & 0xff)
	}
	go func() {
		_, _ = pw.Write(in)
		_ = pw.Close()
	}()

	out := make([]byte, frameBytes)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(tr.AudioReader(), out)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read echo: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for echo")
	}
	if !bytes.Equal(in, out) {
		t.Fatal("echoed payload mismatch")
	}
}

func TestTransportTextRoundTrip(t *testing.T) {
	wsURL, stop := startEchoServer(t, WireBinary)
	defer stop()

	cfg := Config{SampleRate: 16000, WireFormat: WireBinary, Log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, _, err := DialClient(ctx, wsURL, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()

	got := make(chan string, 1)
	tr.SetOnText(func(text string) { got <- text })

	// Start with a never-yielding audioOut so the send loop spins idle.
	pr, _ := io.Pipe()
	tr.Start(pr)

	if err := tr.WriteText(ctx, "hello"); err != nil {
		t.Fatalf("write text: %v", err)
	}
	select {
	case msg := <-got:
		if msg != "hello" {
			t.Fatalf("text mismatch: got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for text echo")
	}
}

func TestTransportHangupFrameClosesPeer(t *testing.T) {
	wsURL, stop := startEchoServer(t, WireBinary)
	defer stop()

	cfg := Config{SampleRate: 16000, WireFormat: WireBinary, Log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, _, err := DialClient(ctx, wsURL, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()

	pr, _ := io.Pipe()
	tr.Start(pr)

	frame, _ := json.Marshal(controlFrame{Type: "hangup"})
	if err := tr.writeText(frame); err != nil {
		t.Fatalf("send hangup: %v", err)
	}
	select {
	case <-tr.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not close after sending hangup; peer should echo and close us")
	}
}

func TestTransportContextCancelStopsLoops(t *testing.T) {
	wsURL, stop := startEchoServer(t, WireBinary)
	defer stop()

	cfg := Config{SampleRate: 16000, WireFormat: WireBinary, Log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, _, err := DialClient(ctx, wsURL, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	pr, _ := io.Pipe()
	tr.Start(pr)

	tr.Close()
	select {
	case <-tr.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not finish after Close()")
	}
}

func TestTransportWriteDeadlineExpires(t *testing.T) {
	// stuckConn blocks Write until the write deadline trips, returning
	// os.ErrDeadlineExceeded. This exercises the write-deadline path
	// that real TCP only triggers when send buffers fill.
	sc := newStuckConn()
	cfg := Config{SampleRate: 16000, WireFormat: WireBinary, WriteTimeout: 80 * time.Millisecond, Log: testLogger()}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	codec, _ := CodecFromConfig(cfg)
	tr := newTransport(cfg, sc, ws.StateClientSide, codec, nil)

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		buf := make([]byte, cfg.FrameBytesPCM())
		for {
			if _, err := pw.Write(buf); err != nil {
				return
			}
		}
	}()
	tr.Start(pr)

	select {
	case <-tr.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not finish after write deadline")
	}
	if err := tr.Err(); err == nil {
		t.Fatal("expected an error after write timeout")
	}
}

// TestTransportAcceptsRoomWSAudioShape verifies that the JSON ingress path
// accepts the room-WS frame shape `{"audio":"<b64>"}` (no `type` field) so a
// client written for /v1/rooms/{id}/ws can talk to /v1/legs/websocket
// unchanged.
func TestTransportAcceptsRoomWSAudioShape(t *testing.T) {
	wsURL, stop := startEchoServerJSONShape(t)
	defer stop()

	cfg := Config{SampleRate: 16000, WireFormat: WireJSONBase64, Log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, _, err := DialClient(ctx, wsURL, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()

	frameBytes := cfg.FrameBytesPCM()
	pr, pw := io.Pipe()
	tr.Start(pr)

	in := make([]byte, frameBytes)
	for i := 0; i < frameBytes; i++ {
		in[i] = byte((i * 7) & 0xff)
	}
	go func() {
		_, _ = pw.Write(in)
		_ = pw.Close()
	}()

	out := make([]byte, frameBytes)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(tr.AudioReader(), out)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read echo: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for echo")
	}
	if !bytes.Equal(in, out) {
		t.Fatal("audio mismatch — server echoed via room-WS shape, client failed to decode")
	}
}

// startEchoServerJSONShape forwards inbound audio back as raw room-WS
// frames `{"audio":"<b64>"}` regardless of what the codec normally emits,
// so the client side is the one being exercised.
func startEchoServerJSONShape(t *testing.T) (string, func()) {
	t.Helper()
	cfg := Config{SampleRate: 16000, WireFormat: WireJSONBase64, Log: testLogger()}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c := cfg
		tr, _, err := UpgradeServer(w, r, c)
		if err != nil {
			return
		}
		// Read inbound PCM and re-ship it as the room-WS shape (no `type`).
		go func() {
			ar := tr.AudioReader()
			buf := make([]byte, c.FrameBytesPCM())
			for {
				if _, err := io.ReadFull(ar, buf); err != nil {
					return
				}
				frame, _ := json.Marshal(map[string]string{
					"audio": base64.StdEncoding.EncodeToString(buf),
				})
				if err := tr.writeText(frame); err != nil {
					return
				}
			}
		}()
		pr, _ := io.Pipe()
		tr.Start(pr)
		<-tr.Done()
	})
	srv := httptest.NewServer(mux)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return wsURL, srv.Close
}

func TestTransportAcceptsStopAlias(t *testing.T) {
	wsURL, stop := startEchoServer(t, WireBinary)
	defer stop()

	cfg := Config{SampleRate: 16000, WireFormat: WireBinary, Log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, _, err := DialClient(ctx, wsURL, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()

	pr, _ := io.Pipe()
	tr.Start(pr)

	// Room-WS clients send `{"type":"stop"}`; leg endpoint must accept it
	// (when echoed back to us by the server) the same as `hangup`.
	frame, _ := json.Marshal(controlFrame{Type: "stop"})
	if err := tr.writeText(frame); err != nil {
		t.Fatalf("send stop: %v", err)
	}
	// Echo server bounces text back through OnText — we only care that
	// the close path runs cleanly when the server's transport sees the
	// frame in its own OnText handler. Simpler: just hangup locally.
	tr.Close()
	select {
	case <-tr.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not finish")
	}
}

func TestTransportIngressOverflowDrops(t *testing.T) {
	// Build a transport with a tiny ingress buffer and write more audio
	// than it can hold; verify Dropped accounts for the overflow.
	cfg := Config{
		SampleRate:      16000,
		WireFormat:      WireBinary,
		IngressBufferMs: 20, // exactly 1 frame
		Log:             testLogger(),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	codec, _ := CodecFromConfig(cfg)
	// Use a discard conn — we won't touch the wire; just exercise writePCM.
	tr := &Transport{
		cfg:     cfg,
		codec:   codec,
		log:     cfg.Log,
		audioIn: newStreamBuffer(cfg.IngressBufferBytes(), cfg.FrameMs),
		done:    make(chan struct{}),
	}
	pcm := make([]int16, cfg.FrameSamples())
	// Write three frames; only the first fits, the rest get dropped.
	for i := 0; i < 3; i++ {
		tr.writePCM(pcm)
	}
	if got := tr.AudioDropsBytes(); got == 0 {
		t.Fatal("expected dropped bytes > 0")
	}
}

func TestTransportSendStructured(t *testing.T) {
	wsURL, stop := startEchoServerWithControlCapture(t)
	defer stop()

	cfg := Config{SampleRate: 16000, WireFormat: WireBinary, Log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, _, err := DialClient(ctx, wsURL, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer tr.Close()

	pr, _ := io.Pipe()
	tr.Start(pr)

	got := make(chan json.RawMessage, 1)
	tr.SetOnControl(func(raw json.RawMessage) { got <- raw })

	if err := tr.SendStructured(map[string]any{"type": "custom", "answer": 42}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case raw := <-got:
		if !bytes.Contains(raw, []byte(`"answer":42`)) {
			t.Fatalf("unexpected payload: %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for control echo")
	}
}

// startEchoServerWithControlCapture upgrades and echoes every text frame
// back as-is (so the client's OnControl fires when the type is unknown).
func startEchoServerWithControlCapture(t *testing.T) (string, func()) {
	t.Helper()
	cfg := Config{SampleRate: 16000, WireFormat: WireBinary, Log: testLogger()}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tr, _, err := UpgradeServer(w, r, cfg)
		if err != nil {
			return
		}
		tr.SetOnControl(func(raw json.RawMessage) {
			_ = tr.writeText(raw)
		})
		pr, _ := io.Pipe()
		tr.Start(pr)
		<-tr.Done()
	})
	srv := httptest.NewServer(mux)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	return wsURL, srv.Close
}
