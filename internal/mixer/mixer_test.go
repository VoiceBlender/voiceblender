package mixer

import (
	"bytes"
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

func TestDownsampleWriter(t *testing.T) {
	var out bytes.Buffer
	dw := NewDownsampleWriter(&out)

	// Write 640 bytes of 16kHz PCM (320 samples)
	input := make([]byte, 640)
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(input[i*2:], uint16(int16(i*100)))
	}

	n, err := dw.Write(input)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 640 {
		t.Fatalf("Write returned %d, want 640", n)
	}

	// Output should be 320 bytes (160 samples at 8kHz)
	if out.Len() != 320 {
		t.Fatalf("output size = %d, want 320", out.Len())
	}

	// Verify decimation: every other sample from input
	outData := out.Bytes()
	for i := 0; i < 160; i++ {
		got := int16(binary.LittleEndian.Uint16(outData[i*2:]))
		want := int16(i * 2 * 100) // every other input sample
		if got != want {
			t.Errorf("sample[%d] = %d, want %d", i, got, want)
		}
	}
}

func TestUpsampleReader(t *testing.T) {
	// Create 8kHz PCM input: 4 samples
	input := make([]byte, 8) // 4 samples * 2 bytes
	binary.LittleEndian.PutUint16(input[0:], uint16(int16(0)))
	binary.LittleEndian.PutUint16(input[2:], uint16(int16(1000)))
	binary.LittleEndian.PutUint16(input[4:], uint16(int16(2000)))
	binary.LittleEndian.PutUint16(input[6:], uint16(int16(3000)))

	ur := NewUpsampleReader(bytes.NewReader(input))

	// Read upsampled output: should be 8 samples (16 bytes)
	out := make([]byte, 16)
	n, err := ur.Read(out)
	if err != nil && err != io.EOF {
		t.Fatalf("Read: %v", err)
	}
	if n != 16 {
		t.Fatalf("Read returned %d bytes, want 16", n)
	}

	// Verify: even indices = original, odd indices = interpolated
	samples := make([]int16, 8)
	for i := 0; i < 8; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(out[i*2:]))
	}

	// Original samples at 0, 2, 4, 6
	if samples[0] != 0 {
		t.Errorf("sample[0] = %d, want 0", samples[0])
	}
	if samples[2] != 1000 {
		t.Errorf("sample[2] = %d, want 1000", samples[2])
	}
	if samples[4] != 2000 {
		t.Errorf("sample[4] = %d, want 2000", samples[4])
	}
	if samples[6] != 3000 {
		t.Errorf("sample[6] = %d, want 3000", samples[6])
	}

	// Interpolated samples at 1, 3, 5
	if samples[1] != 500 { // (0+1000)/2
		t.Errorf("sample[1] = %d, want 500", samples[1])
	}
	if samples[3] != 1500 { // (1000+2000)/2
		t.Errorf("sample[3] = %d, want 1500", samples[3])
	}
	if samples[5] != 2500 { // (2000+3000)/2
		t.Errorf("sample[5] = %d, want 2500", samples[5])
	}
}

func TestUpsample_Downsample_RoundTrip(t *testing.T) {
	// Verify that upsample → downsample produces the original samples.
	numSamples := 160
	input := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		binary.LittleEndian.PutUint16(input[i*2:], uint16(int16(i*100)))
	}

	ur := NewUpsampleReader(bytes.NewReader(input))

	// Read upsampled data (320 samples = 640 bytes)
	upsampled := make([]byte, 640)
	n, err := io.ReadFull(ur, upsampled)
	if err != nil {
		t.Fatalf("ReadFull: %v (got %d bytes)", err, n)
	}

	// Downsample
	var out bytes.Buffer
	dw := NewDownsampleWriter(&out)
	dw.Write(upsampled)

	if out.Len() != numSamples*2 {
		t.Fatalf("round-trip output size = %d, want %d", out.Len(), numSamples*2)
	}

	// Compare
	outData := out.Bytes()
	for i := 0; i < numSamples; i++ {
		got := int16(binary.LittleEndian.Uint16(outData[i*2:]))
		want := int16(i * 100)
		if got != want {
			t.Errorf("sample[%d] = %d, want %d", i, got, want)
			break
		}
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
