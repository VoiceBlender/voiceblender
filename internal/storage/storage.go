package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var (
	// ErrBucketMissing reports that the bucket definitively does not exist.
	ErrBucketMissing = errors.New("s3 bucket does not exist")
	// ErrInsecureEndpoint reports that the configured endpoint would ship
	// recording audio in cleartext to a non-local host.
	ErrInsecureEndpoint = errors.New("s3 endpoint is plaintext http")
	// ErrPreflightInconclusive reports that the bucket probe could neither
	// confirm nor refute the bucket. The backend stays usable.
	ErrPreflightInconclusive = errors.New("s3 bucket preflight inconclusive")
)

// endpointIsInsecure reports whether endpoint would send recordings in
// cleartext to a host outside the local network. An empty endpoint means the
// SDK default (always TLS). Loopback and private destinations are exempt: a
// co-located MinIO is the common plaintext deployment and its traffic never
// crosses an untrusted network.
func endpointIsInsecure(endpoint string) bool {
	if !strings.HasPrefix(strings.ToLower(endpoint), "http://") {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return true
	}
	return !hostIsLocal(u.Hostname())
}

func hostIsLocal(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	// A single-label name resolves only inside the local network — a container
	// or service name, never a public destination.
	if !strings.Contains(host, ".") {
		return host != ""
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".localdomain", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// Backend abstracts where a recording file is stored after capture.
type Backend interface {
	Upload(ctx context.Context, localPath string) (location string, err error)
}

// FileBackend is a no-op backend that returns the local path as-is.
type FileBackend struct{}

func (FileBackend) Upload(_ context.Context, localPath string) (string, error) {
	return localPath, nil
}

// S3Config holds configuration for the S3 storage backend.
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	Prefix    string
	AccessKey string // optional; if set, used instead of default credential chain
	SecretKey string // optional; must be set together with AccessKey
	// AllowInsecure permits a plaintext http:// endpoint on a non-local host.
	AllowInsecure bool
}

// S3Backend uploads recordings to an S3-compatible store.
type S3Backend struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Backend creates an S3Backend from the given config.
// If AccessKey/SecretKey are set, they are used directly; otherwise
// credentials are resolved via the standard AWS SDK chain.
//
// This makes no request to the store; call Preflight for that.
func NewS3Backend(ctx context.Context, cfg S3Config) (*S3Backend, error) {
	// Checked before the credential chain runs, so a broken profile cannot
	// preempt the transport decision with a config error.
	if endpointIsInsecure(cfg.Endpoint) && !cfg.AllowInsecure {
		return nil, fmt.Errorf("%w: %q (set S3_ALLOW_INSECURE_ENDPOINT=true to override)", ErrInsecureEndpoint, cfg.Endpoint)
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		loadOpts = append(loadOpts,
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
			),
		)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	s3Opts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	return &S3Backend{
		client: client,
		bucket: cfg.Bucket,
		prefix: cfg.Prefix,
	}, nil
}

// Preflight probes the bucket so a misconfigured store surfaces before a call
// is recorded rather than at the upload that follows it. Bounded by ctx.
//
// Only a definitive negative is reported as ErrBucketMissing. Every other
// failure — no s3:ListBucket permission (403), a 5xx, an unreachable endpoint,
// an expired ctx — returns ErrPreflightInconclusive, because uploads may still
// succeed and refusing the backend would turn a failed probe into a recording
// outage.
func (b *S3Backend) Preflight(ctx context.Context) error {
	if _, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(b.bucket)}); err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return fmt.Errorf("%w: %q", ErrBucketMissing, b.bucket)
		}
		return fmt.Errorf("%w for bucket %q: %w", ErrPreflightInconclusive, b.bucket, err)
	}
	return nil
}

// NewS3BackendWithClient creates an S3Backend using a pre-configured S3 client.
// Useful for testing.
func NewS3BackendWithClient(client *s3.Client, bucket, prefix string) *S3Backend {
	return &S3Backend{
		client: client,
		bucket: bucket,
		prefix: prefix,
	}
}

// NewS3BackendForTest creates an S3Backend pointing at a test endpoint with
// static credentials. Useful for integration tests with MinIO.
func NewS3BackendForTest(ctx context.Context, endpoint, bucket, region, accessKey, secretKey string) (*S3Backend, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return &S3Backend{
		client: client,
		bucket: bucket,
		prefix: "",
	}, nil
}

func (b *S3Backend) Upload(ctx context.Context, localPath string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open recording file: %w", err)
	}
	defer f.Close()

	key := b.prefix + filepath.Base(localPath)

	contentType := "audio/wav"
	_, err = b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(b.bucket),
		Key:         aws.String(key),
		Body:        f,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("s3 upload: %w", err)
	}

	// Remove local file after successful upload.
	os.Remove(localPath)

	return fmt.Sprintf("s3://%s/%s", b.bucket, key), nil
}
