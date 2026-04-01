//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/amd"
	mp3 "github.com/hajimehoshi/go-mp3"
)

const greetingsDir = "../../tests/data/greetings"

// testSource defines a directory of audio files and the expected AMD result.
type testSource struct {
	dir      string     // subdirectory name under greetingsDir
	expected amd.Result // expected classification for all files
}

// machineSource are voicemail greeting recordings — expected: "machine".
// humanSources are short human greetings — expected: "human".
var machineSources = []testSource{
	{"frankj-dob", amd.ResultMachine},
	{"gavvllaw", amd.ResultMachine},
	{"chetaniitbhilai", amd.ResultMachine},
}

var humanSources = []testSource{
	{"human", amd.ResultHuman},
}

type amdResult struct {
	file     string
	source   string
	expected amd.Result
	det      amd.Detection
	correct  bool
}

// TestAMD_Accuracy runs the AMD analyzer against real voicemail greeting
// recordings (expected: machine) and reports classification accuracy.
func TestAMD_Accuracy(t *testing.T) {
	if _, err := os.Stat(greetingsDir); os.IsNotExist(err) {
		t.Skip("test data not found — run 'make download-greetings' first")
	}
	runAccuracyTest(t, machineSources)
}

// TestAMD_FalsePositives runs the AMD analyzer against short human greeting
// recordings (expected: human) to check for false positives.
func TestAMD_FalsePositives(t *testing.T) {
	humanDir := filepath.Join(greetingsDir, "human")
	if _, err := os.Stat(humanDir); os.IsNotExist(err) {
		t.Skip("human greetings not found — run 'make gen-human-greetings' first (requires ELEVENLABS_API_KEY)")
	}
	runAccuracyTest(t, humanSources)
}

// TestAMD_AccuracyAll runs both machine and human sources together and
// prints a combined accuracy report.
func TestAMD_AccuracyAll(t *testing.T) {
	if _, err := os.Stat(greetingsDir); os.IsNotExist(err) {
		t.Skip("test data not found — run 'make download-greetings' first")
	}
	all := append(machineSources, humanSources...)
	runAccuracyTest(t, all)
}

func runAccuracyTest(t *testing.T, sources []testSource) {
	t.Helper()

	params := amd.DefaultParams()
	params.TotalAnalysisTime = 10 * time.Second
	params.BeepTimeout = 10 * time.Second

	var results []amdResult
	var total, correct int

	for _, src := range sources {
		dir := filepath.Join(greetingsDir, src.dir)
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			t.Logf("skipping %s (directory not found)", src.dir)
			continue
		}
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			ext := strings.ToLower(filepath.Ext(name))
			if ext != ".wav" && ext != ".mp3" {
				continue
			}

			path := filepath.Join(dir, name)
			expected := src.expected
			source := src.dir
			t.Run(source+"/"+name, func(t *testing.T) {
				pcm, sr, err := decodeAudioFile(path)
				if err != nil {
					t.Skipf("decode error: %v", err)
					return
				}

				if sr != 16000 {
					pcm = resample(pcm, sr, 16000)
				}

				reader := bytes.NewReader(pcmToBytes(pcm))
				start := time.Now()
				analyzer := amd.New(params)
				det := analyzer.Run(context.Background(), reader)
				elapsed := time.Since(start)

				isCorrect := det.Result == expected
				r := amdResult{
					file:     name,
					source:   source,
					expected: expected,
					det:      det,
					correct:  isCorrect,
				}
				results = append(results, r)
				total++
				if isCorrect {
					correct++
				}

				mark := "OK"
				if !isCorrect {
					mark = "MISS"
				}
				t.Logf("[%s] %-40s → %-10s (expect=%-7s greeting=%dms silence=%dms total=%dms took=%v)",
					mark, name, det.Result, expected,
					det.GreetingDurationMs, det.InitialSilenceMs, det.TotalAnalysisMs,
					elapsed.Round(time.Millisecond))
			})
		}
	}

	if total == 0 {
		t.Fatal("no audio files found")
	}

	accuracy := float64(correct) / float64(total) * 100
	t.Logf("\n=== AMD Accuracy Report ===")
	t.Logf("Total files:  %d", total)
	t.Logf("Correct:      %d", correct)
	t.Logf("Accuracy:     %.1f%%", accuracy)
	t.Logf("")

	// Breakdown by source.
	for _, src := range sources {
		var srcTotal, srcCorrect int
		for _, r := range results {
			if r.source == src.dir {
				srcTotal++
				if r.correct {
					srcCorrect++
				}
			}
		}
		if srcTotal > 0 {
			t.Logf("  %-20s %d/%d (%.0f%%) [expected: %s]", src.dir, srcCorrect, srcTotal,
				float64(srcCorrect)/float64(srcTotal)*100, src.expected)
		}
	}

	// Show misclassified files.
	var misclassified []amdResult
	for _, r := range results {
		if !r.correct {
			misclassified = append(misclassified, r)
		}
	}
	if len(misclassified) > 0 {
		t.Logf("\nMisclassified files:")
		for _, r := range misclassified {
			t.Logf("  %-45s got=%-10s expected=%-7s (greeting=%dms silence=%dms)",
				r.source+"/"+r.file, r.det.Result, r.expected,
				r.det.GreetingDurationMs, r.det.InitialSilenceMs)
		}
	}

	if accuracy < 60 {
		t.Errorf("accuracy %.1f%% is below 60%% threshold", accuracy)
	}
}

