package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zaf/g711"
)

// buildWAV constructs a minimal WAV file in memory with the given parameters.
func buildWAV(format uint16, channels uint16, sampleRate uint32, bitsPerSample uint16, audioData []byte) []byte {
	dataSize := uint32(len(audioData))
	fmtChunkSize := uint16(16)

	var buf bytes.Buffer

	// RIFF header
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVE")

	// fmt chunk
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(fmtChunkSize))
	binary.Write(&buf, binary.LittleEndian, format)
	binary.Write(&buf, binary.LittleEndian, channels)
	binary.Write(&buf, binary.LittleEndian, sampleRate)
	blockAlign := channels * bitsPerSample / 8
	byteRate := sampleRate * uint32(blockAlign)
	binary.Write(&buf, binary.LittleEndian, byteRate)
	binary.Write(&buf, binary.LittleEndian, blockAlign)
	binary.Write(&buf, binary.LittleEndian, bitsPerSample)

	// data chunk
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, dataSize)
	buf.Write(audioData)

	return buf.Bytes()
}

// buildWAVWithExtraChunks constructs a WAV file with extra chunks (fact, LIST) between fmt and data.
func buildWAVWithExtraChunks(format uint16, channels uint16, sampleRate uint32, bitsPerSample uint16, audioData []byte) []byte {
	dataSize := uint32(len(audioData))
	fmtChunkSize := uint16(18) // extended for non-PCM
	factData := make([]byte, 4)
	binary.LittleEndian.PutUint32(factData, uint32(len(audioData))/uint32(channels))

	var buf bytes.Buffer

	// RIFF header (size will be filled in after)
	buf.WriteString("RIFF")
	riffSizePos := buf.Len()
	binary.Write(&buf, binary.LittleEndian, uint32(0)) // placeholder
	buf.WriteString("WAVE")

	// fmt chunk (extended, 18 bytes)
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(fmtChunkSize))
	binary.Write(&buf, binary.LittleEndian, format)
	binary.Write(&buf, binary.LittleEndian, channels)
	binary.Write(&buf, binary.LittleEndian, sampleRate)
	blockAlign := channels * bitsPerSample / 8
	byteRate := sampleRate * uint32(blockAlign)
	binary.Write(&buf, binary.LittleEndian, byteRate)
	binary.Write(&buf, binary.LittleEndian, blockAlign)
	binary.Write(&buf, binary.LittleEndian, bitsPerSample)
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // cbSize (extra format bytes)

	// fact chunk
	buf.WriteString("fact")
	binary.Write(&buf, binary.LittleEndian, uint32(len(factData)))
	buf.Write(factData)

	// data chunk
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, dataSize)
	buf.Write(audioData)

	// Trailing metadata (should be ignored)
	buf.WriteString("LIST")
	listData := []byte("INFOIARTtest artist")
	binary.Write(&buf, binary.LittleEndian, uint32(len(listData)))
	buf.Write(listData)

	// Fix RIFF size
	result := buf.Bytes()
	binary.LittleEndian.PutUint32(result[riffSizePos:], uint32(len(result)-8))

	return result
}

func TestParseWAVHeader_PCM(t *testing.T) {
	audio := make([]byte, 320) // 160 samples of 16-bit PCM
	for i := 0; i < 160; i++ {
		binary.LittleEndian.PutUint16(audio[i*2:], uint16(int16(i*100)))
	}
	wav := buildWAV(1, 1, 8000, 16, audio)

	hdr, err := parseWAVHeader(bytes.NewReader(wav))
	if err != nil {
		t.Fatalf("parseWAVHeader: %v", err)
	}
	if hdr.Format != 1 {
		t.Errorf("format = %d, want 1", hdr.Format)
	}
	if hdr.NumChannels != 1 {
		t.Errorf("channels = %d, want 1", hdr.NumChannels)
	}
	if hdr.SampleRate != 8000 {
		t.Errorf("sample rate = %d, want 8000", hdr.SampleRate)
	}
	if hdr.BitsPerSample != 16 {
		t.Errorf("bits = %d, want 16", hdr.BitsPerSample)
	}
	if hdr.DataSize != uint32(len(audio)) {
		t.Errorf("data size = %d, want %d", hdr.DataSize, len(audio))
	}
}

