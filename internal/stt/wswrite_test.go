package stt

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// pcmReader yields an endless stream of 16-bit PCM silence.
type pcmReader struct{}

func (pcmReader) Read(p []byte) (int, error) { return len(p), nil }

// syncBuffer is a mutex-guarded log sink; the recv loop writes to it from
// its own goroutine while the test reads it.
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

func captureLogger() (*slog.Logger, *syncBuffer) {
	sb := &syncBuffer{}
	return slog.New(slog.NewTextHandler(sb, nil)), sb
}

// TestSTTSendLoopsUnblockOnWedgedWrite asserts a peer that stops draining
// cannot pin an STT send loop forever. Every send loop returns on a write
// error, so the write deadline is what lets Start's wg.Wait() return, which
// is what runs the deferred conn.Close() and the API-side cleanup.
func TestSTTSendLoopsUnblockOnWedgedWrite(t *testing.T) {
	prev := wsutilx.DefaultWriteTimeout.Load()
	wsutilx.DefaultWriteTimeout.Store(80 * time.Millisecond)
	t.Cleanup(func() { wsutilx.DefaultWriteTimeout.Store(prev) })

	cases := []struct {
		name string
		run  func(ctx context.Context, lw *wsutilx.LockedWriter)
	}{
		{"deepgram", func(ctx context.Context, lw *wsutilx.LockedWriter) {
			NewDeepgram(slog.Default()).sendLoop(ctx, pcmReader{}, lw)
		}},
		{"elevenlabs", func(ctx context.Context, lw *wsutilx.LockedWriter) {
			NewElevenLabs(slog.Default()).sendLoop(ctx, pcmReader{}, lw)
		}},
		{"azure", func(ctx context.Context, lw *wsutilx.LockedWriter) {
			NewAzure("test", slog.Default()).sendLoop(ctx, pcmReader{}, lw, "reqid")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := newStuckConn()
			t.Cleanup(func() { _ = sc.Close() })

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.run(context.Background(), wsutilx.NewLockedWriter(sc))
			}()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s sendLoop did not return: the write is unbounded", tc.name)
			}
		})
	}
}

// TestSTTRecvLoopsLogCloseCode asserts an error close is distinguishable
// from a clean one: the peer's close code and reason reach the log.
func TestSTTRecvLoopsLogCloseCode(t *testing.T) {
	cases := []struct {
		name string
		run  func(ctx context.Context, log *slog.Logger, conn net.Conn)
	}{
		{"deepgram", func(ctx context.Context, log *slog.Logger, conn net.Conn) {
			tr := NewDeepgram(log)
			tr.recvLoop(ctx, conn, wsutilx.NewLockedWriter(conn), func(string, bool) {})
		}},
		{"elevenlabs", func(ctx context.Context, log *slog.Logger, conn net.Conn) {
			tr := NewElevenLabs(log)
			tr.recvLoop(ctx, conn, wsutilx.NewLockedWriter(conn), false, func(string, bool) {})
		}},
		{"azure", func(ctx context.Context, log *slog.Logger, conn net.Conn) {
			tr := NewAzure("test", log)
			tr.recvLoop(ctx, conn, wsutilx.NewLockedWriter(conn), func(string, bool) {}, false)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, srv := net.Pipe()
			t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })

			log, sink := captureLogger()
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.run(context.Background(), log, cli)
			}()

			// Written from a goroutine: a recv loop that reads the close
			// header but never drains the payload would otherwise deadlock
			// this unbuffered pipe instead of failing the assertion.
			body := ws.NewCloseFrameBody(ws.StatusInternalServerError, "upstream boom")
			go func() { _ = wsutil.WriteServerMessage(srv, ws.OpClose, body) }()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s recvLoop did not return after close frame", tc.name)
			}

			out := sink.String()
			if !strings.Contains(out, "1011") {
				t.Errorf("%s: close code 1011 not logged; got %q", tc.name, out)
			}
			if !strings.Contains(out, "upstream boom") {
				t.Errorf("%s: close reason not logged; got %q", tc.name, out)
			}
		})
	}
}
