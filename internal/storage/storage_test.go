package storage

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// headBucketServer is a fake S3 endpoint answering HeadBucket with the given
// status, plus a count of the HEAD requests it actually received.
func headBucketServer(t *testing.T, status int) (*httptest.Server, *atomic.Int64) {
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
	return srv, &hits
}

func TestEndpointIsInsecure(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{"plaintext", "http://minio.internal:9000", true},
		{"plaintext mixed case", "HTTP://minio.internal:9000", true},
		{"tls", "https://s3.example.com", false},
		{"tls mixed case", "HTTPS://s3.example.com", false},
		{"empty means SDK default, always tls", "", false},
		// Cannot be classified by the guard; the SDK rejects it at the
		// preflight as an invalid URI.
		{"scheme-less", "minio.internal:9000", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := endpointIsInsecure(tt.endpoint); got != tt.want {
				t.Errorf("endpointIsInsecure(%q) = %v, want %v", tt.endpoint, got, tt.want)
			}
		})
	}
}

// The preflight is the whole point of the item: a bad bucket must surface at
// construction, not at the first upload after a call has been recorded.
func TestNewS3Backend_Preflight(t *testing.T) {
	tests := []struct {
		name string
		// status of the fake endpoint's HeadBucket reply.
		status int
		// closed shuts the endpoint down before construction, so the probe
		// cannot connect at all.
		closed            bool
		wantErr           bool
		wantBucketMissing bool
	}{
		{name: "bucket reachable", status: http.StatusOK},
		{name: "bucket missing", status: http.StatusNotFound, wantErr: true, wantBucketMissing: true},
		{name: "endpoint erroring", status: http.StatusInternalServerError, wantErr: true},
		{name: "endpoint unreachable", closed: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := headBucketServer(t, tt.status)
			if tt.closed {
				srv.Close()
			}

			backend, err := NewS3Backend(context.Background(), S3Config{
				Bucket:    "test-bucket",
				Region:    "us-east-1",
				Endpoint:  srv.URL,
				AccessKey: "key",
				SecretKey: "secret",
				// srv.URL is http://127.0.0.1:..., which the scheme guard would
				// otherwise reject before we reach the probe under test.
				AllowInsecure: true,
			})

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("expected construction to succeed, got %v", err)
				}
				if backend == nil {
					t.Fatal("expected a non-nil backend")
				}
				return
			}
			if err == nil {
				t.Fatal("expected construction to fail")
			}
			if got := errors.Is(err, ErrBucketMissing); got != tt.wantBucketMissing {
				t.Fatalf("errors.Is(err, ErrBucketMissing) = %v, want %v (err: %v)", got, tt.wantBucketMissing, err)
			}
			if !tt.wantBucketMissing && !strings.Contains(err.Error(), "preflight") {
				t.Errorf("error should identify the preflight as the failing stage, got %q", err.Error())
			}
		})
	}
}

func TestNewS3Backend_InsecureEndpoint(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// The probe rejects this server's self-signed cert; that is expected here
	// and the handshake errors are noise.
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer tlsSrv.Close()

	plainSrv, plainHits := headBucketServer(t, http.StatusOK)

	tests := []struct {
		name          string
		endpoint      string
		allowInsecure bool
		// wantRejected: rejected by the scheme guard with ErrInsecureEndpoint.
		wantRejected bool
		// wantOK: construction succeeds end-to-end.
		wantOK bool
	}{
		{name: "plaintext rejected by default", endpoint: plainSrv.URL, wantRejected: true},
		{name: "plaintext allowed when opted in", endpoint: plainSrv.URL, allowInsecure: true, wantOK: true},
		// Neither of these is classifiable as plaintext, so the guard must let
		// them through; each then fails the probe on its own merits, which is a
		// different error.
		{name: "tls not rejected", endpoint: tlsSrv.URL},
		{name: "scheme-less not rejected", endpoint: strings.TrimPrefix(plainSrv.URL, "http://")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := plainHits.Load()

			backend, err := NewS3Backend(context.Background(), S3Config{
				Bucket:        "test-bucket",
				Region:        "us-east-1",
				Endpoint:      tt.endpoint,
				AccessKey:     "key",
				SecretKey:     "secret",
				AllowInsecure: tt.allowInsecure,
			})

			if tt.wantRejected {
				if !errors.Is(err, ErrInsecureEndpoint) {
					t.Fatalf("expected ErrInsecureEndpoint, got %v", err)
				}
				// The guard must short-circuit: recording credentials and audio
				// must not touch a plaintext endpoint at all.
				if n := plainHits.Load() - before; n != 0 {
					t.Errorf("expected no request to the endpoint, got %d", n)
				}
				return
			}
			if errors.Is(err, ErrInsecureEndpoint) {
				t.Fatalf("endpoint %q must not be rejected by the scheme guard, got %v", tt.endpoint, err)
			}
			if tt.wantOK {
				if err != nil {
					t.Fatalf("expected construction to succeed, got %v", err)
				}
				if backend == nil {
					t.Fatal("expected a non-nil backend")
				}
			}
		})
	}
}

func TestFileBackend_Upload(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "test.wav")
	if err := os.WriteFile(tmp, []byte("wav-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	fb := FileBackend{}
	loc, err := fb.Upload(context.Background(), tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc != tmp {
		t.Errorf("expected %q, got %q", tmp, loc)
	}
	// File should still exist.
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("file should still exist: %v", err)
	}
}

func TestS3Backend_Upload(t *testing.T) {
	var (
		gotKey         string
		gotContentType string
		gotBody        []byte
	)

	// Fake S3 server that accepts PutObject.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			gotKey = r.URL.Path
			gotContentType = r.Header.Get("Content-Type")
			var err error
			gotBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("key", "secret", ""),
	})

	backend := NewS3BackendWithClient(client, "test-bucket", "recordings/")

	// Create a temp file.
	tmp := filepath.Join(t.TempDir(), "20260301_110500_abcd1234.wav")
	if err := os.WriteFile(tmp, []byte("wav-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	loc, err := backend.Upload(context.Background(), tmp)
	if err != nil {
		t.Fatalf("upload error: %v", err)
	}

	expectedLoc := "s3://test-bucket/recordings/20260301_110500_abcd1234.wav"
	if loc != expectedLoc {
		t.Errorf("location = %q, want %q", loc, expectedLoc)
	}

	if !strings.HasSuffix(gotKey, "/recordings/20260301_110500_abcd1234.wav") {
		t.Errorf("S3 key = %q, expected suffix /recordings/20260301_110500_abcd1234.wav", gotKey)
	}

	if gotContentType != "audio/wav" {
		t.Errorf("content-type = %q, want audio/wav", gotContentType)
	}

	if string(gotBody) != "wav-data" {
		t.Errorf("body = %q, want wav-data", string(gotBody))
	}

	// Local file should be deleted.
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("local file should have been deleted after upload")
	}
}

func TestS3Backend_Upload_Error(t *testing.T) {
	// Fake server that always returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("key", "secret", ""),
	})

	backend := NewS3BackendWithClient(client, "bucket", "")

	tmp := filepath.Join(t.TempDir(), "test.wav")
	if err := os.WriteFile(tmp, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := backend.Upload(context.Background(), tmp)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}

	// Local file should still exist when upload fails.
	if _, err := os.Stat(tmp); err != nil {
		t.Error("local file should still exist after failed upload")
	}
}
