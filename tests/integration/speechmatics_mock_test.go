//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// speechmaticsMock is a scripted stand-in for the Speechmatics realtime v2
// endpoint. SPEECHMATICS_URL points a live instance at it, so the whole
// REST -> config -> provider -> event-bus path runs without an account.
type speechmaticsMock struct {
	url string

	onStart []string // frames pushed once StartRecognition arrives
	onForce []string // frames pushed per ForceEndOfUtterance

	mu          sync.Mutex
	authz       string
	start       map[string]any
	messages    []string
	audioFrames int
	audioBytes  int
}

func newSpeechmaticsMock(t *testing.T, onStart, onForce []string) *speechmaticsMock {
	t.Helper()

	m := &speechmaticsMock{onStart: onStart, onForce: onForce}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		m.mu.Lock()
		m.authz = authz
		m.mu.Unlock()
		m.serve(conn)
	}))
	t.Cleanup(srv.Close)

	m.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return m
}

func (m *speechmaticsMock) serve(conn net.Conn) {
	for {
		data, op, err := wsutil.ReadClientData(conn)
		if err != nil {
			return
		}
		if op == ws.OpBinary {
			m.mu.Lock()
			m.audioFrames++
			m.audioBytes += len(data)
			m.mu.Unlock()
			continue
		}
		if op != ws.OpText {
			continue
		}

		var env struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		m.mu.Lock()
		m.messages = append(m.messages, env.Message)
		m.mu.Unlock()

		switch env.Message {
		case "StartRecognition":
			var cfg map[string]any
			_ = json.Unmarshal(data, &cfg)
			m.mu.Lock()
			m.start = cfg
			m.mu.Unlock()
			if !m.push(conn, `{"message":"RecognitionStarted","id":"mock-session"}`) {
				return
			}
			for _, f := range m.onStart {
				if !m.push(conn, f) {
					return
				}
			}
		case "ForceEndOfUtterance":
			for _, f := range m.onForce {
				if !m.push(conn, f) {
					return
				}
			}
		case "EndOfStream":
			m.push(conn, `{"message":"EndOfTranscript"}`)
			return
		}
	}
}

func (m *speechmaticsMock) push(conn net.Conn, frame string) bool {
	return wsutil.WriteServerText(conn, []byte(frame)) == nil
}

func (m *speechmaticsMock) handshake() (string, map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authz, m.start
}

func (m *speechmaticsMock) saw(message string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.messages {
		if msg == message {
			return true
		}
	}
	return false
}

func (m *speechmaticsMock) audio() (frames, bytes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audioFrames, m.audioBytes
}

func waitForCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

// smField walks the decoded StartRecognition frame, failing the test with the
// path that was missing rather than a bare nil-map panic.
func smField(t *testing.T, obj map[string]any, path ...string) any {
	t.Helper()
	var cur any = obj
	for i, key := range path {
		asMap, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("StartRecognition: %s is not an object", strings.Join(path[:i], "."))
		}
		cur, ok = asMap[key]
		if !ok {
			t.Fatalf("StartRecognition: missing %s", strings.Join(path[:i+1], "."))
		}
	}
	return cur
}

// smUtteranceScript is one utterance split across two AddTranscript segments,
// which is what a real session produces whenever an utterance outlives
// max_delay — the provider must join them into a single turn.
func smUtteranceScript() []string {
	return []string{
		`{"message":"AddPartialTranscript","metadata":{"transcript":"how do i","start_time":0.5,"end_time":1.1}}`,
		`{"message":"AddTranscript","metadata":{"transcript":"How do I reset ","start_time":0.5,"end_time":1.6},"results":[` +
			`{"type":"word","start_time":0.5,"end_time":0.7,"alternatives":[{"content":"How","confidence":0.98}]},` +
			`{"type":"word","start_time":0.7,"end_time":0.9,"alternatives":[{"content":"do","confidence":0.97}]},` +
			`{"type":"word","start_time":0.9,"end_time":1.1,"alternatives":[{"content":"I","confidence":0.99}]},` +
			`{"type":"word","start_time":1.1,"end_time":1.6,"alternatives":[{"content":"reset","confidence":0.96}]}]}`,
		`{"message":"AddTranscript","metadata":{"transcript":"my password?","start_time":1.7,"end_time":2.8},"results":[` +
			`{"type":"word","start_time":1.7,"end_time":1.9,"alternatives":[{"content":"my","confidence":0.99}]},` +
			`{"type":"word","start_time":1.9,"end_time":2.7,"alternatives":[{"content":"password","confidence":0.95}]},` +
			`{"type":"punctuation","start_time":2.7,"end_time":2.8,"alternatives":[{"content":"?","confidence":1.0}]}]}`,
		`{"message":"EndOfUtterance","metadata":{"start_time":2.8,"end_time":2.8}}`,
	}
}

