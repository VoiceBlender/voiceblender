package lkmedia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/livekit/protocol/livekit"
)

// syncBuffer is a mutex-guarded log sink.
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

// TestSignalSendUnblocksOnWedgedWrite asserts SignalClient.send is bounded
// by a write deadline. Without one, a peer that stops draining pins the
// caller and holds writeMu, wedging every other Send* on the client.
func TestSignalSendUnblocksOnWedgedWrite(t *testing.T) {
	prev := wsutilx.DefaultWriteTimeout.Load()
	wsutilx.DefaultWriteTimeout.Store(80 * time.Millisecond)
	t.Cleanup(func() { wsutilx.DefaultWriteTimeout.Store(prev) })

	sc := newStuckConn()
	t.Cleanup(func() { _ = sc.Close() })

	c := &SignalClient{conn: sc, log: slog.Default()}
	req := &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Mute{
			Mute: &livekit.MuteTrackRequest{Sid: "TR_test", Muted: true},
		},
	}

	errCh := make(chan error, 1)
	go func() { errCh <- c.send(req) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("send err = %v, want os.ErrDeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return: the signal write is unbounded")
	}
}

// TestSignalReadMessageLogsCloseCode asserts the peer's close code and
// reason reach the log. gobwas answers the close frame inside
// ReadServerData and surfaces it as a ClosedError, never as an OpClose
// opcode, so the classification has to live on the error path.
func TestSignalReadMessageLogsCloseCode(t *testing.T) {
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })

	sink := &syncBuffer{}
	c := &SignalClient{conn: cli, log: slog.New(slog.NewTextHandler(sink, nil))}

	body := ws.NewCloseFrameBody(ws.StatusInternalServerError, "livekit boom")
	go func() {
		_ = wsutil.WriteServerMessage(srv, ws.OpClose, body)
		// gobwas echoes a close frame back; absorb it so the reader isn't
		// blocked writing into an unread pipe.
		buf := make([]byte, 64)
		_, _ = srv.Read(buf)
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.readMessage(); err == nil {
			t.Error("readMessage returned nil error on close frame")
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readMessage did not return after close frame")
	}

	out := sink.String()
	if !strings.Contains(out, "1011") {
		t.Errorf("close code 1011 not logged; got %q", out)
	}
	if !strings.Contains(out, "livekit boom") {
		t.Errorf("close reason not logged; got %q", out)
	}
}

// scriptedConn replays a canned byte stream to Read and hands every Write
// to a channel. Reads block once the script is exhausted, so the reader
// under test stays alive until the test closes it.
type scriptedConn struct {
	mu     sync.Mutex
	rd     *bytes.Reader
	writes chan []byte
	closed chan struct{}
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	n, err := c.rd.Read(p)
	c.mu.Unlock()
	if err == io.EOF {
		<-c.closed
		return 0, io.EOF
	}
	return n, err
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	c.writes <- append([]byte(nil), p...)
	return len(p), nil
}

func (c *scriptedConn) Close() error                     { return nil }
func (c *scriptedConn) LocalAddr() net.Addr              { return nil }
func (c *scriptedConn) RemoteAddr() net.Addr             { return nil }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

// TestSignalControlReplyTakesWriteLock asserts the pong answering a peer
// ping goes out under writeMu. gobwas would write it straight to the conn
// from the read goroutine, splicing it into whatever frame an in-flight
// Send* is midway through emitting.
func TestSignalControlReplyTakesWriteLock(t *testing.T) {
	var script bytes.Buffer
	if err := ws.WriteFrame(&script, ws.NewPingFrame([]byte("hb"))); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	sc := &scriptedConn{
		rd:     bytes.NewReader(script.Bytes()),
		writes: make(chan []byte, 4),
		closed: make(chan struct{}),
	}
	c := &SignalClient{conn: sc, log: slog.Default()}

	c.writeMu.Lock() // stand-in for a Send* that is mid-frame

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = c.readData()
	}()

	select {
	case b := <-sc.writes:
		t.Fatalf("%d bytes written while writeMu was held: the control reply bypasses the send lock", len(b))
	case <-time.After(150 * time.Millisecond):
	}

	c.writeMu.Unlock()

	select {
	case b := <-sc.writes:
		// One Write per reply, so the frame cannot be split by another writer.
		f, err := ws.ReadFrame(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("reply is not a whole frame: %v", err)
		}
		if f.Header.OpCode != ws.OpPong {
			t.Fatalf("reply opcode = %v, want pong", f.Header.OpCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pong after writeMu was released")
	}

	close(sc.closed)
	<-done
}

// TestSignalRecvLoopClassifiesNormalClose asserts a graceful 1000 close is
// recorded as a clean disconnect. gobwas surfaces it as a ClosedError, so a
// check for io.EOF alone reports every normal LiveKit hangup as an error.
func TestSignalRecvLoopClassifiesNormalClose(t *testing.T) {
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = srv.Close() })

	c := &SignalClient{
		conn:   cli,
		log:    slog.Default(),
		events: make(chan SignalEvent, 4),
		done:   make(chan struct{}),
	}

	go func() {
		body := ws.NewCloseFrameBody(ws.StatusNormalClosure, "bye")
		_ = wsutil.WriteServerMessage(srv, ws.OpClose, body)
		_, _ = io.Copy(io.Discard, srv) // absorb the echoed close
	}()

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		c.recvLoop(context.Background(), func() {})
	}()

	select {
	case <-recvDone:
	case <-time.After(2 * time.Second):
		t.Fatal("recvLoop did not return after close frame")
	}

	if got := c.CloseReason(); got != "livekit_signal_closed" {
		t.Errorf("CloseReason = %q, want livekit_signal_closed", got)
	}
	if err := c.Err(); err != nil {
		t.Errorf("Err = %v, want nil for a normal closure", err)
	}
}
