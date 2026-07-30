package lkmedia

import (
	"bytes"
	"errors"
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