func smForcedScript() []string {
	return []string{
		`{"message":"AddTranscript","metadata":{"transcript":"transfer me","start_time":0.4,"end_time":1.2},"results":[` +
			`{"type":"word","start_time":0.4,"end_time":0.8,"alternatives":[{"content":"transfer","confidence":0.94}]},` +
			`{"type":"word","start_time":0.8,"end_time":1.2,"alternatives":[{"content":"me","confidence":0.97}]}]}`,
		`{"message":"EndOfUtterance","metadata":{"start_time":1.2,"end_time":1.2},"forced":true}`,
	}
}

func sttTextEvents(inst *testInstance, legID string) []*events.STTTextData {
	matches := inst.collector.matchAll(events.STTText, func(e events.Event) bool {
		d, ok := e.Data.(*events.STTTextData)
		return ok && d.LegID == legID
	})
	out := make([]*events.STTTextData, len(matches))
	for i, e := range matches {
		out[i] = e.Data.(*events.STTTextData)
	}
	return out
}

func sttTurnEvents(inst *testInstance, legID string) []*events.STTTurnData {
	matches := inst.collector.matchAll(events.STTTurn, func(e events.Event) bool {
		d, ok := e.Data.(*events.STTTurnData)
		return ok && d.LegID == legID
	})
	out := make([]*events.STTTurnData, len(matches))
	for i, e := range matches {
		out[i] = e.Data.(*events.STTTurnData)
	}
	return out
}

func startSpeechmaticsSTT(t *testing.T, inst *testInstance, legID string, body map[string]any) {
	t.Helper()
	body["provider"] = "speechmatics"
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/stt", inst.baseURL(), legID), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start stt: unexpected status %d", resp.StatusCode)
	}
}

