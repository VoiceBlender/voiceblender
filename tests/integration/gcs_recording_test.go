//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/storage"
	"google.golang.org/api/option"
)

// fakeGCS is a Google Cloud Storage JSON-API endpoint that accepts every object
// upload, recording the object names it was given.
type fakeGCS struct {
	url     string
	uploads *atomic.Int64

	mu      sync.Mutex
	objects []string
}

func newFakeGCS(t *testing.T) *fakeGCS {
	t.Helper()
	f := &fakeGCS{uploads: &atomic.Int64{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/upload/storage/v1/b/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.Copy(io.Discard, r.Body)
		name := r.URL.Query().Get("name")
		f.mu.Lock()
		f.objects = append(f.objects, name)
		f.mu.Unlock()
		f.uploads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":%q,"bucket":"recordings"}`, name)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeGCS) objectNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.objects...)
}

// gcsBackend builds a server-level GCS backend aimed at the fake endpoint. The
// explicit client options stand in for Application Default Credentials, which a
// test has no way to obtain.
func gcsBackend(t *testing.T, f *fakeGCS, bucket, prefix string) storage.Backend {
	t.Helper()
	b, err := storage.NewGCSBackend(t.Context(), storage.GCSConfig{
		Bucket: bucket,
		Prefix: prefix,
		ClientOptions: []option.ClientOption{
			option.WithEndpoint(f.url),
			option.WithoutAuthentication(),
		},
	})
	if err != nil {
		t.Fatalf("create GCS backend: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// A leg recording with storage=gcs must reach the bucket, and the finished event
// must report the gs:// location rather than the local staging path — that URI
// is the only handle a caller gets on the uploaded object.
func TestGCSRecording_LegUploadsAndReportsGSURI(t *testing.T) {
	gcs := newFakeGCS(t)

	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	instA.apiSrv.GCS = gcsBackend(t, gcs, "recordings", "dev")

	outboundID, _ := establishCall(t, instA, instB)

	recURL := fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID)
	resp := httpPost(t, recURL, map[string]any{"storage": "gcs"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("record start: status %d, want 200", resp.StatusCode)
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
	var stopped recordingResponse
	decodeJSON(t, stop, &stopped)
	if !strings.HasPrefix(stopped.File, "gs://recordings/dev/") {
		t.Errorf("stop response file = %q, want a gs://recordings/dev/ location", stopped.File)
	}

	e := instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)
	d, ok := e.Data.(*events.RecordingFinishedData)
	if !ok {
		t.Fatalf("unexpected event data type %T", e.Data)
	}
	if !strings.HasPrefix(d.File, "gs://recordings/dev/") {
		t.Errorf("recording.finished file = %q, want a gs://recordings/dev/ location", d.File)
	}
	if n := gcs.uploads.Load(); n != 1 {
		t.Fatalf("expected exactly 1 upload, got %d", n)
	}
	// GCS_OBJECT_NAME_PREFIX=dev is a bare id: the separator has to be supplied
	// by the backend, or every recording lands in a "dev<file>" sibling object.
	if names := gcs.objectNames(); !strings.HasPrefix(names[0], "dev/") {
		t.Errorf("object name = %q, want the dev/ prefix", names[0])
	}
}

// storage=gcs with nothing configured must fail record-start, not start a
// recording that has nowhere to go.
func TestGCSRecording_UnconfiguredRejectsRecordStart(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID),
		map[string]any{"storage": "gcs"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("record start: status %d, want 400 when GCS is not configured", resp.StatusCode)
	}
}

// The per-request path end to end, on the recording that uploads twice through
// one backend: the merged per-participant file and the room mix. Both must land
// in the request's bucket, and the backend is only released once both have.
func TestGCSRecording_PerRequestBucketUploadsMultiChannelAndMix(t *testing.T) {
	gcs := newFakeGCS(t)
	// The client skips Application Default Credentials entirely when pointed at
	// an emulator, which is the only way to reach a fake from the per-request
	// path — it takes no endpoint of its own.
	t.Setenv("STORAGE_EMULATOR_HOST", strings.TrimPrefix(gcs.url, "http://"))

	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]any{"sample_rate": 16000})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	join := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
		map[string]any{"leg_id": outboundID})
	if join.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: status %d", join.StatusCode)
	}
	join.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID && e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	recURL := fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID)
	resp := httpPost(t, recURL, map[string]any{
		"storage":       "gcs",
		"gcs_bucket":    "request-bucket",
		"multi_channel": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("room record start: status %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	time.Sleep(300 * time.Millisecond)

	stop := httpDelete(t, recURL)
	if stop.StatusCode != http.StatusOK {
		t.Fatalf("room record stop: status %d", stop.StatusCode)
	}
	var stopped struct {
		File             string `json:"file"`
		MultiChannelFile string `json:"multi_channel_file"`
	}
	decodeJSON(t, stop, &stopped)

	if !strings.HasPrefix(stopped.File, "gs://request-bucket/") {
		t.Errorf("mix file = %q, want a gs://request-bucket/ location", stopped.File)
	}
	if !strings.HasPrefix(stopped.MultiChannelFile, "gs://request-bucket/") {
		t.Errorf("multi_channel_file = %q, want a gs://request-bucket/ location", stopped.MultiChannelFile)
	}
	if n := gcs.uploads.Load(); n != 2 {
		t.Errorf("expected 2 uploads (multi-channel merge + mix), got %d", n)
	}
}

// A room recording takes a different path to the same backend, so it gets its
// own coverage: the merged mix must be uploaded and reported as a gs:// URI.
func TestGCSRecording_RoomUploadsMix(t *testing.T) {
	gcs := newFakeGCS(t)

	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	instA.apiSrv.GCS = gcsBackend(t, gcs, "recordings", "")

	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]any{"sample_rate": 16000})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	join := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
		map[string]any{"leg_id": outboundID})
	if join.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: status %d", join.StatusCode)
	}
	join.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID && e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	recURL := fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID)
	resp := httpPost(t, recURL, map[string]any{"storage": "gcs"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("room record start: status %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	time.Sleep(300 * time.Millisecond)

	stop := httpDelete(t, recURL)
	if stop.StatusCode != http.StatusOK {
		t.Fatalf("room record stop: status %d", stop.StatusCode)
	}
	var stopped recordingResponse
	decodeJSON(t, stop, &stopped)
	if !strings.HasPrefix(stopped.File, "gs://recordings/") {
		t.Errorf("stop response file = %q, want a gs://recordings/ location", stopped.File)
	}
	if n := gcs.uploads.Load(); n != 1 {
		t.Errorf("expected exactly 1 upload, got %d", n)
	}
}
