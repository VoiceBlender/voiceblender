package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// GCSConfig holds configuration for the Google Cloud Storage backend.
type GCSConfig struct {
	Bucket string
	// Prefix is an optional object-name prefix (e.g. "dev" or "recordings/").
	// A trailing slash is added automatically when missing so callers can pass
	// bare workspace ids matching the GCS_OBJECT_NAME_PREFIX convention used
	// by other Synthflow services.
	Prefix string
	// ClientOptions are forwarded to storage.NewClient. Empty means Application
	// Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, gcloud ADC, or the
	// GCE/GKE metadata server) — the same chain Google Cloud TTS already uses.
	ClientOptions []option.ClientOption
}

// gcsUploader is the subset of GCS we need so Upload can be unit-tested
// without a live project or the JSON API wire format.
type gcsUploader interface {
	Upload(ctx context.Context, object string, r io.Reader, contentType string) error
	Close() error
}

// GCSBackend uploads recordings to Google Cloud Storage via the native GCS API.
// Prefer this over S3_ENDPOINT=https://storage.googleapis.com when running on
// GKE: Workload Identity / ADC works directly, with no HMAC interop keys.
type GCSBackend struct {
	uploader gcsUploader
	bucket   string
	prefix   string
}

type liveGCSUploader struct {
	client *gcs.Client
	bucket string
}

func (u *liveGCSUploader) Upload(ctx context.Context, object string, r io.Reader, contentType string) error {
	w := u.client.Bucket(u.bucket).Object(object).NewWriter(ctx)
	w.ContentType = contentType
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return fmt.Errorf("write object: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close writer: %w", err)
	}
	return nil
}

func (u *liveGCSUploader) Close() error {
	return u.client.Close()
}

// joinObjectKey builds an object key from an optional prefix and a basename.
// A non-empty prefix without a trailing slash gets one, matching how other
// Synthflow services treat GCS_OBJECT_NAME_PREFIX (bare workspace id).
func joinObjectKey(prefix, base string) string {
	if prefix == "" {
		return base
	}
	if strings.HasSuffix(prefix, "/") {
		return prefix + base
	}
	return prefix + "/" + base
}

// NewGCSBackend creates a GCSBackend. Credentials are resolved via Google's
// Application Default Credentials unless ClientOptions override that.
func NewGCSBackend(ctx context.Context, cfg GCSConfig) (*GCSBackend, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("gcs bucket is required")
	}
	client, err := gcs.NewClient(ctx, cfg.ClientOptions...)
	if err != nil {
		return nil, fmt.Errorf("create GCS client: %w", err)
	}
	return &GCSBackend{
		uploader: &liveGCSUploader{client: client, bucket: cfg.Bucket},
		bucket:   cfg.Bucket,
		prefix:   cfg.Prefix,
	}, nil
}

// NewGCSBackendWithUploader creates a GCSBackend around a custom uploader.
// Useful for tests.
func NewGCSBackendWithUploader(uploader gcsUploader, bucket, prefix string) *GCSBackend {
	return &GCSBackend{
		uploader: uploader,
		bucket:   bucket,
		prefix:   prefix,
	}
}

// Close releases the underlying GCS client. Safe to call on a nil receiver or
// a backend built with a no-op test uploader.
func (b *GCSBackend) Close() error {
	if b == nil || b.uploader == nil {
		return nil
	}
	return b.uploader.Close()
}

func (b *GCSBackend) Upload(ctx context.Context, localPath string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open recording file: %w", err)
	}
	defer f.Close()

	key := joinObjectKey(b.prefix, filepath.Base(localPath))
	if err := b.uploader.Upload(ctx, key, f, "audio/wav"); err != nil {
		return "", fmt.Errorf("gcs upload: %w", err)
	}

	// Match S3Backend: drop the local file only after a successful upload.
	os.Remove(localPath)

	return fmt.Sprintf("gs://%s/%s", b.bucket, key), nil
}
