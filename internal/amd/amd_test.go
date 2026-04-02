package amd

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// makeToneFrame creates a 20 ms frame of a 440 Hz sine tone at 16 kHz with
// the given amplitude. The resulting RMS is well above speechThreshold (300).
func makeToneFrame(amplitude int16) []byte {
	frame := make([]byte, frameSizeBytes)
	for i := 0; i < samplesPerFrame; i++ {
		s := int16(float64(amplitude) * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(s))
	}
	return frame
}

func makeSilentFrame() []byte {
	return make([]byte, frameSizeBytes)
}

// framesForDuration returns the number of 20 ms frames needed to cover d.
func framesForDuration(d time.Duration) int {
	n := int(d / frameDuration)
	if d%frameDuration != 0 {
		n++
	}
	return n
}

// buildAudio concatenates a sequence of (frame, count) pairs into a single
// byte slice suitable for an io.Reader.
func buildAudio(segments ...any) []byte {
	var buf bytes.Buffer
	for i := 0; i < len(segments); i += 2 {
		frame := segments[i].([]byte)
		count := segments[i+1].(int)
		for j := 0; j < count; j++ {
			buf.Write(frame)
		}
	}
	return buf.Bytes()
}

func TestAnalyzer_NoSpeech(t *testing.T) {
	params := DefaultParams()
	params.TotalAnalysisTime = 5000 * time.Millisecond

	// All silence — should get no_speech after initial_silence_timeout.
	silenceFrames := framesForDuration(params.InitialSilenceTimeout) + 10
	audio := buildAudio(makeSilentFrame(), silenceFrames)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultNoSpeech {
		t.Fatalf("expected no_speech, got %s", det.Result)
	}
	if det.InitialSilenceMs < int(params.InitialSilenceTimeout.Milliseconds()) {
		t.Errorf("initial_silence_ms=%d, expected >= %d", det.InitialSilenceMs, params.InitialSilenceTimeout.Milliseconds())
	}
}

func TestAnalyzer_Human(t *testing.T) {
	params := DefaultParams()
	loud := makeToneFrame(5000)
	silent := makeSilentFrame()

	// Short greeting (500 ms speech) followed by silence → human.
	speechFrames := framesForDuration(500 * time.Millisecond)
	// Need enough silence frames to exceed after_greeting_silence plus
	// the speechOffFrames debounce delay.
	silenceFrames := framesForDuration(params.AfterGreetingSilence) + speechOffFrames + 10

	audio := buildAudio(loud, speechFrames, silent, silenceFrames)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultHuman {
		t.Fatalf("expected human, got %s", det.Result)
	}
	if det.GreetingDurationMs <= 0 {
		t.Errorf("expected positive greeting duration, got %d", det.GreetingDurationMs)
	}
	if det.GreetingDurationMs >= int(params.GreetingDuration.Milliseconds()) {
		t.Errorf("greeting duration %d ms should be below machine threshold %d ms",
			det.GreetingDurationMs, params.GreetingDuration.Milliseconds())
	}
}

func TestAnalyzer_Machine(t *testing.T) {
	params := DefaultParams()
	loud := makeToneFrame(5000)

	// Long continuous speech (>= greeting_duration) → machine.
	speechFrames := framesForDuration(params.GreetingDuration) + 10
	audio := buildAudio(loud, speechFrames)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultMachine {
		t.Fatalf("expected machine, got %s", det.Result)
	}
	if det.GreetingDurationMs < int(params.GreetingDuration.Milliseconds()) {
		t.Errorf("greeting duration %d ms should be >= threshold %d ms",
			det.GreetingDurationMs, params.GreetingDuration.Milliseconds())
	}
}

func TestAnalyzer_MachineWithShortPauses(t *testing.T) {
	params := DefaultParams()
	loud := makeToneFrame(5000)
	silent := makeSilentFrame()

	// Multiple speech bursts with pauses shorter than after_greeting_silence.
	// Total speech exceeds greeting_duration → machine.
	burstFrames := framesForDuration(400 * time.Millisecond)
	// Pause shorter than after_greeting_silence but enough to reset speech state.
	pauseFrames := speechOffFrames + 2 // just enough to trigger silence state

	// 4 bursts of 400ms = 1600ms total speech (> 1500ms default threshold)
	audio := buildAudio(
		loud, burstFrames,
		silent, pauseFrames,
		loud, burstFrames,
		silent, pauseFrames,
		loud, burstFrames,
		silent, pauseFrames,
		loud, burstFrames,
		silent, 10, // trailing silence
	)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultMachine {
		t.Fatalf("expected machine, got %s (greeting_duration=%d ms)", det.Result, det.GreetingDurationMs)
	}
}