// --- Audio decoding helpers ---

// decodeAudioFile reads a WAV or MP3 file and returns mono int16 samples and
// the sample rate.
func decodeAudioFile(path string) ([]int16, int, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".wav":
		return decodeWAV(path)
	case ".mp3":
		return decodeMP3(path)
	default:
		return nil, 0, fmt.Errorf("unsupported format: %s", ext)
	}
}

func decodeWAV(path string) ([]int16, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	hdr, err := parseWAVHeader(f)
	if err != nil {
		return nil, 0, err
	}

	data := make([]byte, hdr.dataSize)
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, 0, fmt.Errorf("read WAV data: %w", err)
	}
	data = data[:n]

	samples, err := wavDataToMono(data, hdr)
	if err != nil {
		return nil, 0, err
	}
	return samples, int(hdr.sampleRate), nil
}

type wavHdr struct {
	format        uint16 // 1=PCM, 6=A-law, 7=mu-law
	numChannels   uint16
	sampleRate    uint32
	bitsPerSample uint16
	dataSize      uint32
}

func parseWAVHeader(r io.Reader) (*wavHdr, error) {
	var riffHdr [12]byte
	if _, err := io.ReadFull(r, riffHdr[:]); err != nil {
		return nil, fmt.Errorf("read RIFF header: %w", err)
	}
	if string(riffHdr[0:4]) != "RIFF" || string(riffHdr[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a valid WAV file")
	}

	h := &wavHdr{}
	foundFmt := false
	foundData := false

	for !foundData {
		var chunkHdr [8]byte
		if _, err := io.ReadFull(r, chunkHdr[:]); err != nil {
			return nil, fmt.Errorf("read chunk header: %w", err)
		}
		chunkID := string(chunkHdr[0:4])
		chunkSize := binary.LittleEndian.Uint32(chunkHdr[4:8])

		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return nil, fmt.Errorf("fmt chunk too small: %d", chunkSize)
			}
			fmtData := make([]byte, chunkSize)
			if _, err := io.ReadFull(r, fmtData); err != nil {
				return nil, fmt.Errorf("read fmt chunk: %w", err)
			}
			h.format = binary.LittleEndian.Uint16(fmtData[0:2])
			h.numChannels = binary.LittleEndian.Uint16(fmtData[2:4])
			h.sampleRate = binary.LittleEndian.Uint32(fmtData[4:8])
			h.bitsPerSample = binary.LittleEndian.Uint16(fmtData[14:16])
			foundFmt = true
		case "data":
			if !foundFmt {
				return nil, fmt.Errorf("data chunk before fmt chunk")
			}
			h.dataSize = chunkSize
			foundData = true
		default:
			// Skip unknown chunks.
			skip := make([]byte, chunkSize)
			io.ReadFull(r, skip)
		}

		// WAV chunks are word-aligned (2-byte boundary).
		if chunkID != "data" && chunkSize%2 != 0 {
			var pad [1]byte
			r.Read(pad[:])
		}
	}
	return h, nil
}

func wavDataToMono(data []byte, h *wavHdr) ([]int16, error) {
	switch h.format {
	case 1: // PCM
		if h.bitsPerSample == 16 {
			return pcmBytesToMono(data, h.numChannels), nil
		} else if h.bitsPerSample == 8 {
			return pcm8ToMono(data, h.numChannels), nil
		}
		return nil, fmt.Errorf("unsupported PCM bit depth: %d", h.bitsPerSample)
	case 6: // A-law
		return compandedToMono(data, h.numChannels, alawDecode), nil
	case 7: // mu-law
		return compandedToMono(data, h.numChannels, ulawDecode), nil
	default:
		return nil, fmt.Errorf("unsupported WAV format: %d", h.format)
	}
}