// TestSpeechmaticsMock_LegLifecycle is the end-to-end wiring check: the REST
// request reaches the wire as a StartRecognition frame, leg audio reaches the
// socket as binary frames, and the scripted transcripts come back out as
// stt.text and stt.turn events on the bus.
func TestSpeechmaticsMock_LegLifecycle(t *testing.T) {
	mock := newSpeechmaticsMock(t, smUtteranceScript(), nil)

	instA := newTestInstanceWithOpts(t, "sm-mock-a", func(c *config.Config) {
		c.SpeechmaticsAPIKey = "sm-test-key"
		c.SpeechmaticsURL = mock.url
	})
	instB := newTestInstance(t, "sm-mock-b")
	outboundID, _ := establishCall(t, instA, instB)

	startSpeechmaticsSTT(t, instA, outboundID, map[string]any{
		"partial":          true,
		"language":         "en",
		"utterance_end_ms": 800,
		"endpointing":      1000,
	})

	waitForCond(t, 10*time.Second, "StartRecognition", func() bool { return mock.saw("StartRecognition") })

	authz, start := mock.handshake()
	if authz != "Bearer sm-test-key" {
		t.Errorf("Authorization = %q, want the key from SPEECHMATICS_API_KEY", authz)
	}
	if got := smField(t, start, "audio_format", "encoding"); got != "pcm_s16le" {
		t.Errorf("audio_format.encoding = %v, want pcm_s16le", got)
	}
	if got := smField(t, start, "audio_format", "sample_rate"); got != float64(16000) {
		t.Errorf("audio_format.sample_rate = %v, want 16000", got)
	}
	if got := smField(t, start, "transcription_config", "language"); got != "en" {
		t.Errorf("transcription_config.language = %v, want en", got)
	}
	if got := smField(t, start, "transcription_config", "enable_partials"); got != true {
		t.Errorf("transcription_config.enable_partials = %v, want true", got)
	}
	// utterance_end_ms and endpointing are the request fields that carry
	// Speechmatics' two timing knobs; ms in, seconds on the wire.
	if got := smField(t, start, "transcription_config", "conversation_config", "end_of_utterance_silence_trigger"); got != 0.8 {
		t.Errorf("end_of_utterance_silence_trigger = %v, want 0.8", got)
	}
	if got := smField(t, start, "transcription_config", "max_delay"); got != float64(1) {
		t.Errorf("max_delay = %v, want 1", got)
	}

	turn := instA.collector.waitForMatch(t, events.STTTurn, func(e events.Event) bool {
		d, ok := e.Data.(*events.STTTurnData)
		return ok && d.LegID == outboundID
	}, 10*time.Second).Data.(*events.STTTurnData)

	if turn.Event != "end_of_turn" {
		t.Errorf("turn event = %q, want end_of_turn", turn.Event)
	}
	// Both AddTranscript segments belong to one utterance.
	if turn.Text != "How do I reset my password?" {
		t.Errorf("turn text = %q, want the two segments joined", turn.Text)
	}
	if len(turn.Words) != 6 {
		t.Errorf("turn carried %d words, want 6 (the punctuation result is not a word)", len(turn.Words))
	} else if turn.Words[0].Word != "How" || turn.Words[0].StartMs != 500 || turn.Words[5].Word != "password" {
		t.Errorf("word timings did not survive the hop: %+v", turn.Words)
	}
	if turn.AudioWindowStartMs != 500 || turn.AudioWindowEndMs != 2800 {
		t.Errorf("audio window = [%d, %d]ms, want [500, 2800]", turn.AudioWindowStartMs, turn.AudioWindowEndMs)
	}

	texts := sttTextEvents(instA, outboundID)
	var interim, final int
	var finalSpans [][2]int
	for _, d := range texts {
		if d.IsFinal {
			final++
			finalSpans = append(finalSpans, [2]int{d.AudioStartMs, d.AudioEndMs})
		} else {
			interim++
		}
		// The final segment precedes EndOfUtterance, so it cannot know the
		// speaker stopped; stt.turn is the only signal for that.
		if d.SpeechFinal {
			t.Errorf("stt.text %q claims speech_final", d.Text)
		}
	}
	if interim != 1 {
		t.Errorf("got %d interim transcripts, want 1", interim)
	}
	if final != 2 {
		t.Errorf("got %d final transcripts, want 2 (one per AddTranscript)", final)
	}
	// Each final carries its own segment window, not the whole utterance.
	wantSpans := [][2]int{{500, 1600}, {1700, 2800}}
	if final == 2 && finalSpans != nil && (finalSpans[0] != wantSpans[0] || finalSpans[1] != wantSpans[1]) {
		t.Errorf("final transcript spans = %v, want %v", finalSpans, wantSpans)
	}

	// Leg audio must reach the socket as binary frames, not just the control
	// frames the script drives.
	waitForCond(t, 15*time.Second, "leg audio on the Speechmatics socket", func() bool {
		_, bytes := mock.audio()
		return bytes >= 8000 // 0.25s of 16kHz mono PCM16
	})
	frames, bytes := mock.audio()
	t.Logf("mock received %d binary frames / %d bytes of audio", frames, bytes)

	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/stt", instA.baseURL(), outboundID))
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop stt: unexpected status %d", stopResp.StatusCode)
	}
	waitForCond(t, 10*time.Second, "EndOfStream", func() bool { return mock.saw("EndOfStream") })
}

