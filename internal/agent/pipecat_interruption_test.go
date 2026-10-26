package agent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws/wsutil"
	"google.golang.org/protobuf/encoding/protowire"
)

// Pipecat frames are encoded by hand to upstream's frames.proto layout, so a
// vendored proto that drifts from it cannot pass by agreeing with itself.
func pipecatWire(field protowire.Number, inner []byte) []byte {
	b := protowire.AppendTag(nil, field, protowire.BytesType)
	return protowire.AppendBytes(b, inner)
}

func pipecatWireAudio(fill byte) []byte {
	inner := protowire.AppendTag(nil, 3, protowire.BytesType)
	inner = protowire.AppendBytes(inner, []byte{fill, 0, fill, 0})
	return pipecatWire(2, inner)
}

func pipecatWireMessage(data string) []byte {
	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendString(inner, data)
	return pipecatWire(4, inner)
}

func pipecatWireInterruption(id uint64) []byte {
	inner := protowire.AppendTag(nil, 1, protowire.VarintType)
	inner = protowire.AppendVarint(inner, id)
	return pipecatWire(5, inner)
}

func TestPipecatRecvLoopInterruptionAndRTVI(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()

	rec := &speakRecorder{}
	var mu sync.Mutex
	var user, bot []string
	cb := Callbacks{
		OnUserTranscript: func(s string) { mu.Lock(); user = append(user, s); mu.Unlock() },
		OnAgentResponse:  func(s string) { mu.Lock(); bot = append(bot, s); mu.Unlock() },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewPipecat(slog.New(slog.NewTextHandler(io.Discard, nil))).recvLoop(context.Background(), cli, rec, cb)
	}()

	frames := [][]byte{
		pipecatWireMessage(`{"label":"rtvi-ai","type":"user-transcription","data":{"text":"hello","final":true}}`),
		pipecatWireAudio(1),
		pipecatWireAudio(2),
		pipecatWireInterruption(42),
		pipecatWireAudio(3),
		pipecatWireMessage(`{"label":"rtvi-ai","type":"bot-transcription","data":{"text":"hi there"}}`),
	}
	for _, f := range frames {
		if err := wsutil.WriteServerBinary(srv, f); err != nil {
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
		t.Errorf("ops = %v, want %v", rec.ops, want)
	}
	if want := []string{"hello"}; !reflect.DeepEqual(user, want) {
		t.Errorf("user transcripts = %v, want %v", user, want)
	}
	if want := []string{"hi there"}; !reflect.DeepEqual(bot, want) {
		t.Errorf("bot transcripts = %v, want %v", bot, want)
	}
}
