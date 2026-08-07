package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/stt"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
)

// fakeSTTProvider satisfies stt.Provider and nothing more, standing in for the
// Azure and ElevenLabs integrations, which have no mid-stream flush.
type fakeSTTProvider struct {
	mu    sync.Mutex
	stops int
}

func (f *fakeSTTProvider) Start(context.Context, io.Reader, string, stt.Options, stt.TranscriptCallback) error {
	return nil
}

func (f *fakeSTTProvider) Stop() {
	f.mu.Lock()
	f.stops++
	f.mu.Unlock()
}

func (f *fakeSTTProvider) Running() bool { return true }

func (f *fakeSTTProvider) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

// fakeSTTFinalizer additionally satisfies stt.Finalizer, standing in for
// Deepgram.
type fakeSTTFinalizer struct {
	fakeSTTProvider
	err error

	mu        sync.Mutex
	finalizes int
}

func (f *fakeSTTFinalizer) Finalize(context.Context) error {
	f.mu.Lock()
	f.finalizes++
	f.mu.Unlock()
	return f.err
}

func (f *fakeSTTFinalizer) finalizeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finalizes
}

// registerTranscriber injects into the process-wide legTranscribers map and
// guarantees removal: a leaked entry makes unrelated tests in this package see
// "STT already running on this leg".
func registerTranscriber(t *testing.T, legID string, p stt.Provider) {
	t.Helper()
	legTranscribers.Lock()
	legTranscribers.m[legID] = p
	legTranscribers.Unlock()
	t.Cleanup(func() {
		legTranscribers.Lock()
		delete(legTranscribers.m, legID)
		legTranscribers.Unlock()
	})
}

func transcriberRegistered(legID string) bool {
	legTranscribers.Lock()
	defer legTranscribers.Unlock()
	_, ok := legTranscribers.m[legID]
	return ok
}

// "nothing to flush" must stay distinguishable from "cannot flush".
func TestFinalizeSTTLeg_NoSession404(t *testing.T) {
	s := newTestServer(t)

	w := doRequest(s, http.MethodPost, "/v1/legs/leg-absent/stt/finalize", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

// A provider without the capability must say so, not silently succeed and not
// masquerade as a missing session.
func TestFinalizeSTTLeg_UnsupportedProvider501(t *testing.T) {
	s := newTestServer(t)
	registerTranscriber(t, "leg-azure", &fakeSTTProvider{})

	w := doRequest(s, http.MethodPost, "/v1/legs/leg-azure/stt/finalize", "")
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (body %s)", w.Code, w.Body.String())
	}
}

// The happy path reaches the provider and — the whole difference from stop —
// leaves the session registered and running.
func TestFinalizeSTTLeg_Success200(t *testing.T) {
	s := newTestServer(t)
	f := &fakeSTTFinalizer{}
	registerTranscriber(t, "leg-dg", f)

	w := doRequest(s, http.MethodPost, "/v1/legs/leg-dg/stt/finalize", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"stt_finalized"`) {
		t.Errorf("body = %s, want it to carry the stt_finalized status", w.Body.String())
	}
	if got := f.finalizeCount(); got != 1 {
		t.Errorf("Finalize called %d times, want 1", got)
	}
	if got := f.stopCount(); got != 0 {
		t.Errorf("Stop called %d times, want 0: finalize must not end the session", got)
	}
	if !transcriberRegistered("leg-dg") {
		t.Error("transcriber was dropped from legTranscribers: finalize must leave STT running")
	}
}

// A provider-side failure — the teardown race where the writer is still
// published but the socket has already closed — is a conflict, not a success
// and not a panic.
func TestFinalizeSTTLeg_WriteFailure409(t *testing.T) {
	s := newTestServer(t)
	registerTranscriber(t, "leg-dead", &fakeSTTFinalizer{err: errors.New("conn closed")})

	w := doRequest(s, http.MethodPost, "/v1/legs/leg-dead/stt/finalize", "")
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
}

// The VSI parity gates only assert that a `case "leg_stt_finalize":` label
// exists, so nothing else catches a dispatch body that calls the stop helper
// instead. Both return (*T, error) and fit the surrounding shape, so the
// copy-paste compiles and type-checks.
func TestWSLegSTTFinalizeDispatchesToFinalize(t *testing.T) {
	s := newTestServer(t)
	f := &fakeSTTFinalizer{}
	registerTranscriber(t, "leg-vsi", f)

	payload, err := json.Marshal(map[string]string{"id": "leg-vsi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	lw := wsutilx.NewLockedWriter(discardConn{})
	s.wsHandleCommand(context.Background(), lw, vsiInMsg{Type: "leg_stt_finalize", Payload: payload})

	if got := f.finalizeCount(); got != 1 {
		t.Errorf("Finalize called %d times, want 1", got)
	}
	if got := f.stopCount(); got != 0 {
		t.Errorf("Stop called %d times, want 0: leg_stt_finalize must not end the session", got)
	}
	if !transcriberRegistered("leg-vsi") {
		t.Error("transcriber was dropped from legTranscribers: leg_stt_finalize must leave STT running")
	}
}
