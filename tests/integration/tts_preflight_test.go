//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/tts"
	"github.com/gobwas/ws/wsutil"
)

// slowTTSProvider stands in for a real provider's synthesis latency, which is
// the whole cost preflight exists to move off the critical path.
type slowTTSProvider struct {
	delay time.Duration
	audio []byte
}

func (p *slowTTSProvider) Synthesize(ctx context.Context, _ string, _ tts.Options) (*tts.Result, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &tts.Result{
		Audio:    io.NopCloser(bytes.NewReader(p.audio)),
		MimeType: "audio/pcm;rate=16000",
	}, nil
}

// awaitVSIEvent reads until an event frame of the given type arrives. Events
// are flattened into the envelope, so the whole frame is returned.
func awaitVSIEvent(t *testing.T, conn net.Conn, typ string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		data, err := wsutil.ReadServerText(conn)
		if err != nil {
			t.Fatalf("read while waiting for %q: %v", typ, err)
		}
		var f map[string]interface{}
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f["type"] == typ {
			return f
		}
	}
	t.Fatalf("timed out waiting for event %q", typ)
	return nil
}

// 1 second of 16kHz mono PCM16.
func onesecPCM() []byte { return make([]byte, 32000) }

// The point of the feature: with preflight, the synthesis delay is paid before
// the commit, so playback starts promptly once the app decides to speak.
func TestTTSPreflight_CommitAvoidsSynthesisLatency(t *testing.T) {
	const synthDelay = 700 * time.Millisecond

	instA := newTestInstance(t, "pf-a")
	instB := newTestInstance(t, "pf-b")
	instA.apiSrv.TTS = &slowTTSProvider{delay: synthDelay, audio: onesecPCM()}
	outID, _ := establishCall(t, instA, instB)

	// Baseline: leg_tts pays the synthesis delay on the critical path.
	directStart := time.Now()
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts", instA.baseURL(), outID), map[string]interface{}{
		"text": "baseline", "voice": "v", "api_key": "k",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("leg_tts: status %d", resp.StatusCode)
	}
	var direct struct {
		TTSID string `json:"tts_id"`
	}
	decodeJSON(t, resp, &direct)
	instA.collector.waitForMatch(t, events.TTSStarted, func(e events.Event) bool {
		d, ok := e.Data.(*events.TTSStartedData)
		return ok && d.TTSID == direct.TTSID
	}, 5*time.Second)
	directLatency := time.Since(directStart)
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), outID, direct.TTSID))

	// Preflight: stage first, then commit.
	resp = httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts/preflight", instA.baseURL(), outID), map[string]interface{}{
		"text": "staged reply", "voice": "v", "api_key": "k",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight: status %d", resp.StatusCode)
	}
	var staged struct {
		TTSID  string `json:"tts_id"`
		Status string `json:"status"`
	}
	decodeJSON(t, resp, &staged)
	if staged.Status != "staged" {
		t.Fatalf("preflight status = %q, want staged", staged.Status)
	}

	ev := instA.collector.waitForMatch(t, events.TTSStaged, func(e events.Event) bool {
		d, ok := e.Data.(*events.TTSStagedData)
		return ok && d.TTSID == staged.TTSID
	}, 5*time.Second)
	sd := ev.Data.(*events.TTSStagedData)
	if sd.Bytes != len(onesecPCM()) || sd.DurationMs != 1000 {
		t.Errorf("tts.staged = %d bytes / %dms, want 32000 bytes / 1000ms", sd.Bytes, sd.DurationMs)
	}

	commitStart := time.Now()
	resp = httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts/%s/commit", instA.baseURL(), outID, staged.TTSID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commit: status %d", resp.StatusCode)
	}
	instA.collector.waitForMatch(t, events.TTSStarted, func(e events.Event) bool {
		d, ok := e.Data.(*events.TTSStartedData)
		return ok && d.TTSID == staged.TTSID
	}, 5*time.Second)
	commitLatency := time.Since(commitStart)

	t.Logf("leg_tts -> first audio: %v; commit -> first audio: %v (synthesis delay %v)",
		directLatency, commitLatency, synthDelay)

	if directLatency < synthDelay {
		t.Fatalf("baseline latency %v is below the synthesis delay %v; the fixture is not exercising synthesis", directLatency, synthDelay)
	}
	if commitLatency >= synthDelay {
		t.Fatalf("commit -> first audio was %v, no better than paying the %v synthesis delay; the head start was lost", commitLatency, synthDelay)
	}
}

