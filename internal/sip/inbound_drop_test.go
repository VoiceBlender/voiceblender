package sip

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestInboundCall_DropWithoutTransaction(t *testing.T) {
	(&InboundCall{}).Drop()
}

// Dropping an unanswered INVITE must end the dialog while keeping every final
// response off the wire.
func TestInboundCall_DropSendsNoFinalResponse(t *testing.T) {
	udpPort := pickFreePort(t, "udp")
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: udpPort,
		SIPHost:  "test-vb",
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	dialogEnded := make(chan bool, 4)
	engine.OnInvite(func(call *InboundCall) {
		if err := engine.DialogRespond(call.Dialog, sip.StatusTrying, "Trying", nil, engine.ServerHeader()); err != nil {
			t.Errorf("respond 100: %v", err)
		}
		call.Drop()
		select {
		case <-call.Dialog.Context().Done():
			dialogEnded <- true
		case <-time.After(2 * time.Second):
			dialogEnded <- false
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.Serve(ctx)

	clientPort := pickFreePort(t, "udp")
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("drop-test"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	defer ua.Close()
	cli, err := sipgo.NewClient(ua,
		sipgo.WithClientHostname("127.0.0.1"),
		sipgo.WithClientPort(clientPort),
		sipgo.WithClientConnectionAddr(fmt.Sprintf("127.0.0.1:%d", clientPort)),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	target := sip.Uri{Scheme: "sip", User: "test", Host: "127.0.0.1", Port: udpPort}
	sdp := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n" +
		"m=audio 40000 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
	newInvite := func() *sip.Request {
		req := sip.NewRequest(sip.INVITE, target)
		req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "caller", Host: "127.0.0.1", Port: clientPort}})
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody([]byte(sdp))
		return req
	}

	// Serve binds asynchronously, so retry until an INVITE draws a response.
	var (
		tx       sip.ClientTransaction
		first    *sip.Response
		deadline = time.Now().Add(3 * time.Second)
	)
	for first == nil && time.Now().Before(deadline) {
		tx, err = cli.TransactionRequest(context.Background(), newInvite())
		if err != nil {
			t.Fatalf("send INVITE: %v", err)
		}
		select {
		case first = <-tx.Responses():
		case <-time.After(300 * time.Millisecond):
			tx.Terminate()
		}
	}
	if first == nil {
		t.Fatal("INVITE never drew a response")
	}
	defer tx.Terminate()
	if first.StatusCode != sip.StatusTrying {
		t.Fatalf("first response = %d, want 100", first.StatusCode)
	}

	select {
	case ended := <-dialogEnded:
		if !ended {
			t.Fatal("dialog context still live after Drop")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("INVITE handler never ran")
	}

	select {
	case res := <-tx.Responses():
		t.Fatalf("got %d after Drop, want no further response", res.StatusCode)
	case <-time.After(time.Second):
	}
}
