package mixer

import (
	"encoding/binary"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// captureWriter collects all data written to it.
type captureWriter struct {
	mu   sync.Mutex
	data []byte
}

func (cw *captureWriter) Write(p []byte) (int, error) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.data = append(cw.data, p...)
	return len(p), nil
}

func (cw *captureWriter) Bytes() []byte {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	out := make([]byte, len(cw.data))
	copy(out, cw.data)
	return out
}

func TestMixer_PlaybackSource_SingleParticipant(t *testing.T) {
	log := slog.Default()
	m := New(log, DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes
	spf := m.samplesPerFrame

	// Create a participant (SIP leg) that just receives audio
	participantReader, participantFeeder := io.Pipe()
	capture := &captureWriter{}

	m.AddParticipant("leg1", participantReader, capture)

	// Feed silence from the participant (they're not speaking)
	go func() {
		silence := make([]byte, fsz)
		ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 5; i++ {
			<-ticker.C
			participantFeeder.Write(silence)
		}
		participantFeeder.Close()
	}()

	// Create a playback source with known audio
	playbackReader, playbackWriter := io.Pipe()
	m.AddPlaybackSource("playback1", playbackReader)

	// Write 3 frames of known audio into the playback pipe
	numFrames := 3
	var expectedSamples []int16
	go func() {
		for f := 0; f < numFrames; f++ {
			frame := make([]byte, fsz)
			for i := 0; i < spf; i++ {
				val := int16((f + 1) * 100 * (i%10 + 1))
				binary.LittleEndian.PutUint16(frame[i*2:], uint16(val))
			}
			playbackWriter.Write(frame)
			time.Sleep(time.Duration(Ptime) * time.Millisecond)
		}
		playbackWriter.Close()
	}()

	// Build expected samples
	for f := 0; f < numFrames; f++ {
		for i := 0; i < spf; i++ {
			expectedSamples = append(expectedSamples, int16((f+1)*100*(i%10+1)))
		}
	}

	// Wait for the mixer to process frames
	time.Sleep(time.Duration((numFrames+3)*Ptime) * time.Millisecond)

	// Check that the participant received the playback audio
	data := capture.Bytes()
	if len(data) == 0 {
		t.Fatal("participant received no audio")
	}

	// Extract samples from captured data
	var gotSamples []int16
	for i := 0; i < len(data)-1; i += 2 {
		gotSamples = append(gotSamples, int16(binary.LittleEndian.Uint16(data[i:])))
	}

	// The captured output may have silence frames before and after playback.
	// Find the first non-zero sample to locate the start of playback audio.
	startIdx := -1
	for i, s := range gotSamples {
		if s != 0 {
			startIdx = i
			break
		}
	}

	if startIdx < 0 {
		t.Fatal("participant received only silence")
	}

	// Align to frame boundary
	startIdx = (startIdx / spf) * spf

	// Check we have enough samples
	needSamples := numFrames * spf
	if startIdx+needSamples > len(gotSamples) {
		t.Fatalf("not enough captured samples: have %d from idx %d, need %d",
			len(gotSamples)-startIdx, startIdx, needSamples)
	}

	maxDiff := int16(0)
	mismatches := 0
	for i := 0; i < needSamples; i++ {
		got := gotSamples[startIdx+i]
		want := expectedSamples[i]
		diff := got - want
		if diff < 0 {
			diff = -diff
		}
		if diff > maxDiff {
			maxDiff = diff
		}
		if got != want {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("sample[%d] = %d, want %d (diff %d)", i, got, want, diff)
			}
		}
	}

	if mismatches > 0 {
		t.Errorf("total mismatches: %d/%d, max diff: %d", mismatches, needSamples, maxDiff)
	} else {
		t.Logf("playback audio matched perfectly: %d samples, max diff: %d", needSamples, maxDiff)
	}
}