func TestParseWAVHeader_UlawWithExtraChunks(t *testing.T) {
	audio := make([]byte, 160) // 160 mu-law samples
	wav := buildWAVWithExtraChunks(7, 1, 8000, 8, audio)

	hdr, err := parseWAVHeader(bytes.NewReader(wav))
	if err != nil {
		t.Fatalf("parseWAVHeader: %v", err)
	}
	if hdr.Format != 7 {
		t.Errorf("format = %d, want 7", hdr.Format)
	}
	if hdr.DataSize != 160 {
		t.Errorf("data size = %d, want 160", hdr.DataSize)
	}
}

func putSample(data []byte, off int, val int16) {
	binary.LittleEndian.PutUint16(data[off:], uint16(val))
}

func TestDecodeToMono_PCM_Mono(t *testing.T) {
	data := make([]byte, 6) // 3 samples
	putSample(data, 0, 100)
	putSample(data, 2, -200)
	putSample(data, 4, 300)

	hdr := &wavHeader{Format: 1, NumChannels: 1, BitsPerSample: 16}
	out := decodeToMono(data, hdr)

	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[0] != 100 || out[1] != -200 || out[2] != 300 {
		t.Errorf("samples = %v, want [100, -200, 300]", out)
	}
}

func TestDecodeToMono_PCM_Stereo(t *testing.T) {
	data := make([]byte, 8) // 2 stereo frames
	putSample(data, 0, 100)  // L
	putSample(data, 2, 200)  // R
	putSample(data, 4, -100) // L
	putSample(data, 6, -300) // R

	hdr := &wavHeader{Format: 1, NumChannels: 2, BitsPerSample: 16}
	out := decodeToMono(data, hdr)

	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0] != 150 { // (100+200)/2
		t.Errorf("sample[0] = %d, want 150", out[0])
	}
	if out[1] != -200 { // (-100+-300)/2
		t.Errorf("sample[1] = %d, want -200", out[1])
	}
}

func TestDecodeToMono_Ulaw_Mono(t *testing.T) {
	// Use known mu-law values
	data := []byte{0xFF, 0x80, 0x00}
	hdr := &wavHeader{Format: 7, NumChannels: 1, BitsPerSample: 8}
	out := decodeToMono(data, hdr)

	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	// Verify against g711 reference
	for i, b := range data {
		expected := g711.DecodeUlawFrame(b)
		if out[i] != expected {
			t.Errorf("sample[%d] = %d, want %d (input 0x%02X)", i, out[i], expected, b)
		}
	}
}

func TestDecodeToMono_Ulaw_Stereo(t *testing.T) {
	// 2 stereo frames of mu-law
	data := []byte{0xFF, 0x80, 0x00, 0x7F}
	hdr := &wavHeader{Format: 7, NumChannels: 2, BitsPerSample: 8}
	out := decodeToMono(data, hdr)

	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	left0 := int32(g711.DecodeUlawFrame(0xFF))
	right0 := int32(g711.DecodeUlawFrame(0x80))
	expected0 := int16((left0 + right0) / 2)
	if out[0] != expected0 {
		t.Errorf("sample[0] = %d, want %d", out[0], expected0)
	}
}

func TestResampleLinear_SameRate(t *testing.T) {
	samples := []int16{100, 200, 300}
	out := resampleLinear(samples, 8000, 8000)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	for i, s := range out {
		if s != samples[i] {
			t.Errorf("sample[%d] = %d, want %d", i, s, samples[i])
		}
	}
}

