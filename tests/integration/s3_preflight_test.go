//go:build integration

package integration

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// fakeS3 is an S3-compatible endpoint that answers HeadBucket with headStatus
// and accepts every PutObject, counting both.
type fakeS3 struct {
	url     string
	heads   *atomic.Int64
	uploads *atomic.Int64
}

func newFakeS3(t *testing.T, headStatus int) *fakeS3 {
	t.Helper()
	var heads, uploads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			heads.Add(1)
			w.WriteHeader(headStatus)
		case http.MethodPut:
			uploads.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	// httptest listens on 127.0.0.1, which the insecure-endpoint guard exempts,
	// so no S3_ALLOW_INSECURE_ENDPOINT opt-in is needed here.
	return &fakeS3{url: srv.URL, heads: &heads, uploads: &uploads}
}

func s3RecordBody(endpoint string) map[string]any {
	return map[string]any{
		"storage":       "s3",
		"s3_bucket":     "recordings",
		"s3_endpoint":   endpoint,
		"s3_region":     "us-east-1",
		"s3_access_key": "key",
		"s3_secret_key": "secret",
	}
}

func s3TestInstance(t *testing.T, name string) *testInstance {
	return newTestInstanceWithOpts(t, name, func(cfg *config.Config) {
		cfg.S3RequestPreflightTimeout = 2 * time.Second
	})
}

// A bucket the store reports as absent must fail record-start, so the caller
// learns about it before the call is recorded rather than from a log line after
// the audio was already captured.
func TestS3Preflight_MissingBucketRejectsRecordStart(t *testing.T) {
	s3 := newFakeS3(t, http.StatusNotFound)

	instA := newTestInstanceWithOpts(t, "instance-a", func(cfg *config.Config) {
		cfg.S3RequestPreflightTimeout = 2 * time.Second
	})
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), s3RecordBody(s3.url))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("record start: status %d, want 400 for a bucket that does not exist", resp.StatusCode)
	}
	resp.Body.Close()

	if n := s3.heads.Load(); n == 0 {
		t.Fatal("the bucket was never probed")
	}
	if n := s3.uploads.Load(); n != 0 {
		t.Errorf("expected no upload attempt, got %d", n)
	}
}

// The least-privilege case: credentials with s3:PutObject but no s3:ListBucket
// get a 403 from HeadBucket. That says nothing about the bucket, so recording
// must proceed and the upload must still happen.
func TestS3Preflight_NoListBucketPermissionStillRecords(t *testing.T) {
	for _, tc := range []struct {
		name       string
		headStatus int
	}{
		{name: "no ListBucket permission", headStatus: http.StatusForbidden},
		{name: "endpoint erroring", headStatus: http.StatusInternalServerError},
		{name: "bucket reachable", headStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s3 := newFakeS3(t, tc.headStatus)

			instA := s3TestInstance(t, "instance-a")
			instB := newTestInstance(t, "instance-b")
			outboundID, _ := establishCall(t, instA, instB)

			recURL := fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID)
			resp := httpPost(t, recURL, s3RecordBody(s3.url))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("record start: status %d, want 200 — an inconclusive probe must not fail the request", resp.StatusCode)
			}
			var started recordingResponse
			decodeJSON(t, resp, &started)
			if started.Status != "recording" {
				t.Fatalf("status = %q, want recording", started.Status)
			}

			time.Sleep(300 * time.Millisecond)

			stop := httpDelete(t, recURL)
			if stop.StatusCode != http.StatusOK {
				t.Fatalf("record stop: status %d", stop.StatusCode)
			}
			stop.Body.Close()

			// The finished event carries the upload location, so it proves the
			// object actually went to S3 rather than staying on disk.
			e := instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
				return e.Data.GetLegID() == outboundID
			}, 5*time.Second)
			d, ok := e.Data.(*events.RecordingFinishedData)
			if !ok {
				t.Fatalf("unexpected event data type %T", e.Data)
			}
			if !strings.HasPrefix(d.File, "s3://recordings/") {
				t.Errorf("recording.finished file = %q, want an s3://recordings/ location", d.File)
			}
			if n := s3.uploads.Load(); n != 1 {
				t.Errorf("expected exactly 1 upload, got %d", n)
			}
		})
	}
}

// A plaintext endpoint on a non-local host is refused without the operator
// opt-in, and accepted with it. No request is made either way.
func TestS3Preflight_InsecureEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allow      bool
		wantStatus int
	}{
		{name: "refused by default", wantStatus: http.StatusBadRequest},
		{name: "allowed when operator opted in", allow: true, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instA := newTestInstanceWithOpts(t, "instance-a", func(cfg *config.Config) {
				cfg.S3AllowInsecureEndpoint = tc.allow
				// The host is not reachable, so leave the probe out of it: the
				// scheme guard is what is under test.
				cfg.S3RequestPreflightTimeout = 0
			})
			instB := newTestInstance(t, "instance-b")
			outboundID, _ := establishCall(t, instA, instB)

			resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID),
				s3RecordBody("http://s3.example.com"))
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("record start: status %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

// blackHoleS3 returns the address of a listener that accepts connections and
// then never replies. A refused connection would fail instantly and prove
// nothing about the budget, so the probe must be left to hang.
func blackHoleS3(t *testing.T) string {
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

// An endpoint that accepts the connection and never answers must not hold
// record-start open for longer than the configured probe budget.
func TestS3Preflight_UnresponsiveEndpointDoesNotStallRecordStart(t *testing.T) {
	endpoint := blackHoleS3(t)

	instA := newTestInstanceWithOpts(t, "instance-a", func(cfg *config.Config) {
		cfg.S3RequestPreflightTimeout = 300 * time.Millisecond
	})
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	start := time.Now()
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), s3RecordBody(endpoint))
	elapsed := time.Since(start)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("record start: status %d, want 200 — an unanswered probe must not fail the request", resp.StatusCode)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("record start took %v: the probe budget was not applied", elapsed)
	}
}