// TestSpeechmaticsMock_Finalize covers POST /stt/finalize: the flush reaches
// the wire as ForceEndOfUtterance, closes a turn, and leaves the session up.
func TestSpeechmaticsMock_Finalize(t *testing.T) {
	// No start script: the only turn this session can produce is the forced one.
	mock := newSpeechmaticsMock(t, nil, smForcedScript())

	instA := newTestInstanceWithOpts(t, "sm-fin-a", func(c *config.Config) {
		c.SpeechmaticsAPIKey = "sm-test-key"
		c.SpeechmaticsURL = mock.url
	})
	instB := newTestInstance(t, "sm-fin-b")
	outboundID, _ := establishCall(t, instA, instB)

	startSpeechmaticsSTT(t, instA, outboundID, map[string]any{"partial": true})
	waitForCond(t, 10*time.Second, "StartRecognition", func() bool { return mock.saw("StartRecognition") })

	if turns := sttTurnEvents(instA, outboundID); len(turns) != 0 {
		t.Fatalf("got %d turn events before finalize, want 0", len(turns))
	}

	finResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/stt/finalize", instA.baseURL(), outboundID), nil)
	defer finResp.Body.Close()
	if finResp.StatusCode != http.StatusOK {
		t.Fatalf("finalize: status %d, want 200 (speechmatics implements stt.Finalizer)", finResp.StatusCode)
	}
	var fin struct {
		Status string `json:"status"`
	}
	decodeJSON(t, finResp, &fin)
	if fin.Status != "stt_finalized" {
		t.Errorf("finalize status = %q, want stt_finalized", fin.Status)
	}

	if !mock.saw("ForceEndOfUtterance") {
		t.Error("finalize did not reach the wire as ForceEndOfUtterance")
	}

	turn := instA.collector.waitForMatch(t, events.STTTurn, func(e events.Event) bool {
		d, ok := e.Data.(*events.STTTurnData)
		return ok && d.LegID == outboundID
	}, 10*time.Second).Data.(*events.STTTurnData)
	if turn.Event != "end_of_turn" {
		t.Errorf("turn event = %q, want end_of_turn", turn.Event)
	}
	if turn.Text != "transfer me" {
		t.Errorf("turn text = %q, want the forced transcript", turn.Text)
	}

	// The flush must not tear the session down — a second start would
	// succeed if it had.
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/stt", instA.baseURL(), outboundID), map[string]any{
		"provider": "speechmatics",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("restart after finalize: status %d, want 409 — the session should still be running", resp.StatusCode)
	}
	if mock.saw("EndOfStream") {
		t.Error("finalize closed the audio stream; it must only flush")
	}
}

// TestSpeechmaticsMock_DefaultEndOfUtterance pins the default that makes turn
// events work at all: a request that sets no timing fields still asks for
// end-of-utterance detection.
func TestSpeechmaticsMock_DefaultEndOfUtterance(t *testing.T) {
	mock := newSpeechmaticsMock(t, nil, nil)

	instA := newTestInstanceWithOpts(t, "sm-eou-a", func(c *config.Config) {
		c.SpeechmaticsAPIKey = "sm-test-key"
		c.SpeechmaticsURL = mock.url
	})
	instB := newTestInstance(t, "sm-eou-b")
	outboundID, _ := establishCall(t, instA, instB)

	startSpeechmaticsSTT(t, instA, outboundID, map[string]any{})
	waitForCond(t, 10*time.Second, "StartRecognition", func() bool { return mock.saw("StartRecognition") })

	_, start := mock.handshake()
	if got := smField(t, start, "transcription_config", "conversation_config", "end_of_utterance_silence_trigger"); got != 0.6 {
		t.Errorf("default end_of_utterance_silence_trigger = %v, want 0.6", got)
	}
	if got := smField(t, start, "transcription_config", "language"); got != "en" {
		t.Errorf("default language = %v, want en", got)
	}
	cfg, _ := smField(t, start, "transcription_config").(map[string]any)
	if _, ok := cfg["max_delay"]; ok {
		t.Errorf("max_delay was sent without an endpointing field: %v", cfg["max_delay"])
	}
	if _, ok := cfg["enable_partials"]; ok {
		t.Errorf("enable_partials was sent without a partial field: %v", cfg["enable_partials"])
	}
}
