package leg

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsmedia"
)

func wsLegTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// wsEchoServer returns a URL backed by a WebSocket server that echoes
// every audio frame and every text message back to the client.
func wsEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	srvCfg := wsmedia.Config{SampleRate: 16000, WireFormat: wsmedia.WireBinary, Log: wsLegTestLogger()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c := srvCfg
		c.Log = wsLegTestLogger()
		if err := c.Validate(); err != nil {
			t.Logf("validate: %v", err)
			return
		}
		tr, _, err := wsmedia.UpgradeServer(w, r, c)
		if err != nil {
			return
		}
		tr.SetOnText(func(text string) { _ = tr.WriteText(context.Background(), text) })
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			ar := tr.AudioReader()
			buf := make([]byte, c.FrameBytesPCM())
			for {
				if _, err := io.ReadFull(ar, buf); err != nil {
					return
				}
				if _, err := pw.Write(buf); err != nil {
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

func TestWebSocketLegOutboundLifecycle(t *testing.T) {
	wsURL, stop := wsEchoServer(t)
	defer stop()

	l := NewWebSocketOutboundPendingLeg(16000, true, wsLegTestLogger())
	if l.State() != StateRinging {
		t.Fatalf("want StateRinging, got %s", l.State())
	}
	if l.Type() != TypeWebSocketOutbound {
		t.Fatalf("type=%s", l.Type())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, peerHdr, err := wsmedia.DialClient(ctx, wsURL, wsmedia.Config{
		SampleRate: 16000, WireFormat: wsmedia.WireBinary, Log: wsLegTestLogger(),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hdr := map[string]string{}
	for k := range peerHdr {
		hdr[k] = peerHdr.Get(k)
	}
	l.AttachTransport(tr, hdr)

	if l.State() != StateConnected {
		t.Fatalf("want StateConnected after attach, got %s", l.State())
	}
	if l.AnsweredAt().IsZero() {
		t.Fatal("answeredAt should be set after attach")
	}

	// Round-trip audio: write a frame to AudioWriter, read it from AudioReader.
	frame := make([]byte, 640)
	for i := range frame {
		frame[i] = byte(i & 0xff)
	}
	go func() {
		_, _ = l.AudioWriter().Write(frame)
	}()

	got := make([]byte, 640)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(l.AudioReader(), got)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for audio echo")
	}
	if string(got) != string(frame) {
		t.Fatal("audio mismatch after round-trip")
	}

	// Text round-trip.
	rxText := make(chan string, 1)
	l.OnTextReceived(func(text string, _ bool) { rxText <- text })
	if err := l.SendText(ctx, "hi"); err != nil {
		t.Fatalf("send text: %v", err)
	}
	select {
	case got := <-rxText:
		if got != "hi" {
			t.Fatalf("text=%q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for text")
	}

	// ClaimDisconnect single-flight.
	if !l.ClaimDisconnect() {
		t.Fatal("first claim should succeed")
	}
	if l.ClaimDisconnect() {
		t.Fatal("second claim should fail")
	}

	if err := l.Hangup(ctx); err != nil {
		t.Fatalf("hangup: %v", err)
	}
	if l.State() != StateHungUp {
		t.Fatalf("state=%s", l.State())
	}
}

func TestWebSocketLegInboundConstructsConnected(t *testing.T) {
	// Build a server that returns the leg from upgrade and stash it for inspection.
	type result struct {
		leg *WebSocketLeg
		err error
	}
	resultCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c := wsmedia.Config{SampleRate: 16000, WireFormat: wsmedia.WireBinary, Log: wsLegTestLogger()}
		tr, reqHdr, err := wsmedia.UpgradeServer(w, r, c)
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		hdr := map[string]string{}
		for k := range reqHdr {
			if strings.HasPrefix(strings.ToLower(k), "x-") || strings.HasPrefix(strings.ToLower(k), "p-") {
				hdr[k] = reqHdr.Get(k)
			}
		}
		l := NewWebSocketInboundLeg(tr, hdr, 16000, true, wsLegTestLogger())
		resultCh <- result{leg: l}
		<-tr.Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialCfg := wsmedia.Config{
		SampleRate: 16000, WireFormat: wsmedia.WireBinary, Log: wsLegTestLogger(),
		Headers: http.Header{"X-Tenant": []string{"t1"}, "Other": []string{"y"}},
	}
	clientTr, _, err := wsmedia.DialClient(ctx, wsURL, dialCfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientTr.Close()
	pr, _ := io.Pipe()
	clientTr.Start(pr)

	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("server upgrade: %v", r.err)
		}
		if r.leg.State() != StateConnected {
			t.Fatalf("state=%s", r.leg.State())
		}
		if r.leg.Type() != TypeWebSocketInbound {
			t.Fatalf("type=%s", r.leg.Type())
		}
		got := r.leg.Headers()
		if got["X-Tenant"] != "t1" {
			t.Fatalf("missing X-Tenant: %#v", got)
		}
		if _, present := got["Other"]; present {
			t.Fatalf("non-X-/P- header leaked through: %#v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server never produced a leg")
	}
}

func TestWebSocketLegSendTextWithoutRTT(t *testing.T) {
	l := NewWebSocketOutboundPendingLeg(16000, false, wsLegTestLogger())
	if err := l.SendText(context.Background(), "x"); err != ErrRTTNotNegotiated {
		t.Fatalf("err=%v", err)
	}
}

func TestWebSocketLegSendDTMFNotSupported(t *testing.T) {
	l := NewWebSocketOutboundPendingLeg(16000, true, wsLegTestLogger())
	if err := l.SendDTMF(context.Background(), "1"); err == nil {
		t.Fatal("want error")
	}
}
