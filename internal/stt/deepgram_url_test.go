package stt

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
)

// A Deepgram-dialect endpoint that isn't Deepgram (an emulator, a proxy, a
// load-test mock) is only reachable if the base URL is honoured, so assert the
// dial actually lands there rather than that a field was stored.
func TestDeepgramSTT_DialsOverriddenBaseURL(t *testing.T) {
	gotURL := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotURL <- r.URL.String():
		default:
		}
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(io.Discard, conn)
	}))
	defer srv.Close()

	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/listen"

	pr, pw := io.Pipe()
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := NewDeepgram(slog.Default(), base)
	go tr.Start(ctx, pr, "token", Options{Language: "en"}, func(string, bool) {})

	select {
	case u := <-gotURL:
		if !strings.HasPrefix(u, "/v1/listen?") {
			t.Errorf("dialed path = %q, want /v1/listen with a query string", u)
		}
		// The query the platform sends is part of what a benchmark measures;
		// a mock has to be able to rely on it.
		for _, want := range []string{"encoding=linear16", "sample_rate=16000", "model=nova-3", "language=en"} {
			if !strings.Contains(u, want) {
				t.Errorf("dialed URL %q missing %q", u, want)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transcriber never dialled the overridden endpoint")
	}
}

func TestDeepgramSTT_EmptyBaseURLKeepsPublicEndpoint(t *testing.T) {
	if got := NewDeepgram(slog.Default(), "").baseURL; got != DefaultDeepgramWSURL {
		t.Errorf("baseURL = %q, want %q", got, DefaultDeepgramWSURL)
	}
}