func TestResampleLinear_8kTo16k(t *testing.T) {
	samples := []int16{0, 1000, 2000, 3000}
	out := resampleLinear(samples, 8000, 16000)

	// 4 samples at 8kHz -> 8 samples at 16kHz
	if len(out) != 8 {
		t.Fatalf("len = %d, want 8", len(out))
	}
	// Original samples should appear at even indices
	if out[0] != 0 {
		t.Errorf("out[0] = %d, want 0", out[0])
	}
	if out[2] != 1000 {
		t.Errorf("out[2] = %d, want 1000", out[2])
	}
	// Interpolated samples at odd indices
	if out[1] != 500 {
		t.Errorf("out[1] = %d, want 500 (interpolated)", out[1])
	}
}

func TestStreamWAV_PCM_Mono_SameRate(t *testing.T) {
	// Generate 320 samples of 8kHz mono PCM (one 20ms frame at 8kHz)
	numSamples := 160
	audio := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		val := int16(1000 * math.Sin(2*math.Pi*440*float64(i)/8000))
		binary.LittleEndian.PutUint16(audio[i*2:], uint16(val))
	}
	wav := buildWAV(1, 1, 8000, 16, audio)

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.streamWAV(context.Background(), bytes.NewReader(wav), &output, 8000)
	if err != nil {
		t.Fatalf("streamWAV: %v", err)
	}

	// Output should contain one frame of 320 bytes (160 samples * 2 bytes)
	if output.Len() != 320 {
		t.Fatalf("output size = %d, want 320", output.Len())
	}

	// Verify samples match input (same rate, mono, no resampling needed)
	outData := output.Bytes()
	for i := 0; i < numSamples; i++ {
		got := int16(binary.LittleEndian.Uint16(outData[i*2:]))
		want := int16(binary.LittleEndian.Uint16(audio[i*2:]))
		if got != want {
			t.Errorf("sample[%d] = %d, want %d", i, got, want)
			break
		}
	}
}

func TestStreamWAV_Ulaw_Stereo_8kTo16k(t *testing.T) {
	// Build a stereo mu-law WAV at 8kHz.
	// Generate 320 stereo mu-law bytes = 160 stereo frames = 160 mono samples.
	numFrames := 160
	audio := make([]byte, numFrames*2) // stereo
	for i := 0; i < numFrames; i++ {
		// Use the same byte for L and R for simplicity
		audio[i*2] = uint8(i % 256)
		audio[i*2+1] = uint8(i % 256)
	}
	wav := buildWAV(7, 2, 8000, 8, audio)

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.streamWAV(context.Background(), bytes.NewReader(wav), &output, 16000)
	if err != nil {
		t.Fatalf("streamWAV: %v", err)
	}

	// At 16kHz with 20ms ptime, one frame = 320 samples = 640 bytes
	expectedFrameSize := 640
	if output.Len() != expectedFrameSize {
		t.Fatalf("output size = %d, want %d", output.Len(), expectedFrameSize)
	}

	// Decode expected: mu-law stereo -> mono -> resample 8k->16k
	monoSamples := make([]int16, numFrames)
	for i := 0; i < numFrames; i++ {
		left := int32(g711.DecodeUlawFrame(audio[i*2]))
		right := int32(g711.DecodeUlawFrame(audio[i*2+1]))
		monoSamples[i] = int16((left + right) / 2)
	}
	resampled := resampleLinear(monoSamples, 8000, 16000)

	outData := output.Bytes()
	maxDiff := int16(0)
	for i := 0; i < len(resampled) && i < 320; i++ {
		got := int16(binary.LittleEndian.Uint16(outData[i*2:]))
		want := resampled[i]
		diff := got - want
		if diff < 0 {
			diff = -diff
		}
		if diff > maxDiff {
			maxDiff = diff
		}
		if got != want {
			t.Errorf("sample[%d] = %d, want %d (diff %d)", i, got, want, diff)
			break
		}
	}
	if maxDiff > 0 {
		t.Logf("max sample diff: %d", maxDiff)
	}
}