func TestMixer_PlaybackSource_BufferedNoDrops(t *testing.T) {
	// Verify that the playback source doesn't lose frames even under timing pressure.
	// Write frames faster than the mixer ticks, then verify all frames were received.
	log := slog.Default()
	m := New(log, DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes

	participantReader, participantFeeder := io.Pipe()
	capture := &captureWriter{}
	m.AddParticipant("leg1", participantReader, capture)

	go func() {
		silence := make([]byte, fsz)
		ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < 20; i++ {
			<-ticker.C
			participantFeeder.Write(silence)
		}
		participantFeeder.Close()
	}()

	playbackReader, playbackWriter := io.Pipe()
	m.AddPlaybackSource("playback-burst", playbackReader)

	// Write 10 frames with unique patterns — paced at 20ms like the real player
	numFrames := 10
	go func() {
		for f := 0; f < numFrames; f++ {
			frame := make([]byte, fsz)
			// Use frame number as a marker in first sample
			binary.LittleEndian.PutUint16(frame[0:], uint16(int16(1000+f)))
			playbackWriter.Write(frame)
			time.Sleep(time.Duration(Ptime) * time.Millisecond)
		}
		playbackWriter.Close()
	}()

	// Wait for all frames to be processed
	time.Sleep(time.Duration((numFrames+5)*Ptime) * time.Millisecond)

	data := capture.Bytes()
	// Extract first sample of each frame
	var frameMarkers []int16
	for i := 0; i+fsz <= len(data); i += fsz {
		s := int16(binary.LittleEndian.Uint16(data[i:]))
		if s >= 1000 && s < 1000+int16(numFrames) {
			frameMarkers = append(frameMarkers, s)
		}
	}

	t.Logf("received %d playback frames (expected %d)", len(frameMarkers), numFrames)
	if len(frameMarkers) < numFrames {
		t.Errorf("lost %d frames", numFrames-len(frameMarkers))
		t.Logf("received markers: %v", frameMarkers)
	}
}

func TestMixer_TapRecording(t *testing.T) {
	log := slog.Default()
	m := New(log, DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes
	spf := m.samplesPerFrame

	// Set up tap
	tap := &captureWriter{}
	m.SetTap(tap)

	// Add a playback source with known audio
	playbackReader, playbackWriter := io.Pipe()
	m.AddPlaybackSource("playback-tap", playbackReader)

	// Also add a participant so mixer outputs mixed audio
	participantReader, participantFeeder := io.Pipe()
	devNull := &captureWriter{}
	m.AddParticipant("leg-tap", participantReader, devNull)

	go func() {
		silence := make([]byte, fsz)
		for i := 0; i < 5; i++ {
			participantFeeder.Write(silence)
			time.Sleep(time.Duration(Ptime) * time.Millisecond)
		}
		participantFeeder.Close()
	}()

	// Write 2 frames of audio
	go func() {
		for f := 0; f < 2; f++ {
			frame := make([]byte, fsz)
			for i := 0; i < spf; i++ {
				binary.LittleEndian.PutUint16(frame[i*2:], uint16(int16(500)))
			}
			playbackWriter.Write(frame)
			time.Sleep(time.Duration(Ptime) * time.Millisecond)
		}
		playbackWriter.Close()
	}()

	time.Sleep(time.Duration(6*Ptime) * time.Millisecond)

	tapData := tap.Bytes()
	if len(tapData) == 0 {
		t.Fatal("tap received no data")
	}
	t.Logf("tap received %d bytes (%d frames)", len(tapData), len(tapData)/fsz)

	// Verify tap contains the playback audio (not just silence)
	hasNonZero := false
	for i := 0; i < len(tapData)-1; i += 2 {
		s := int16(binary.LittleEndian.Uint16(tapData[i:]))
		if s != 0 {
			hasNonZero = true
			break
		}
	}
	if !hasNonZero {
		t.Error("tap data is all silence")
	}
}

func TestMixer_SampleRateConfigurations(t *testing.T) {
	tests := []struct {
		rate    int
		wantSPF int
		wantFSZ int
	}{
		{8000, 160, 320},
		{16000, 320, 640},
		{48000, 960, 1920},
		{0, 320, 640}, // default
	}
	for _, tt := range tests {
		m := New(slog.Default(), tt.rate)
		if m.SamplesPerFrame() != tt.wantSPF {
			t.Errorf("rate=%d: SamplesPerFrame()=%d, want %d", tt.rate, m.SamplesPerFrame(), tt.wantSPF)
		}
		if m.FrameSizeBytes() != tt.wantFSZ {
			t.Errorf("rate=%d: FrameSizeBytes()=%d, want %d", tt.rate, m.FrameSizeBytes(), tt.wantFSZ)
		}
		if tt.rate == 0 {
			if m.SampleRate() != DefaultSampleRate {
				t.Errorf("rate=0: SampleRate()=%d, want %d", m.SampleRate(), DefaultSampleRate)
			}
		} else {
			if m.SampleRate() != tt.rate {
				t.Errorf("rate=%d: SampleRate()=%d", tt.rate, m.SampleRate())
			}
		}
	}
}

func TestValidSampleRate(t *testing.T) {
	valid := []int{8000, 16000, 48000}
	for _, r := range valid {
		if !ValidSampleRate(r) {
			t.Errorf("ValidSampleRate(%d) = false, want true", r)
		}
	}
	invalid := []int{0, 4000, 22050, 44100, 96000}
	for _, r := range invalid {
		if ValidSampleRate(r) {
			t.Errorf("ValidSampleRate(%d) = true, want false", r)
		}
	}
}
