package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/storage"
)

// headBucketEndpoint is a fake S3 endpoint answering HeadBucket with the given
// status, plus a count of the HEAD requests it received.
func headBucketEndpoint(t *testing.T, status int) (string, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			hits.Add(1)
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

func s3Request(endpoint string) RecordRequest {
	return RecordRequest{
		Storage:     "s3",
		S3Bucket:    "test-bucket",
		S3Endpoint:  endpoint,
		S3AccessKey: "key",
		S3SecretKey: "secret",
	}
}

// The per-request backend must inherit the operator's insecure-endpoint
// decision from server config — a caller must not be able to downgrade the
// transport, and an operator who has opted in must not be blocked. A loopback
// endpoint is not a downgrade and needs no opt-in.
func TestResolveStorage_InsecureEndpoint(t *testing.T) {
	loopback, _ := headBucketEndpoint(t, http.StatusOK)

	tests := []struct {
		name          string
		endpoint      string
		allowInsecure bool
		wantRejected  bool
	}{
		{name: "public plaintext refused by default", endpoint: "http://s3.example.com", wantRejected: true},
		{name: "public plaintext allowed when operator opted in", endpoint: "http://s3.example.com", allowInsecure: true},
		{name: "loopback needs no opt-in", endpoint: loopback},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			s.Config.S3AllowInsecureEndpoint = tt.allowInsecure
			// A public endpoint that is allowed through is never actually
			// reachable from a test, so leave the probe out of the picture.
			if !tt.wantRejected && tt.endpoint != loopback {
				s.Config.S3RequestPreflightTimeout = 0
			} else {
				s.Config.S3RequestPreflightTimeout = 2 * time.Second
			}

			backend, err := s.resolveStorage(context.Background(), s3Request(tt.endpoint))

			if tt.wantRejected {
				if !errors.Is(err, storage.ErrInsecureEndpoint) {
					t.Fatalf("expected ErrInsecureEndpoint (surfaced as 400), got %v", err)
				}
				return
			}
			if errors.Is(err, storage.ErrInsecureEndpoint) {
				t.Fatalf("endpoint %q must not be rejected, got %v", tt.endpoint, err)
			}
			if err != nil {
				t.Fatalf("expected a backend, got %v", err)
			}
			if backend == nil {
				t.Fatal("expected a non-nil backend")
			}
		})
	}
}

// A bucket the store says is absent must fail the request. Anything the probe
// cannot answer must not: the upload may well succeed, and a probe that cannot
// get a verdict is no reason to refuse to record.
func TestResolveStorage_PreflightPolicy(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "bucket missing fails the request", status: http.StatusNotFound, wantErr: true},
		{name: "no ListBucket permission still records", status: http.StatusForbidden},
		{name: "bucket reachable", status: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint, hits := headBucketEndpoint(t, tt.status)
			s := newTestServer(t)
			s.Config.S3RequestPreflightTimeout = 2 * time.Second

			backend, err := s.resolveStorage(context.Background(), s3Request(endpoint))

			if hits.Load() == 0 {
				t.Fatal("the bucket was never probed")
			}
			if tt.wantErr {
				if !errors.Is(err, storage.ErrBucketMissing) {
					t.Fatalf("expected ErrBucketMissing, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the recording to proceed, got %v", err)
			}
			if backend == nil {
				t.Fatal("expected a non-nil backend")
			}
		})
	}
}

func TestResolveStorage_PreflightDisabled(t *testing.T) {
	endpoint, hits := headBucketEndpoint(t, http.StatusNotFound)
	s := newTestServer(t)
	s.Config.S3RequestPreflightTimeout = 0

	if _, err := s.resolveStorage(context.Background(), s3Request(endpoint)); err != nil {
		t.Fatalf("with the probe disabled the backend must be returned as-is, got %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("expected no probe, got %d request(s)", n)
	}
}

// blackHoleEndpoint returns the address of a listener that accepts connections
// and then never replies. A refused connection would fail instantly and prove
// nothing about the deadline, so the probe must be left to hang.
func blackHoleEndpoint(t *testing.T) string {
	t.Helper()

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

	return "http://" + ln.Addr().String()
}

// An endpoint that accepts the connection and never answers must not hold the
// request open, and must not fail it either.
func TestResolveStorage_UnresponsiveEndpoint(t *testing.T) {
	s := newTestServer(t)
	s.Config.S3RequestPreflightTimeout = 200 * time.Millisecond

	start := time.Now()
	backend, err := s.resolveStorage(context.Background(), s3Request(blackHoleEndpoint(t)))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("an unanswered probe must not fail the request, got %v", err)
	}
	if backend == nil {
		t.Fatal("expected a non-nil backend")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("resolveStorage took %v: the probe budget was not applied", elapsed)
	}
}

// The caller's own deadline must cut the probe short even when the configured
// budget is longer, so an HTTP client that hung up — or a closed VSI
// connection — stops the work it asked for.
func TestResolveStorage_CallerDeadlineWins(t *testing.T) {
	s := newTestServer(t)
	s.Config.S3RequestPreflightTimeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := s.resolveStorage(ctx, s3Request(blackHoleEndpoint(t))); err != nil {
		t.Fatalf("an aborted probe must not fail the request, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("resolveStorage took %v: the caller's deadline was ignored", elapsed)
	}
}