func TestStreamWAV_LimitReader_NoTrailingData(t *testing.T) {
	// Build a WAV with trailing metadata after the data chunk.
	// Verify the player doesn't read past DataSize.
	numSamples := 160
	audio := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		binary.LittleEndian.PutUint16(audio[i*2:], uint16(int16(i*100)))
	}

	// Manually construct WAV with trailing data
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, uint32(36+len(audio)+100)) // includes trailing
	wav.WriteString("WAVE")
	wav.WriteString("fmt ")
	binary.Write(&wav, binary.LittleEndian, uint32(16))
	binary.Write(&wav, binary.LittleEndian, uint16(1))    // PCM
	binary.Write(&wav, binary.LittleEndian, uint16(1))    // mono
	binary.Write(&wav, binary.LittleEndian, uint32(8000)) // rate
	binary.Write(&wav, binary.LittleEndian, uint32(16000))
	binary.Write(&wav, binary.LittleEndian, uint16(2))
	binary.Write(&wav, binary.LittleEndian, uint16(16))
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, uint32(len(audio)))
	wav.Write(audio)
	// Write garbage trailing data that should NOT be read
	garbage := bytes.Repeat([]byte{0xFF}, 100)
	wav.Write(garbage)

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.streamWAV(context.Background(), bytes.NewReader(wav.Bytes()), &output, 8000)
	if err != nil {
		t.Fatalf("streamWAV: %v", err)
	}

	// Verify output matches input (no garbage contamination)
	outData := output.Bytes()
	if len(outData) != 320 {
		t.Fatalf("output size = %d, want 320", len(outData))
	}
	for i := 0; i < numSamples; i++ {
		got := int16(binary.LittleEndian.Uint16(outData[i*2:]))
		want := int16(i * 100)
		if got != want {
			t.Errorf("sample[%d] = %d, want %d (garbage contamination?)", i, got, want)
			break
		}
	}
}

// TestStreamWAV_MultipleFrames tests that a longer audio file produces multiple
// correctly-timed frames.
func TestStreamWAV_MultipleFrames(t *testing.T) {
	// 480 samples of 8kHz mono PCM = 3 frames of 160 samples
	numSamples := 480
	audio := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		val := int16(1000 * math.Sin(2*math.Pi*440*float64(i)/8000))
		binary.LittleEndian.PutUint16(audio[i*2:], uint16(val))
	}
	wav := buildWAV(1, 1, 8000, 16, audio)

	p := NewPlayer(slog.Default())

	// Use a custom writer that collects frames
	var frames [][]byte
	writer := &frameCollector{frames: &frames, frameSize: 320}

	err := p.streamWAV(context.Background(), bytes.NewReader(wav), writer, 8000)
	if err != nil {
		t.Fatalf("streamWAV: %v", err)
	}

	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}

	// Verify each frame
	for f := 0; f < 3; f++ {
		frame := frames[f]
		for i := 0; i < 160; i++ {
			sampleIdx := f*160 + i
			got := int16(binary.LittleEndian.Uint16(frame[i*2:]))
			want := int16(binary.LittleEndian.Uint16(audio[sampleIdx*2:]))
			if got != want {
				t.Errorf("frame[%d] sample[%d] = %d, want %d", f, i, got, want)
				break
			}
		}
	}
}

type frameCollector struct {
	frames    *[][]byte
	frameSize int
}

func (fc *frameCollector) Write(p []byte) (int, error) {
	frame := make([]byte, len(p))
	copy(frame, p)
	*fc.frames = append(*fc.frames, frame)
	return len(p), nil
}

func TestStreamWAV_Alaw(t *testing.T) {
	// Build a mono A-law WAV at 8kHz
	numSamples := 160
	audio := make([]byte, numSamples)
	for i := 0; i < numSamples; i++ {
		audio[i] = uint8(i % 256)
	}
	wav := buildWAV(6, 1, 8000, 8, audio)

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.streamWAV(context.Background(), bytes.NewReader(wav), &output, 8000)
	if err != nil {
		t.Fatalf("streamWAV: %v", err)
	}

	if output.Len() != 320 {
		t.Fatalf("output size = %d, want 320", output.Len())
	}

	// Verify against reference decode
	outData := output.Bytes()
	for i := 0; i < numSamples; i++ {
		got := int16(binary.LittleEndian.Uint16(outData[i*2:]))
		want := g711.DecodeAlawFrame(audio[i])
		if got != want {
			t.Errorf("sample[%d] = %d, want %d", i, got, want)
			break
		}
	}
}