func TestAnalyzer_NotSure_Timeout(t *testing.T) {
	params := DefaultParams()
	params.TotalAnalysisTime = 500 * time.Millisecond
	params.InitialSilenceTimeout = 1000 * time.Millisecond // longer than total
	params.GreetingDuration = 1000 * time.Millisecond      // longer than total
	loud := makeToneFrame(5000)

	// Speech that doesn't trigger machine (greeting_duration too high) and
	// doesn't trigger human (no long silence). Total analysis time expires.
	speechFrames := framesForDuration(params.TotalAnalysisTime) + 5
	audio := buildAudio(loud, speechFrames)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultNotSure {
		t.Fatalf("expected not_sure, got %s", det.Result)
	}
}

func TestAnalyzer_NotSure_ContextCancelled(t *testing.T) {
	params := DefaultParams()
	loud := makeToneFrame(5000)

	// Provide enough audio, but cancel context immediately.
	speechFrames := framesForDuration(params.TotalAnalysisTime) + 10
	audio := buildAudio(loud, speechFrames)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run starts

	a := New(params)
	det := a.Run(ctx, bytes.NewReader(audio))

	if det.Result != ResultNotSure {
		t.Fatalf("expected not_sure on cancelled context, got %s", det.Result)
	}
}

func TestAnalyzer_NotSure_ReaderExhausted(t *testing.T) {
	params := DefaultParams()
	loud := makeToneFrame(5000)

	// Only 2 frames of audio — reader will return EOF quickly.
	audio := buildAudio(loud, 2)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultNotSure {
		t.Fatalf("expected not_sure on short audio, got %s", det.Result)
	}
}

func TestAnalyzer_MinimumWordLength(t *testing.T) {
	params := DefaultParams()
	params.MinimumWordLength = 200 * time.Millisecond
	loud := makeToneFrame(5000)
	silent := makeSilentFrame()

	// Very short speech burst (60 ms < 200 ms minimum_word_length) followed
	// by long silence. The burst should be treated as noise and the analyzer
	// should continue waiting, eventually hitting initial_silence_timeout.
	shortBurst := 3 // 60 ms
	silenceFrames := framesForDuration(params.InitialSilenceTimeout) + 10

	audio := buildAudio(loud, shortBurst, silent, silenceFrames)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultNoSpeech {
		t.Fatalf("expected no_speech (burst below min word length), got %s", det.Result)
	}
}

func TestAnalyzer_InitialSilenceThenHuman(t *testing.T) {
	params := DefaultParams()
	loud := makeToneFrame(5000)
	silent := makeSilentFrame()

	// Some initial silence (1s), then short greeting, then silence → human.
	initialSilence := framesForDuration(1000 * time.Millisecond)
	speechFrames := framesForDuration(500 * time.Millisecond)
	afterSilence := framesForDuration(params.AfterGreetingSilence) + speechOffFrames + 10

	audio := buildAudio(silent, initialSilence, loud, speechFrames, silent, afterSilence)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultHuman {
		t.Fatalf("expected human, got %s", det.Result)
	}
	if det.InitialSilenceMs < 900 {
		t.Errorf("initial_silence_ms=%d, expected ~1000", det.InitialSilenceMs)
	}
}

