package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
)

// stuckWSConn is an in-process net.Conn whose Write blocks until the stored
// write deadline trips (returning os.ErrDeadlineExceeded) or Close is called.
// Read serves any pre-loaded bytes, then blocks until Close and returns io.EOF.
// Modelled on internal/wsmedia/stuckconn_test.go — it deterministically
// exercises the write-deadline path that real TCP only reaches once the kernel
// send buffer fills.
type stuckWSConn struct {
	deadline atomic.Value // time.Time
	closed   chan struct{}
	once     sync.Once
	// closes counts every Close call and is incremented OUTSIDE once, so a
	// test can tell "closed twice" from "closed once". Putting it inside the
	// once would make the fake dedupe on the writer's behalf and no assertion
	// could ever see a missing sync.Once.
	closes atomic.Int64

	readMu  sync.Mutex
	readBuf []byte
}

func newStuckWSConn(preload []byte) *stuckWSConn {
	return &stuckWSConn{closed: make(chan struct{}), readBuf: preload}
}

func (s *stuckWSConn) Read(p []byte) (int, error) {
	s.readMu.Lock()
	if len(s.readBuf) > 0 {
		n := copy(p, s.readBuf)
		s.readBuf = s.readBuf[n:]
		s.readMu.Unlock()
		return n, nil
	}
	s.readMu.Unlock()
	<-s.closed
	return 0, io.EOF
}

func (s *stuckWSConn) Write(p []byte) (int, error) {
	dl, _ := s.deadline.Load().(time.Time)
	if dl.IsZero() {
		<-s.closed
		return 0, io.ErrClosedPipe
	}
	wait := time.Until(dl)
	if wait <= 0 {
		return 0, os.ErrDeadlineExceeded
	}
	select {
	case <-s.closed:
		return 0, io.ErrClosedPipe
	case <-time.After(wait):
		return 0, os.ErrDeadlineExceeded
	}
}

func (s *stuckWSConn) Close() error {
	s.closes.Add(1)
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *stuckWSConn) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *stuckWSConn) closeCount() int64 { return s.closes.Load() }

func (s *stuckWSConn) LocalAddr() net.Addr                { return nil }
func (s *stuckWSConn) RemoteAddr() net.Addr               { return nil }
func (s *stuckWSConn) SetDeadline(time.Time) error        { return nil }
func (s *stuckWSConn) SetReadDeadline(time.Time) error    { return nil }
func (s *stuckWSConn) SetWriteDeadline(t time.Time) error { s.deadline.Store(t); return nil }

// The pong reply the recv loop emits goes through the raw conn, not the locked
// writer, so it needs its own deadline. Without one a client that pings and
// then stops reading pins the recv loop forever — the permanent-leak chain
// this branch exists to close.
//
// The two cases are separate code paths on separate deadlines: a standalone
// control frame is answered by the loop body, one interleaved into a
// fragmented message is answered from the reader's OnIntermediate hook.
// Dropping either deadline alone leaves the other test green.
func TestVSIRecvLoopBoundsControlFrameReply(t *testing.T) {
	prev := wsutilx.DefaultWriteTimeout.Load()
	wsutilx.DefaultWriteTimeout.Store(50 * time.Millisecond)
	t.Cleanup(func() { wsutilx.DefaultWriteTimeout.Store(prev) })

	frames := func(t *testing.T, fs ...ws.Frame) []byte {
		t.Helper()
		var buf bytes.Buffer
		for _, f := range fs {
			if err := ws.WriteFrame(&buf, ws.MaskFrameInPlace(f)); err != nil {
				t.Fatalf("build client frame: %v", err)
			}
		}
		return buf.Bytes()
	}

	for _, tc := range []struct {
		name    string
		preload func(*testing.T) []byte
	}{
		{
			name:    "standalone ping",
			preload: func(t *testing.T) []byte { return frames(t, ws.NewPingFrame(nil)) },
		},
		{
			name: "ping interleaved into a fragmented text message",
			preload: func(t *testing.T) []byte {
				return frames(t,
					ws.NewFrame(ws.OpText, false, []byte(`{"type":`)),
					ws.NewPingFrame(nil),
					ws.NewFrame(ws.OpContinuation, true, []byte(`"pong"}`)),
				)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			sc := newStuckWSConn(tc.preload(t))
			var closed atomic.Bool

			done := make(chan string, 1)
			go func() {
				lw := wsutilx.NewServerLockedWriter(sc, func() { _ = sc.Close() })
				done <- s.vsiRecvLoop(context.Background(), sc, lw, &closed)
			}()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("vsiRecvLoop never returned: the control-frame reply is not bounded by a deadline")
			}
		})
	}
}