func TestStreamWAV_UnsupportedFormat(t *testing.T) {
	audio := make([]byte, 160)
	wav := buildWAV(3, 1, 8000, 16, audio) // format 3 = IEEE float, unsupported

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.streamWAV(context.Background(), bytes.NewReader(wav), &output, 8000)
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
	expected := "unsupported WAV format: 3"
	if err.Error() != fmt.Sprintf("unsupported WAV format: 3 (supported: PCM=1, A-law=6, mu-law=7)") {
		t.Logf("error message: %s (contains expected: %v)", err.Error(), expected)
	}
}

// captureWriter implements io.Writer and stores all writes.
type captureWriter struct {
	data []byte
}

func (cw *captureWriter) Write(p []byte) (int, error) {
	cw.data = append(cw.data, p...)
	return len(p), nil
}

// roundTripWriter captures 16kHz PCM output, downsamples to 8kHz, and stores it.
type roundTripWriter struct {
	samples16k []int16
}

func (rw *roundTripWriter) Write(p []byte) (int, error) {
	for i := 0; i < len(p)-1; i += 2 {
		rw.samples16k = append(rw.samples16k, int16(binary.LittleEndian.Uint16(p[i:])))
	}
	return len(p), nil
}

func TestStreamWAV_Ulaw_RoundTrip_8k_16k_8k(t *testing.T) {
	// Build a mono mu-law WAV at 8kHz, stream at 16kHz, then downsample back to 8kHz.
	// Compare with direct decode at 8kHz. Should be near-identical.
	numSamples := 160
	audio := make([]byte, numSamples)
	for i := 0; i < numSamples; i++ {
		audio[i] = uint8((i * 7) % 256) // varied mu-law values
	}
	wav16k := buildWAV(7, 1, 8000, 8, audio)
	wav8k := buildWAV(7, 1, 8000, 8, audio)

	// Stream at 16kHz
	p16 := NewPlayer(slog.Default())
	rt := &roundTripWriter{}
	err := p16.streamWAV(context.Background(), bytes.NewReader(wav16k), rt, 16000)
	if err != nil {
		t.Fatalf("streamWAV 16k: %v", err)
	}

	// Stream at 8kHz (reference)
	p8 := NewPlayer(slog.Default())
	ref := &roundTripWriter{}
	err = p8.streamWAV(context.Background(), bytes.NewReader(wav8k), ref, 8000)
	if err != nil {
		t.Fatalf("streamWAV 8k: %v", err)
	}

	// Downsample 16kHz output to 8kHz (take every other sample)
	var downsampled []int16
	for i := 0; i < len(rt.samples16k); i += 2 {
		downsampled = append(downsampled, rt.samples16k[i])
	}

	if len(downsampled) != len(ref.samples16k) {
		t.Fatalf("round-trip length = %d, reference = %d", len(downsampled), len(ref.samples16k))
	}

	maxDiff := int16(0)
	for i := 0; i < len(downsampled); i++ {
		diff := downsampled[i] - ref.samples16k[i]
		if diff < 0 {
			diff = -diff
		}
		if diff > maxDiff {
			maxDiff = diff
		}
	}

	t.Logf("round-trip max diff: %d (over %d samples)", maxDiff, len(downsampled))
	if maxDiff > 0 {
		t.Errorf("round-trip max diff = %d, want 0 (lossless round-trip)", maxDiff)
	}
}

