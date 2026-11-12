package api

import (
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/providercfg"
	"github.com/VoiceBlender/voiceblender/internal/stt"
)

func legTranscriberFor(legID string) stt.Provider {
	legTranscribers.Lock()
	defer legTranscribers.Unlock()
	return legTranscribers.m[legID]
}

// TestLegSTTRestart_OldTranscriberExitLeavesNewOne stops STT on a standalone
// leg and starts it again before the first transcriber has returned. A
// transcriber returns only when its send loop next reads leg audio, so the
// audio is paused across the restart.
func TestLegSTTRestart_OldTranscriberExitLeavesNewOne(t *testing.T) {
	const legID = "stt-restart-leg"
	s := newTestServer(t)
	bot := newSwapBot(t)
	s.Config.Deepgram = providercfg.Deepgram{BaseURL: bot.httpURL}
	audio := newPausedAudio(t)
	s.LegMgr.Add(&apiMockLeg{id: legID, reader: audio, createdAt: time.Now()})
	t.Cleanup(func() { _, _ = s.doStopSTTLeg(legID) })

	req := STTRequest{Provider: "deepgram", APIKey: "test-key"}
	if _, err := s.doStartSTTLeg(legID, req); err != nil {
		t.Fatalf("start first STT: %v", err)
	}
	swapEventually(t, "first transcriber to read leg audio", func() bool {
		return bot.count() == 1 && audio.readers.Load() == 1
	})
	first := legTranscriberFor(legID)
	if _, err := s.doStopSTTLeg(legID); err != nil {
		t.Fatalf("stop first STT: %v", err)
	}
	if _, err := s.doStartSTTLeg(legID, req); err != nil {
		t.Fatalf("start second STT: %v", err)
	}
	swapEventually(t, "second transcriber to connect", func() bool { return bot.count() == 2 })
	second := legTranscriberFor(legID)
	if second == nil || second == first {
		t.Fatal("second transcriber is not registered")
	}
	if !first.Running() {
		t.Fatal("first transcriber returned before the audio resumed; the test no longer forces the order")
	}

	audio.resume()
	swapEventually(t, "first transcriber to exit", func() bool { return !first.Running() })

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if legTranscriberFor(legID) != second {
			t.Fatal("the stopped transcriber's exit dropped the transcriber started after it")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if _, err := s.doStopSTTLeg(legID); err != nil {
		t.Fatalf("stop second STT: %v", err)
	}
	swapEventually(t, "second transcriber's connection to close", func() bool { return bot.isClosed(1) })
}

// TestLegSTT_TranscriberEndFreesLeg: a transcriber that ends by itself must
// still free the leg for the next one.
func TestLegSTT_TranscriberEndFreesLeg(t *testing.T) {
	const legID = "stt-end-leg"
	s := newTestServer(t)
	bot := newSwapBot(t)
	s.Config.Deepgram = providercfg.Deepgram{BaseURL: bot.httpURL}
	audio := newPausedAudio(t)
	audio.resume()
	s.LegMgr.Add(&apiMockLeg{id: legID, reader: audio, createdAt: time.Now()})
	t.Cleanup(func() { _, _ = s.doStopSTTLeg(legID) })

	if _, err := s.doStartSTTLeg(legID, STTRequest{Provider: "deepgram", APIKey: "test-key"}); err != nil {
		t.Fatalf("start STT: %v", err)
	}
	swapEventually(t, "transcriber to connect", func() bool { return bot.count() == 1 })
	bot.hangUp(0)
	swapEventually(t, "ended transcriber to be dropped", func() bool { return legTranscriberFor(legID) == nil })
}