// syncBuffer is a concurrency-safe log sink: the handler logs from several
// goroutines, so a bare bytes.Buffer would race under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Binds the production wiring in vsi(): the handler really does build a writer
// that closes the conn on a missed deadline, so a client that stops draining is
// disconnected rather than pinning the handler and its event-bus subscription
// forever. The wsutilx unit tests build their own writer and would stay green
// if ws_events.go wired a plain client-side one with no onFail. The log
// assertion binds the second half — that the disconnect is attributed to the
// write deadline and not mis-reported as a peer close.
func TestVSIStalledClientIsDisconnected(t *testing.T) {
	prev := wsutilx.DefaultWriteTimeout.Load()
	wsutilx.DefaultWriteTimeout.Store(200 * time.Millisecond)
	t.Cleanup(func() { wsutilx.DefaultWriteTimeout.Store(prev) })

	s := newTestServer(t)
	logs := &syncBuffer{}
	s.Log = slog.New(slog.NewJSONHandler(logs, nil))
	srv := httptest.NewServer(s.Router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/v1/vsi"
	conn, _, _, err := ws.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial vsi: %v", err)
	}
	defer conn.Close()
	// Deliberately not reading: the kernel send buffer fills, the send loop's
	// write blocks, and the write deadline is the only thing that can end it.

	time.Sleep(100 * time.Millisecond) // let the handler subscribe

	// One shared 64 KiB string referenced by every event, so this costs a
	// transient marshal buffer per frame rather than 64 MiB of payload.
	big := strings.Repeat("x", 64*1024)
	for i := 0; i < 2000; i++ {
		s.Bus.Publish(events.DTMFReceived, &events.DTMFReceivedData{
			LegScope: events.LegScope{LegID: "leg-1"},
			Digit:    big,
		})
	}

	// Comfortably past the 200ms write deadline.
	time.Sleep(time.Second)

	// Now drain. Everything already queued arrives first; the read then ends
	// in EOF/reset if the server hung up, or in our own deadline if it did not.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 64*1024)
	for {
		if _, err := conn.Read(buf); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("server never disconnected the stalled client: the VSI write is unbounded in production")
			}
			break // server hung up — the deadline fired and closed the conn
		}
	}

	// The disconnect log is emitted as the handler unwinds, which may trail the
	// close the client just observed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(logs.String(), `"reason":"write_timeout"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("disconnect was not attributed to the write deadline:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestVSIExitReason(t *testing.T) {
	cases := []struct {
		name       string
		readReason string
		writeErr   error
		want       string
	}{
		{name: "no write error passes the read reason through", readReason: "peer_close", want: "peer_close"},
		{name: "stop is preserved", readReason: "stop", want: "stop"},
		{
			name:       "write deadline overrides the misleading peer_close",
			readReason: "peer_close",
			writeErr:   os.ErrDeadlineExceeded,
			want:       "write_timeout",
		},
		{
			// A broken pipe on the write side is collateral of the peer
			// leaving; the read reason already says so accurately.
			name:       "non-timeout write error does not override",
			readReason: "peer_close",
			writeErr:   io.ErrClosedPipe,
			want:       "peer_close",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vsiExitReason(tc.readReason, tc.writeErr); got != tc.want {
				t.Fatalf("vsiExitReason(%q, %v) = %q, want %q", tc.readReason, tc.writeErr, got, tc.want)
			}
		})
	}
}