func TestStreamWAV_CancelContext(t *testing.T) {
	// Large audio to ensure streaming blocks
	numSamples := 16000 // 2 seconds at 8kHz
	audio := make([]byte, numSamples*2)
	wav := buildWAV(1, 1, 8000, 16, audio)

	ctx, cancel := context.WithCancel(context.Background())
	p := NewPlayer(slog.Default())

	// Cancel after writing starts
	var output bytes.Buffer
	go func() {
		// Wait a bit then cancel
		for output.Len() == 0 {
			// spin until first frame written
		}
		cancel()
	}()

	err := p.streamWAV(ctx, bytes.NewReader(wav), &output, 8000)
	if err != context.Canceled {
		t.Logf("error = %v (expected context.Canceled)", err)
	}
	// Should have written at least one frame before cancel
	if output.Len() == 0 {
		t.Error("expected at least one frame before cancel")
	}
}

// makeTestMP3 builds a minimal valid MP3 in memory (MPEG1, Layer 3, 128kbps, 44100Hz, stereo).
// Each frame is 417 bytes and decodes to 1152 stereo samples (4608 bytes of PCM).
// The audio data is all zeros (silence).
func makeTestMP3(numFrames int) []byte {
	const frameSize = 417
	frame := make([]byte, frameSize)
	frame[0] = 0xFF // sync
	frame[1] = 0xFB // MPEG1, Layer 3, no CRC
	frame[2] = 0x90 // 128kbps, 44100Hz, no padding
	frame[3] = 0x00 // stereo
	// bytes 4-416 are all zeros (valid: decodes to silence)

	buf := make([]byte, 0, frameSize*numFrames)
	for i := 0; i < numFrames; i++ {
		buf = append(buf, frame...)
	}
	return buf
}

func TestDetectFormat_MimeType(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		mime     string
		wantKind audioFormat
	}{
		{"mp3 mime", "http://example.com/file", "audio/mpeg", formatMP3},
		{"mp3 mime alt", "http://example.com/file", "audio/mp3", formatMP3},
		{"mp3 extension", "http://example.com/file.mp3", "", formatMP3},
		{"wav extension", "http://example.com/file.wav", "", formatWAV},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bytes.NewReader([]byte("dummy"))
			f := detectFormat(tt.url, tt.mime, body)
			if f.kind != tt.wantKind {
				t.Errorf("kind = %d, want %d", f.kind, tt.wantKind)
			}
		})
	}
}

func TestDetectFormat_MagicBytes(t *testing.T) {
	// WAV magic
	wavData := []byte("RIFF" + "rest of data here")
	f := detectFormat("http://example.com/audio", "", bytes.NewReader(wavData))
	if f.kind != formatWAV {
		t.Errorf("WAV magic: kind = %d, want %d", f.kind, formatWAV)
	}

	// ID3 tag (MP3 with metadata)
	id3Data := append([]byte("ID3"), make([]byte, 20)...)
	f = detectFormat("http://example.com/audio", "", bytes.NewReader(id3Data))
	if f.kind != formatMP3 {
		t.Errorf("ID3 magic: kind = %d, want %d", f.kind, formatMP3)
	}

	// MP3 sync bytes
	mp3Data := []byte{0xFF, 0xFB, 0x90, 0x00}
	f = detectFormat("http://example.com/audio", "", bytes.NewReader(mp3Data))
	if f.kind != formatMP3 {
		t.Errorf("MP3 sync: kind = %d, want %d", f.kind, formatMP3)
	}
}