func TestTTSPreflight_DiscardOverVSI(t *testing.T) {
	instA := newTestInstance(t, "pfd-a")
	instB := newTestInstance(t, "pfd-b")
	instA.apiSrv.TTS = &slowTTSProvider{delay: 50 * time.Millisecond, audio: onesecPCM()}
	outID, _ := establishCall(t, instA, instB)

	conn := dialVSI(t, instA)
	defer conn.Close()
	if f := readWSFrame(t, conn, 5*time.Second); f.Type != "connected" {
		t.Fatalf("first frame = %q, want connected", f.Type)
	}

	res := vsiSend(t, conn, "leg_tts_preflight", "1", map[string]interface{}{
		"id": outID, "text": "draft", "voice": "v", "api_key": "k",
	})
	if res.Type != "leg_tts_preflight.result" {
		t.Fatalf("type = %q, want leg_tts_preflight.result", res.Type)
	}
	var staged struct {
		TTSID  string `json:"tts_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(res.Data, &staged); err != nil {
		t.Fatalf("decode preflight result: %v", err)
	}
	if staged.TTSID == "" || staged.Status != "staged" {
		t.Fatalf("preflight result = %+v, want a tts_id with status staged", staged)
	}
	ttsID := staged.TTSID

	awaitVSIEvent(t, conn, "tts.staged", 5*time.Second)

	discardRes := vsiSend(t, conn, "leg_tts_discard", "2", map[string]interface{}{"id": outID, "tts_id": ttsID})
	var discarded struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(discardRes.Data, &discarded); err != nil {
		t.Fatalf("decode discard result: %v", err)
	}
	if discarded.Status != "discarded" {
		t.Fatalf("discard result = %+v, want status discarded", discarded)
	}

	ev := awaitVSIEvent(t, conn, "tts.discarded", 5*time.Second)
	if ev["reason"] != "app" || ev["tts_id"] != ttsID {
		t.Fatalf("tts.discarded = %v, want reason app for %s", ev, ttsID)
	}

	// A discarded utterance never plays.
	instA.collector.mu.Lock()
	defer instA.collector.mu.Unlock()
	for _, e := range instA.collector.events {
		if e.Type == events.TTSStarted {
			if d, ok := e.Data.(*events.TTSStartedData); ok && d.TTSID == ttsID {
				t.Fatal("tts.started fired for a discarded utterance")
			}
		}
	}
}

// The staging TTL is a leak backstop; an app that stages and then goes quiet
// must not pin the audio forever.
func TestTTSPreflight_ExpiresAfterTTL(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "pfx-a", func(c *config.Config) {
		c.TTSPreflightTTL = 300 * time.Millisecond
	})
	instB := newTestInstance(t, "pfx-b")
	instA.apiSrv.TTS = &slowTTSProvider{delay: 10 * time.Millisecond, audio: onesecPCM()}
	outID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts/preflight", instA.baseURL(), outID), map[string]interface{}{
		"text": "forgotten", "voice": "v", "api_key": "k",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight: status %d", resp.StatusCode)
	}
	var staged struct {
		TTSID string `json:"tts_id"`
	}
	decodeJSON(t, resp, &staged)

	ev := instA.collector.waitForMatch(t, events.TTSDiscarded, func(e events.Event) bool {
		d, ok := e.Data.(*events.TTSDiscardedData)
		return ok && d.TTSID == staged.TTSID
	}, 5*time.Second)
	if d := ev.Data.(*events.TTSDiscardedData); d.Reason != "expired" {
		t.Fatalf("discard reason = %q, want expired", d.Reason)
	}

	// The id is gone: committing it now is a 404 rather than a silent no-op.
	resp = httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts/%s/commit", instA.baseURL(), outID, staged.TTSID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("commit after expiry: status %d, want 404", resp.StatusCode)
	}
}

// A hangup mid-stage must release the buffered audio rather than hold it for
// the rest of the TTL.
func TestTTSPreflight_LegHangupDiscards(t *testing.T) {
	instA := newTestInstance(t, "pfh-a")
	instB := newTestInstance(t, "pfh-b")
	instA.apiSrv.TTS = &slowTTSProvider{delay: 10 * time.Millisecond, audio: onesecPCM()}
	outID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts/preflight", instA.baseURL(), outID), map[string]interface{}{
		"text": "unsent", "voice": "v", "api_key": "k",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight: status %d", resp.StatusCode)
	}
	var staged struct {
		TTSID string `json:"tts_id"`
	}
	decodeJSON(t, resp, &staged)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID))

	ev := instA.collector.waitForMatch(t, events.TTSDiscarded, func(e events.Event) bool {
		d, ok := e.Data.(*events.TTSDiscardedData)
		return ok && d.TTSID == staged.TTSID
	}, 5*time.Second)
	if d := ev.Data.(*events.TTSDiscardedData); d.Reason != "leg_gone" {
		t.Fatalf("discard reason = %q, want leg_gone", d.Reason)
	}
}
