package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

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