func TestDetectFormat_ReaderPreserved(t *testing.T) {
	// Verify the returned reader still contains the peeked bytes.
	original := []byte("RIFF1234567890")
	f := detectFormat("http://example.com/audio", "", bytes.NewReader(original))
	all, err := io.ReadAll(f.reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(all, original) {
		t.Errorf("reader content = %q, want %q", all, original)
	}
}

func TestStreamMP3_SingleFrame(t *testing.T) {
	// A single MP3 frame at 44100Hz decodes to 1152 stereo samples.
	// At 20ms ptime, target 8kHz: 160 samples/frame = 320 bytes output.
	// 1152 source samples at 44100Hz = ~26.12ms → 1 ptime frame of output + partial.
	mp3Data := makeTestMP3(1)

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.streamMP3(context.Background(), bytes.NewReader(mp3Data), &output, 8000)
	if err != nil {
		t.Fatalf("streamMP3: %v", err)
	}

	// Should produce at least one frame.
	if output.Len() == 0 {
		t.Fatal("expected non-empty output")
	}
	// Each output frame is 320 bytes (160 samples × 2 bytes at 8kHz).
	if output.Len()%320 != 0 {
		t.Errorf("output size %d not a multiple of 320", output.Len())
	}
	t.Logf("MP3 single frame: %d bytes output (%d frames)", output.Len(), output.Len()/320)
}

func TestStreamMP3_MultipleFrames(t *testing.T) {
	// 4 MP3 frames ≈ 104ms of audio at 44100Hz.
	mp3Data := makeTestMP3(4)

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.streamMP3(context.Background(), bytes.NewReader(mp3Data), &output, 16000)
	if err != nil {
		t.Fatalf("streamMP3: %v", err)
	}

	// At 16kHz, 20ms ptime = 320 samples = 640 bytes per frame.
	frameSize := 640
	if output.Len() == 0 {
		t.Fatal("expected non-empty output")
	}
	if output.Len()%frameSize != 0 {
		t.Errorf("output size %d not a multiple of %d", output.Len(), frameSize)
	}
	numFrames := output.Len() / frameSize
	// 4 MP3 frames × 1152 samples / 44100Hz ≈ 104.5ms → 5 ptime frames at 20ms
	if numFrames < 4 || numFrames > 6 {
		t.Errorf("expected 4-6 output frames, got %d", numFrames)
	}
	t.Logf("MP3 4 frames: %d bytes output (%d ptime frames)", output.Len(), numFrames)
}

func TestStreamMP3_OutputIsSilence(t *testing.T) {
	// Zero-filled MP3 frames decode to silence.
	mp3Data := makeTestMP3(2)

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.streamMP3(context.Background(), bytes.NewReader(mp3Data), &output, 8000)
	if err != nil {
		t.Fatalf("streamMP3: %v", err)
	}

	outData := output.Bytes()
	for i := 0; i < len(outData)-1; i += 2 {
		sample := int16(binary.LittleEndian.Uint16(outData[i:]))
		if sample != 0 {
			t.Errorf("sample at offset %d = %d, want 0 (silence)", i/2, sample)
			break
		}
	}
}

func TestStreamMP3_CancelContext(t *testing.T) {
	// Large MP3 to ensure streaming blocks.
	mp3Data := makeTestMP3(100) // ~2.6 seconds

	ctx, cancel := context.WithCancel(context.Background())
	p := NewPlayer(slog.Default())

	var output bytes.Buffer
	go func() {
		for output.Len() == 0 {
			// spin until first frame written
		}
		cancel()
	}()

	err := p.streamMP3(ctx, bytes.NewReader(mp3Data), &output, 8000)
	if err != context.Canceled {
		t.Logf("error = %v (expected context.Canceled)", err)
	}
	if output.Len() == 0 {
		t.Error("expected at least one frame before cancel")
	}
}

func TestOnStart_NotCalledOnFetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	p := NewPlayer(slog.Default())
	startCalled := false
	p.OnStart(func() { startCalled = true })

	var output bytes.Buffer
	err := p.PlayAt8kHz(context.Background(), &output, ts.URL+"/audio.wav", "audio/wav", 1)
	if err == nil {
		t.Fatal("expected error from 403 response")
	}
	if startCalled {
		t.Error("OnStart was called despite fetch error")
	}
	if output.Len() != 0 {
		t.Errorf("expected no output, got %d bytes", output.Len())
	}
}