func pcmBytesToMono(data []byte, channels uint16) []int16 {
	bytesPerFrame := int(channels) * 2
	numFrames := len(data) / bytesPerFrame
	out := make([]int16, numFrames)
	if channels == 1 {
		for i := 0; i < numFrames; i++ {
			out[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
		}
	} else {
		for i := 0; i < numFrames; i++ {
			left := int32(int16(binary.LittleEndian.Uint16(data[i*bytesPerFrame:])))
			right := int32(int16(binary.LittleEndian.Uint16(data[i*bytesPerFrame+2:])))
			out[i] = int16((left + right) / 2)
		}
	}
	return out
}

func pcm8ToMono(data []byte, channels uint16) []int16 {
	bytesPerFrame := int(channels)
	numFrames := len(data) / bytesPerFrame
	out := make([]int16, numFrames)
	if channels == 1 {
		for i := 0; i < numFrames; i++ {
			out[i] = int16(data[i]-128) << 8
		}
	} else {
		for i := 0; i < numFrames; i++ {
			left := int32(data[i*bytesPerFrame]) - 128
			right := int32(data[i*bytesPerFrame+1]) - 128
			out[i] = int16((left+right)/2) << 8
		}
	}
	return out
}

func compandedToMono(data []byte, channels uint16, decode func(byte) int16) []int16 {
	bytesPerFrame := int(channels)
	numFrames := len(data) / bytesPerFrame
	out := make([]int16, numFrames)
	if channels == 1 {
		for i := 0; i < numFrames; i++ {
			out[i] = decode(data[i])
		}
	} else {
		for i := 0; i < numFrames; i++ {
			left := int32(decode(data[i*bytesPerFrame]))
			right := int32(decode(data[i*bytesPerFrame+1]))
			out[i] = int16((left + right) / 2)
		}
	}
	return out
}

// ulawDecode converts a mu-law byte to a 16-bit linear sample.
func ulawDecode(b byte) int16 {
	b = ^b
	sign := int16(1)
	if b&0x80 != 0 {
		b &= 0x7F
		sign = -1
	}
	exp := int16((b >> 4) & 0x07)
	mantissa := int16(b & 0x0F)
	sample := (mantissa<<(exp+3) | 1<<(exp+3)) - 0x84
	if exp == 0 {
		sample = (mantissa << 4) + 0x08
	}
	return sign * sample
}

// alawDecode converts an A-law byte to a 16-bit linear sample.
func alawDecode(b byte) int16 {
	b ^= 0x55
	sign := int16(1)
	if b&0x80 == 0 {
		sign = -1
	}
	b &= 0x7F
	exp := int16((b >> 4) & 0x07)
	mantissa := int16(b & 0x0F)
	var sample int16
	if exp == 0 {
		sample = (mantissa << 4) + 8
	} else {
		sample = ((mantissa << 4) + 0x108) << (exp - 1)
	}
	return sign * sample
}

func decodeMP3(path string) ([]int16, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	dec, err := mp3.NewDecoder(f)
	if err != nil {
		return nil, 0, fmt.Errorf("mp3 decode: %w", err)
	}

	sampleRate := dec.SampleRate()
	// go-mp3 outputs interleaved stereo 16-bit LE.
	buf := make([]byte, 4096)
	var samples []int16
	for {
		n, err := dec.Read(buf)
		if n > 0 {
			// Stereo to mono: average left+right.
			for i := 0; i+3 < n; i += 4 {
				left := int32(int16(binary.LittleEndian.Uint16(buf[i:])))
				right := int32(int16(binary.LittleEndian.Uint16(buf[i+2:])))
				samples = append(samples, int16((left+right)/2))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read mp3: %w", err)
		}
	}
	return samples, sampleRate, nil
}

// resample converts samples from srcRate to dstRate using linear interpolation.
func resample(samples []int16, srcRate, dstRate int) []int16 {
	if srcRate == dstRate {
		return samples
	}
	ratio := float64(srcRate) / float64(dstRate)
	outLen := int(float64(len(samples)) / ratio)
	out := make([]int16, outLen)
	for i := 0; i < outLen; i++ {
		srcPos := float64(i) * ratio
		idx := int(srcPos)
		frac := srcPos - float64(idx)
		if idx+1 < len(samples) {
			s0 := float64(samples[idx])
			s1 := float64(samples[idx+1])
			out[i] = int16(math.Round(s0 + (s1-s0)*frac))
		} else if idx < len(samples) {
			out[i] = samples[idx]
		}
	}
	return out
}

// pcmToBytes converts int16 samples to 16-bit little-endian PCM bytes.
func pcmToBytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}
