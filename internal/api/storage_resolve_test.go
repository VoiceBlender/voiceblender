package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/storage"
)

// The per-request S3 backend must inherit the operator's insecure-endpoint
// decision from server config — a caller must not be able to downgrade the
// transport, and an operator who has opted in must not be blocked.
//
// Scope: this covers the per-request call site (resolveStorage) only. The boot
// call site in cmd/voiceblender/main.go builds the same storage.S3Config
// named-field literal, so dropping AllowInsecure there still compiles and the
// suite stays green — that path has no test seam and is not covered here.
func TestResolveStorage_AllowInsecure(t *testing.T) {
	// httptest serves http://127.0.0.1:..., i.e. a genuinely plaintext
	// endpoint, which is exactly the condition under test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/test-bucket" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	req := RecordRequest{
		Storage:     "s3",
		S3Bucket:    "test-bucket",
		S3Endpoint:  srv.URL,
		S3AccessKey: "key",
		S3SecretKey: "secret",
	}

	t.Run("operator opted in", func(t *testing.T) {
		s := &Server{Config: config.Config{S3AllowInsecureEndpoint: true}}

		backend, err := s.resolveStorage(context.Background(), req)
		if errors.Is(err, storage.ErrInsecureEndpoint) {
			t.Fatalf("S3_ALLOW_INSECURE_ENDPOINT=true must reach the endpoint, got %v", err)
		}
		if err != nil {
			t.Fatalf("expected the backend to be created, got %v", err)
		}
		if backend == nil {
			t.Fatal("expected a non-nil backend")
		}
	})

	t.Run("insecure endpoint refused by default", func(t *testing.T) {
		s := &Server{Config: config.Config{S3AllowInsecureEndpoint: false}}

		_, err := s.resolveStorage(context.Background(), req)
		if !errors.Is(err, storage.ErrInsecureEndpoint) {
			t.Fatalf("expected ErrInsecureEndpoint (surfaced to the caller as 400), got %v", err)
		}
	})
}

// The bucket preflight must honour the caller's deadline. Against an endpoint
// that accepts the connection and never answers, the probe would otherwise run
// for the full preflight budget with nothing able to cut it short — an HTTP
// client that has already hung up would still hold the request open.
func TestResolveStorage_ContextCancelled(t *testing.T) {
	// Accept connections and never reply: the SDK gets a live TCP socket and
	// then waits for response headers that never come. A closed/refused port
	// would fail instantly and prove nothing about the deadline.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})

	s := &Server{Config: config.Config{S3AllowInsecureEndpoint: true}}
	req := RecordRequest{
		Storage:     "s3",
		S3Bucket:    "b",
		S3Endpoint:  "http://" + ln.Addr().String(),
		S3AccessKey: "k",
		S3SecretKey: "s",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = s.resolveStorage(ctx, req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error once the caller's deadline expires, got nil")
	}
	// Generous vs. the 100ms deadline, but far below the preflight budget the
	// call falls back to when the caller's ctx is not threaded through.
	if elapsed > 2*time.Second {
		t.Fatalf("resolveStorage took %v: the caller's deadline was ignored and the "+
			"probe ran on its own budget instead", elapsed)
	}
}