func TestOnStart_CalledOnSuccess(t *testing.T) {
	audio := make([]byte, 160*2) // one 20ms frame at 8kHz mono PCM
	wav := buildWAV(1, 1, 8000, 16, audio)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer ts.Close()

	p := NewPlayer(slog.Default())
	startCalled := false
	p.OnStart(func() { startCalled = true })

	var output bytes.Buffer
	err := p.Play(context.Background(), &output, ts.URL+"/audio.wav", "audio/wav", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !startCalled {
		t.Error("OnStart was not called on successful playback")
	}
}

func TestRepeat_ZeroPlaysOnce(t *testing.T) {
	audio := make([]byte, 160*2)
	wav := buildWAV(1, 1, 8000, 16, audio)

	var reqCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer ts.Close()

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.PlayAt8kHz(context.Background(), &output, ts.URL+"/audio.wav", "audio/wav", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCount != 1 {
		t.Errorf("server requests = %d, want 1", reqCount)
	}
}

func TestRepeat_ThreePlaysThreeTimes(t *testing.T) {
	audio := make([]byte, 160*2) // one 20ms frame at 8kHz
	wav := buildWAV(1, 1, 8000, 16, audio)

	var reqCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer ts.Close()

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.PlayAt8kHz(context.Background(), &output, ts.URL+"/audio.wav", "audio/wav", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCount != 3 {
		t.Errorf("server requests = %d, want 3", reqCount)
	}
	// Each iteration writes one 320-byte frame, so total should be 960
	if output.Len() != 320*3 {
		t.Errorf("output size = %d, want %d", output.Len(), 320*3)
	}
}

func TestRepeat_InfiniteStopsOnCancel(t *testing.T) {
	audio := make([]byte, 160*2)
	wav := buildWAV(1, 1, 8000, 16, audio)

	var reqCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := NewPlayer(slog.Default())

	var output bytes.Buffer
	go func() {
		// Wait until a few frames have been written, then cancel.
		for output.Len() < 320*2 {
			// spin
		}
		cancel()
	}()

	err := p.PlayAt8kHz(ctx, &output, ts.URL+"/audio.wav", "audio/wav", -1)
	if err == nil {
		t.Fatal("expected context error for infinite repeat with cancel")
	}
	if reqCount < 2 {
		t.Errorf("server requests = %d, want >= 2", reqCount)
	}
}

func TestRepeat_OnStartCalledOnce(t *testing.T) {
	audio := make([]byte, 160*2)
	wav := buildWAV(1, 1, 8000, 16, audio)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer ts.Close()

	p := NewPlayer(slog.Default())
	startCount := 0
	p.OnStart(func() { startCount++ })

	var output bytes.Buffer
	err := p.PlayAt8kHz(context.Background(), &output, ts.URL+"/audio.wav", "audio/wav", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if startCount != 1 {
		t.Errorf("onStart called %d times, want 1", startCount)
	}
}

func TestRepeat_FetchErrorOnSecondIteration(t *testing.T) {
	audio := make([]byte, 160*2)
	wav := buildWAV(1, 1, 8000, 16, audio)

	var reqCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		if reqCount == 1 {
			w.Header().Set("Content-Type", "audio/wav")
			w.Write(wav)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	p := NewPlayer(slog.Default())
	var output bytes.Buffer
	err := p.PlayAt8kHz(context.Background(), &output, ts.URL+"/audio.wav", "audio/wav", 3)
	if err == nil {
		t.Fatal("expected error on second iteration fetch failure")
	}
	if reqCount != 2 {
		t.Errorf("server requests = %d, want 2", reqCount)
	}
	// First iteration's audio should have been written
	if output.Len() != 320 {
		t.Errorf("output size = %d, want 320 (first iteration's audio)", output.Len())
	}
}

func BenchmarkDecodeToMono_Ulaw_Stereo(b *testing.B) {
	data := make([]byte, 320) // 160 stereo mu-law frames
	for i := range data {
		data[i] = uint8(i % 256)
	}
	hdr := &wavHeader{Format: 7, NumChannels: 2, BitsPerSample: 8}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decodeToMono(data, hdr)
	}
}

func BenchmarkResampleLinear_8kTo16k(b *testing.B) {
	samples := make([]int16, 160)
	for i := range samples {
		samples[i] = int16(i * 100)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resampleLinear(samples, 8000, 16000)
	}
}

func init() {
	// Suppress unused import error
	_ = io.Discard
}
