package agent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/providercfg"
	"github.com/gobwas/ws/wsutil"
)

func TestVAPIUserInterruptedDiscardsBufferedSpeech(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()

	rec := &speakRecorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewVAPI(providercfg.VAPI{}, slog.New(slog.NewTextHandler(io.Discard, nil))).recvLoop(context.Background(), cli, rec, Callbacks{})
	}()

	writes := []func() error{
		func() error { return wsutil.WriteServerBinary(srv, []byte{1, 0, 1, 0}) },
		func() error { return wsutil.WriteServerBinary(srv, []byte{2, 0, 2, 0}) },
		func() error {
			return wsutil.WriteServerText(srv, []byte(`{"type":"speech-update","status":"started","role":"user","turn":1}`))
		},
		func() error {
			return wsutil.WriteServerText(srv, []byte(`{"type":"user-interrupted","turnId":"turn-1"}`))
		},
		func() error { return wsutil.WriteServerBinary(srv, []byte{3, 0, 3, 0}) },
	}
	for _, w := range writes {
		if err := w(); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}
	srv.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recvLoop did not exit")
	}

	if want := []string{"audio:1", "audio:2", "discard", "audio:3"}; !reflect.DeepEqual(rec.ops, want) {
		t.Fatalf("ops = %v, want %v", rec.ops, want)
	}
}
