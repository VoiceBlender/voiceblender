package sip

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func startAdmissionEngine(t *testing.T) (*Engine, sip.Uri, *sipgo.Client) {
	t.Helper()
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
	listening := make(chan struct{})
	engine.OnListening(func() { close(listening) })

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- engine.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Errorf("Serve did not return after ctx cancel")
		}
	})
	select {
	case <-listening:
	case <-time.After(3 * time.Second):
		t.Fatal("OnListening never fired")
	}

	clientPort := pickFreePort(t, "udp")
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("admission-test"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	t.Cleanup(func() { ua.Close() })
	cli, err := sipgo.NewClient(ua,
		sipgo.WithClientHostname("127.0.0.1"),
		sipgo.WithClientPort(clientPort),
		sipgo.WithClientConnectionAddr(fmt.Sprintf("127.0.0.1:%d", clientPort)),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return engine, sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: udpPort}, cli
}

func newTestRequest(method sip.RequestMethod, target sip.Uri, toTag bool) *sip.Request {
	req := sip.NewRequest(method, target)
	to := &sip.ToHeader{Address: target, Params: sip.NewParams()}
	if toTag {
		to.Params.Add("tag", sip.GenerateTagN(8))
	}
	req.AppendHeader(to)
	from := &sip.FromHeader{Address: target, Params: sip.NewParams()}
	from.Params.Add("tag", sip.GenerateTagN(8))
	req.AppendHeader(from)
	return req
}

func doRequest(t *testing.T, cli *sipgo.Client, req *sip.Request) *sip.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := cli.Do(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", req.Method, err)
	}
	return res
}

func TestEngine_AdmissionRefusesOptionsOutsideDialog(t *testing.T) {
	engine, target, cli := startAdmissionEngine(t)

	if res := doRequest(t, cli, newTestRequest(sip.OPTIONS, target, false)); res.StatusCode != sip.StatusOK {
		t.Fatalf("OPTIONS with no gate = %d, want 200", res.StatusCode)
	}

	engine.SetAdmission(func() (bool, int) { return false, 42 })
	res := doRequest(t, cli, newTestRequest(sip.OPTIONS, target, false))
	if res.StatusCode != sip.StatusServiceUnavailable {
		t.Fatalf("OPTIONS while refusing = %d, want 503", res.StatusCode)
	}
	if h := res.GetHeader("Retry-After"); h == nil || h.Value() != "42" {
		t.Fatalf("Retry-After = %v, want 42", h)
	}
	if h := res.GetHeader("Allow"); h == nil || h.Value() == "" {
		t.Fatal("503 missing Allow header")
	}

	if res := doRequest(t, cli, newTestRequest(sip.OPTIONS, target, true)); res.StatusCode != sip.StatusOK {
		t.Fatalf("in-dialog OPTIONS while refusing = %d, want 200", res.StatusCode)
	}

	engine.SetAdmission(func() (bool, int) { return true, 0 })
	if res := doRequest(t, cli, newTestRequest(sip.OPTIONS, target, false)); res.StatusCode != sip.StatusOK {
		t.Fatalf("OPTIONS after accepting again = %d, want 200", res.StatusCode)
	}
}

func TestEngine_AdmissionRefusesInviteBeforeAny1xx(t *testing.T) {
	engine, target, cli := startAdmissionEngine(t)
	var offered atomic.Bool
	engine.OnInvite(func(*InboundCall) { offered.Store(true) })
	engine.SetAdmission(func() (bool, int) { return false, 0 })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := cli.TransactionRequest(ctx, newTestRequest(sip.INVITE, target, false))
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}
	defer tx.Terminate()
	select {
	case res := <-tx.Responses():
		if res.StatusCode != sip.StatusServiceUnavailable {
			t.Fatalf("first response = %d, want 503", res.StatusCode)
		}
		if h := res.GetHeader("Retry-After"); h != nil {
			t.Fatalf("Retry-After sent with retryAfter 0: %v", h)
		}
	case <-ctx.Done():
		t.Fatal("no response to INVITE")
	}
	if offered.Load() {
		t.Fatal("refused INVITE reached the application")
	}
}
