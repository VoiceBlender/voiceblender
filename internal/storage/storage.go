package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

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
func NewS3Backend(ctx context.Context, cfg S3Config) (*S3Backend, error) {
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
