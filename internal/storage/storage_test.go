package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func newBackend(t *testing.T, cfg S3Config) *S3Backend {
	t.Helper()
	if cfg.Bucket == "" {
		cfg.Bucket = "test-bucket"
	}
	cfg.Region, cfg.AccessKey, cfg.SecretKey = "us-east-1", "key", "secret"
	b, err := NewS3Backend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	return b
}

func TestEndpointIsInsecure(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{"plaintext public host", "http://s3.example.com", true},
		{"plaintext public host mixed case", "HTTP://s3.example.com", true},
		{"plaintext public ip", "http://93.184.216.34:9000", true},
		{"tls", "https://s3.example.com", false},
		{"tls mixed case", "HTTPS://s3.example.com", false},
		{"empty means SDK default, always tls", "", false},
		// Not classifiable as a scheme; the SDK rejects it as an invalid URI.
		{"scheme-less", "minio.internal:9000", false},
		// Plaintext but never leaves the local network — the common MinIO
		// deployments, which must keep working without the opt-in flag.
		{"loopback", "http://127.0.0.1:9000", false},
		{"loopback name", "http://localhost:9000", false},
		{"ipv6 loopback", "http://[::1]:9000", false},
		{"rfc1918", "http://10.1.2.3:9000", false},
		{"rfc1918 172.16/12", "http://172.16.0.9:9000", false},
		{"link-local", "http://169.254.10.5:9000", false},
		{"ipv6 unique local", "http://[fd00::1]:9000", false},
		{"container service name", "http://minio:9000", false},
		{"internal suffix", "http://minio.internal:9000", false},
		{"local suffix", "http://minio.local:9000", false},
		// A public IP inside a private-looking name must not be exempted by
		// the suffix rules.
		{"public subdomain", "http://minio.example.com:9000", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := endpointIsInsecure(tt.endpoint); got != tt.want {
				t.Errorf("endpointIsInsecure(%q) = %v, want %v", tt.endpoint, got, tt.want)
			}
		})
	}
}

// A bucket the store denies must surface before a call is recorded, but a probe
// that cannot get an answer must not take the backend down with it.
func TestS3Backend_Preflight(t *testing.T) {
	tests := []struct {
		name string
		// status of the fake endpoint's HeadBucket reply.
		status int
		// closed shuts the endpoint down first, so the probe cannot connect.
		closed bool
		// budget mirrors the deadline a caller imposes; the SDK retries a 5xx
		// or a refused connection, so without one these cases spend seconds in
		// backoff that production would have cut short.
		budget           time.Duration
		wantMissing      bool
		wantInconclusive bool
	}{
		{name: "bucket reachable", status: http.StatusOK},
		{name: "bucket missing", status: http.StatusNotFound, wantMissing: true},
		// 403 is what a least-privilege uploader with s3:PutObject but no
		// s3:ListBucket gets. It says nothing about the bucket existing, and
		// must not be treated as fatal.
		{name: "no ListBucket permission", status: http.StatusForbidden, wantInconclusive: true},
		{name: "endpoint erroring", status: http.StatusInternalServerError, budget: time.Second, wantInconclusive: true},
		{name: "endpoint unreachable", closed: true, budget: time.Second, wantInconclusive: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := headBucketServer(t, tt.status)
			if tt.closed {
				srv.Close()
			}

			ctx := context.Background()
			if tt.budget > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.budget)
				defer cancel()
			}

			err := newBackend(t, S3Config{Endpoint: srv.URL}).Preflight(ctx)

			if got := errors.Is(err, ErrBucketMissing); got != tt.wantMissing {
				t.Fatalf("errors.Is(err, ErrBucketMissing) = %v, want %v (err: %v)", got, tt.wantMissing, err)
			}
			if got := errors.Is(err, ErrPreflightInconclusive); got != tt.wantInconclusive {
				t.Fatalf("errors.Is(err, ErrPreflightInconclusive) = %v, want %v (err: %v)", got, tt.wantInconclusive, err)
			}
			if !tt.wantMissing && !tt.wantInconclusive && err != nil {
				t.Fatalf("expected a clean probe, got %v", err)
			}
		})
	}
}