func TestParams_Validate(t *testing.T) {
	valid := DefaultParams()
	if err := valid.Validate(); err != nil {
		t.Fatalf("default params should be valid: %v", err)
	}

	tests := []struct {
		name   string
		modify func(*Params)
	}{
		{"zero initial silence", func(p *Params) { p.InitialSilenceTimeout = 0 }},
		{"negative greeting", func(p *Params) { p.GreetingDuration = -1 }},
		{"zero after greeting", func(p *Params) { p.AfterGreetingSilence = 0 }},
		{"zero total", func(p *Params) { p.TotalAnalysisTime = 0 }},
		{"zero min word", func(p *Params) { p.MinimumWordLength = 0 }},
		{"total < initial silence", func(p *Params) {
			p.TotalAnalysisTime = 1000 * time.Millisecond
			p.InitialSilenceTimeout = 2000 * time.Millisecond
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := DefaultParams()
			tt.modify(&p)
			if err := p.Validate(); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestMergeMillis(t *testing.T) {
	defaults := DefaultParams()

	// All zeros → keep defaults.
	p := MergeMillis(defaults, 0, 0, 0, 0, 0, 0)
	if p != defaults {
		t.Error("all-zero merge should return defaults")
	}

	// Override one field.
	p = MergeMillis(defaults, 3000, 0, 0, 0, 0, 0)
	if p.InitialSilenceTimeout != 3000*time.Millisecond {
		t.Errorf("expected 3000ms, got %v", p.InitialSilenceTimeout)
	}
	if p.GreetingDuration != defaults.GreetingDuration {
		t.Error("non-overridden field should stay default")
	}
}

func TestComputeRMS(t *testing.T) {
	// Silence
	silence := make([]int16, samplesPerFrame)
	if rms := computeRMS(silence); rms != 0 {
		t.Errorf("silence RMS = %f, want 0", rms)
	}

	// Constant signal
	constant := make([]int16, samplesPerFrame)
	for i := range constant {
		constant[i] = 1000
	}
	if rms := computeRMS(constant); math.Abs(rms-1000) > 0.1 {
		t.Errorf("constant RMS = %f, want 1000", rms)
	}

	// Empty
	if rms := computeRMS(nil); rms != 0 {
		t.Errorf("nil RMS = %f, want 0", rms)
	}
}

// --- Goertzel / Beep detection tests ---

// makeSineFrame generates a 20ms frame of a pure sine wave at the given
// frequency and amplitude, at 16 kHz sample rate.
func makeSineFrame(freq float64, amplitude int16) []byte {
	frame := make([]byte, frameSizeBytes)
	for i := 0; i < samplesPerFrame; i++ {
		s := int16(float64(amplitude) * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(s))
	}
	return frame
}

func TestGoertzelEnergy_PureTone(t *testing.T) {
	// Generate a 1000 Hz tone and verify Goertzel detects it.
	samples := make([]int16, samplesPerFrame)
	for i := range samples {
		samples[i] = int16(10000 * math.Sin(2*math.Pi*1000*float64(i)/float64(sampleRate)))
	}

	energy1000 := goertzelEnergy(samples, sampleRate, 1000)
	energy500 := goertzelEnergy(samples, sampleRate, 500)
	energy2000 := goertzelEnergy(samples, sampleRate, 2000)

	// Energy at 1000 Hz should be much larger than at 500 or 2000 Hz.
	if energy1000 <= energy500 {
		t.Errorf("1000 Hz energy (%.0f) should be >> 500 Hz energy (%.0f)", energy1000, energy500)
	}
	if energy1000 <= energy2000 {
		t.Errorf("1000 Hz energy (%.0f) should be >> 2000 Hz energy (%.0f)", energy1000, energy2000)
	}
	if energy1000/energy500 < 10 {
		t.Errorf("expected at least 10x ratio, got %.1f", energy1000/energy500)
	}
}

func TestGoertzelEnergy_Silence(t *testing.T) {
	samples := make([]int16, samplesPerFrame)
	energy := goertzelEnergy(samples, sampleRate, 1000)
	if energy != 0 {
		t.Errorf("silence should have zero energy, got %f", energy)
	}
}

func TestIsTonal(t *testing.T) {
	// Pure 1000 Hz tone should be tonal in 800-1200 Hz band.
	samples := make([]int16, samplesPerFrame)
	for i := range samples {
		samples[i] = int16(10000 * math.Sin(2*math.Pi*1000*float64(i)/float64(sampleRate)))
	}
	if !isTonal(samples, sampleRate, 800, 1200, 0.3) {
		t.Error("1000 Hz tone should be detected as tonal in 800-1200 Hz band")
	}

	// Same tone should NOT be tonal in 2000-3000 Hz band.
	if isTonal(samples, sampleRate, 2000, 3000, 0.3) {
		t.Error("1000 Hz tone should not be detected in 2000-3000 Hz band")
	}

	// White noise (random samples) should not be tonal.
	noise := make([]int16, samplesPerFrame)
	for i := range noise {
		noise[i] = int16((i*17 + 31) % 10000)
	}
	if isTonal(noise, sampleRate, 800, 1200, 0.3) {
		t.Error("pseudo-noise should not be detected as tonal")
	}

	// Silence should not be tonal.
	silence := make([]int16, samplesPerFrame)
	if isTonal(silence, sampleRate, 800, 1200, 0.3) {
		t.Error("silence should not be detected as tonal")
	}
}

func TestBeepDetector_DetectsBeep(t *testing.T) {
	bd := newBeepDetector(800, 1200, 0.3, 4)

	// Feed 4 frames of 1000 Hz tone — should detect beep on frame 4.
	samples := make([]int16, samplesPerFrame)
	for i := range samples {
		samples[i] = int16(10000 * math.Sin(2*math.Pi*1000*float64(i)/float64(sampleRate)))
	}

	for i := 0; i < 3; i++ {
		if bd.feed(samples) {
			t.Fatalf("premature beep on frame %d", i)
		}
	}
	if !bd.feed(samples) {
		t.Error("expected beep on frame 4")
	}
}

func TestBeepDetector_NoBeepOnSpeech(t *testing.T) {
	bd := newBeepDetector(800, 1200, 0.3, 4)

	// Speech-like audio (440 Hz tone — like the mixer test) has multiple
	// harmonics and should not trigger beep detection at 800-1200 Hz.
	// Actually 440 Hz is below the band, so it shouldn't trigger.
	samples := make([]int16, samplesPerFrame)
	for i := range samples {
		samples[i] = int16(5000 * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate)))
	}

	for i := 0; i < 20; i++ {
		if bd.feed(samples) {
			t.Fatalf("false beep on 440 Hz speech at frame %d", i)
		}
	}
}

func TestBeepDetector_ResetOnGap(t *testing.T) {
	bd := newBeepDetector(800, 1200, 0.3, 4)

	beepSamples := make([]int16, samplesPerFrame)
	for i := range beepSamples {
		beepSamples[i] = int16(10000 * math.Sin(2*math.Pi*1000*float64(i)/float64(sampleRate)))
	}
	silentSamples := make([]int16, samplesPerFrame)

	// Feed 2 beep frames, then 1 silent, then 2 beep — should NOT detect
	// because consecutive count resets.
	bd.feed(beepSamples)
	bd.feed(beepSamples)
	bd.feed(silentSamples) // reset
	bd.feed(beepSamples)
	if bd.feed(beepSamples) {
		t.Error("should not detect beep — gap broke consecutive count")
	}
	// Two more should trigger (total 4 consecutive).
	bd.feed(beepSamples)
	if !bd.feed(beepSamples) {
		t.Error("expected beep after 4 consecutive frames")
	}
}

func TestAnalyzer_MachineWithBeep(t *testing.T) {
	params := DefaultParams()
	params.GreetingDuration = 500 * time.Millisecond
	params.BeepTimeout = 3000 * time.Millisecond

	loud := makeToneFrame(5000) // 440 Hz speech-like
	silent := makeSilentFrame()
	beepTone := makeSineFrame(1000, 10000) // 1000 Hz beep

	// Long speech (600ms > 500ms threshold) → machine, then silence, then beep.
	speechFrames := framesForDuration(600 * time.Millisecond)
	silenceFrames := 10 // 200ms gap
	beepFrames := 10    // 200ms of 1000 Hz beep (well > 4 frame minimum)

	audio := buildAudio(
		loud, speechFrames,
		silent, silenceFrames,
		beepTone, beepFrames,
		silent, 10, // trailing silence
	)

	reader := bytes.NewReader(audio)
	a := New(params)
	det := a.Run(context.Background(), reader)

	if det.Result != ResultMachine {
		t.Fatalf("expected machine, got %s", det.Result)
	}

	// Now wait for beep on the remaining audio.
	beep := a.WaitForBeep(context.Background(), reader)
	if !beep.Detected {
		t.Fatal("expected beep to be detected")
	}
	if beep.BeepMs <= 0 {
		t.Errorf("beep_ms should be positive, got %d", beep.BeepMs)
	}
	t.Logf("Machine with beep: greeting=%dms beep_ms=%dms total=%dms",
		det.GreetingDurationMs, beep.BeepMs, det.TotalAnalysisMs)
}

func TestAnalyzer_MachineNoBeepTimeout(t *testing.T) {
	params := DefaultParams()
	params.GreetingDuration = 500 * time.Millisecond
	params.BeepTimeout = 500 * time.Millisecond

	loud := makeToneFrame(5000)
	silent := makeSilentFrame()

	// Long speech → machine, then silence but no beep. BeepTimeout expires.
	speechFrames := framesForDuration(600 * time.Millisecond)
	silenceFrames := framesForDuration(600 * time.Millisecond) // longer than beep timeout

	audio := buildAudio(loud, speechFrames, silent, silenceFrames)

	reader := bytes.NewReader(audio)
	a := New(params)
	det := a.Run(context.Background(), reader)

	if det.Result != ResultMachine {
		t.Fatalf("expected machine, got %s", det.Result)
	}

	beep := a.WaitForBeep(context.Background(), reader)
	if beep.Detected {
		t.Fatal("expected no beep detection (timeout)")
	}
}

func TestAnalyzer_MachineBeepDisabled(t *testing.T) {
	params := DefaultParams()
	params.GreetingDuration = 500 * time.Millisecond
	params.BeepTimeout = 0 // disabled

	loud := makeToneFrame(5000)

	// Machine detection — beep detection not called since BeepTimeout=0.
	speechFrames := framesForDuration(600 * time.Millisecond)

	audio := buildAudio(loud, speechFrames)

	a := New(params)
	det := a.Run(context.Background(), bytes.NewReader(audio))

	if det.Result != ResultMachine {
		t.Fatalf("expected machine, got %s", det.Result)
	}
	// WaitForBeep should not be called when BeepTimeout=0; the caller checks this.
	if a.Params().BeepTimeout != 0 {
		t.Fatal("BeepTimeout should be 0")
	}
}