func TestS3Backend_PreflightHonoursContext(t *testing.T) {
	srv, hits := headBucketServer(t, http.StatusOK)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := newBackend(t, S3Config{Endpoint: srv.URL}).Preflight(ctx)
	if !errors.Is(err, ErrPreflightInconclusive) {
		t.Fatalf("a cancelled probe is inconclusive, not fatal, got %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("expected no request once ctx is dead, got %d", n)
	}
}

func TestNewS3Backend_InsecureEndpoint(t *testing.T) {
	plainSrv, plainHits := headBucketServer(t, http.StatusOK)

	tests := []struct {
		name          string
		endpoint      string
		allowInsecure bool
		wantRejected  bool
	}{
		{name: "public plaintext rejected by default", endpoint: "http://s3.example.com", wantRejected: true},
		{name: "public plaintext allowed when opted in", endpoint: "http://s3.example.com", allowInsecure: true},
		// The backwards-compatibility case: a co-located MinIO keeps working
		// without the operator setting anything.
		{name: "loopback plaintext allowed by default", endpoint: plainSrv.URL},
		{name: "tls allowed", endpoint: "https://s3.example.com"},
		{name: "scheme-less allowed", endpoint: strings.TrimPrefix(plainSrv.URL, "http://")},
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
				return
			}
			if err != nil {
				t.Fatalf("expected construction to succeed, got %v", err)
			}
			if backend == nil {
				t.Fatal("expected a non-nil backend")
			}
			// Construction alone must stay offline — the probe is a separate,
			// caller-budgeted step.
			if n := plainHits.Load() - before; n != 0 {
				t.Errorf("NewS3Backend made %d request(s); it must not touch the network", n)
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

type fakeGCSUploader struct {
	object      string
	body        []byte
	contentType string
	err         error
	closed      bool
}

func (f *fakeGCSUploader) Upload(_ context.Context, object string, r io.Reader, contentType string) error {
	if f.err != nil {
		return f.err
	}
	f.object = object
	f.contentType = contentType
	var err error
	f.body, err = io.ReadAll(r)
	return err
}

func (f *fakeGCSUploader) Close() error {
	f.closed = true
	return nil
}

func TestGCSBackend_Upload(t *testing.T) {
	fake := &fakeGCSUploader{}
	backend := NewGCSBackendWithUploader(fake, "rec-bucket", "voiceblender/")

	tmp := filepath.Join(t.TempDir(), "20260301_110500_abcd1234.wav")
	if err := os.WriteFile(tmp, []byte("wav-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	loc, err := backend.Upload(context.Background(), tmp)
	if err != nil {
		t.Fatalf("upload error: %v", err)
	}

	expectedLoc := "gs://rec-bucket/voiceblender/20260301_110500_abcd1234.wav"
	if loc != expectedLoc {
		t.Errorf("location = %q, want %q", loc, expectedLoc)
	}
	if fake.object != "voiceblender/20260301_110500_abcd1234.wav" {
		t.Errorf("object = %q, want voiceblender/20260301_110500_abcd1234.wav", fake.object)
	}
	if fake.contentType != "audio/wav" {
		t.Errorf("content-type = %q, want audio/wav", fake.contentType)
	}
	if string(fake.body) != "wav-data" {
		t.Errorf("body = %q, want wav-data", fake.body)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("local file should have been deleted after upload")
	}
}

func TestGCSBackend_Upload_AddsTrailingSlash(t *testing.T) {
	fake := &fakeGCSUploader{}
	// Bare workspace id — same shape as GCS_OBJECT_NAME_PREFIX=dev.
	backend := NewGCSBackendWithUploader(fake, "rec-bucket", "dev")

	tmp := filepath.Join(t.TempDir(), "call.wav")
	if err := os.WriteFile(tmp, []byte("wav-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	loc, err := backend.Upload(context.Background(), tmp)
	if err != nil {
		t.Fatalf("upload error: %v", err)
	}
	if want := "gs://rec-bucket/dev/call.wav"; loc != want {
		t.Errorf("location = %q, want %q", loc, want)
	}
	if fake.object != "dev/call.wav" {
		t.Errorf("object = %q, want dev/call.wav", fake.object)
	}
}

func TestJoinObjectKey(t *testing.T) {
	tests := []struct {
		prefix, base, want string
	}{
		{"", "a.wav", "a.wav"},
		{"dev", "a.wav", "dev/a.wav"},
		{"dev/", "a.wav", "dev/a.wav"},
		{"recordings/inbound", "a.wav", "recordings/inbound/a.wav"},
		{"recordings/inbound/", "a.wav", "recordings/inbound/a.wav"},
	}
	for _, tt := range tests {
		if got := joinObjectKey(tt.prefix, tt.base); got != tt.want {
			t.Errorf("joinObjectKey(%q, %q) = %q, want %q", tt.prefix, tt.base, got, tt.want)
		}
	}
}

func TestGCSBackend_Upload_Error(t *testing.T) {
	fake := &fakeGCSUploader{err: fmt.Errorf("boom")}
	backend := NewGCSBackendWithUploader(fake, "bucket", "")

	tmp := filepath.Join(t.TempDir(), "test.wav")
	if err := os.WriteFile(tmp, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := backend.Upload(context.Background(), tmp); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Error("local file should still exist after failed upload")
	}
}

func TestNewGCSBackend_RequiresBucket(t *testing.T) {
	if _, err := NewGCSBackend(context.Background(), GCSConfig{}); err == nil {
		t.Fatal("expected error for empty bucket")
	}
}
