package recording

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/google/uuid"
)

// ChannelInfo describes a participant's channel assignment and time offsets
// within a multi-channel recording.
type ChannelInfo struct {
	Channel int   `json:"channel"`
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
}

// MultiChannelInput describes one participant's mono WAV file and when they
// joined relative to the recording start.
type MultiChannelInput struct {
	LegID      string
	FilePath   string
	JoinOffset time.Duration
}

// MultiChannelResult is the output of MergeMultiChannel.
type MultiChannelResult struct {
	FilePath string
	Channels map[string]ChannelInfo
}

// MergeMultiChannel reads per-participant mono WAV files and produces a single
// multi-channel WAV with time-aligned tracks. Each participant becomes a
// channel; silence is inserted before their join offset and after their audio
// ends, so all channels span the full recording duration.
func MergeMultiChannel(dir string, inputs []MultiChannelInput, totalDuration time.Duration, sampleRate int) (*MultiChannelResult, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("no inputs to merge")
	}

	numChannels := len(inputs)
	totalSamples := int(totalDuration.Seconds() * float64(sampleRate))

	// Open all input WAV files for reading.
	decoders := make([]*wav.Decoder, numChannels)
	files := make([]*os.File, numChannels)
	joinSamples := make([]int, numChannels)

	for i, input := range inputs {
		joinSamples[i] = int(input.JoinOffset.Seconds() * float64(sampleRate))

		f, err := os.Open(input.FilePath)
		if err != nil {
			// Close already opened files.
			for j := 0; j < i; j++ {
				files[j].Close()
			}
			return nil, fmt.Errorf("open input %s: %w", input.FilePath, err)
		}
		files[i] = f
		decoders[i] = wav.NewDecoder(f)
		if !decoders[i].IsValidFile() {
			for j := 0; j <= i; j++ {
				files[j].Close()
			}
			return nil, fmt.Errorf("invalid WAV file: %s", input.FilePath)
		}
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	// Create output file.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}
	filename := fmt.Sprintf("%s_multichannel_%s.wav",
		time.Now().Format("20060102_150405"), uuid.New().String()[:8])
	outPath := filepath.Join(dir, filename)
	outFile, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("create output: %w", err)
	}
	defer outFile.Close()

	enc := wav.NewEncoder(outFile, sampleRate, 16, numChannels, 1)
	defer enc.Close()

	// Read buffers — one per channel.
	const frameSamples = 320 // 20ms at 16kHz
	readBufs := make([]*audio.IntBuffer, numChannels)
	for i := range readBufs {
		readBufs[i] = &audio.IntBuffer{
			Data:           make([]int, frameSamples),
			Format:         &audio.Format{SampleRate: sampleRate, NumChannels: 1},
			SourceBitDepth: 16,
		}
	}

	// Track how many samples we've read from each channel's decoder.
	readPos := make([]int, numChannels)
	// Track whether each decoder has reached EOF.
	eof := make([]bool, numChannels)

	outBuf := &audio.IntBuffer{
		Format: &audio.Format{SampleRate: sampleRate, NumChannels: numChannels},
	}

	// Process one frame at a time across all channels.
	for samplePos := 0; samplePos < totalSamples; samplePos += frameSamples {
		remaining := frameSamples
		if samplePos+remaining > totalSamples {
			remaining = totalSamples - samplePos
		}

		// Read one frame from each channel's input.
		channelData := make([][]int, numChannels)
		for ch := 0; ch < numChannels; ch++ {
			channelData[ch] = make([]int, remaining)

			if samplePos+remaining <= joinSamples[ch] || eof[ch] {
				// Entirely before this participant joined or after their file ended — silence.
				continue
			}

			// How many samples of this frame fall before the join offset?
			skipStart := 0
			if samplePos < joinSamples[ch] {
				skipStart = joinSamples[ch] - samplePos
			}

			toRead := remaining - skipStart
			readBufs[ch].Data = readBufs[ch].Data[:toRead]
			n, readErr := decoders[ch].PCMBuffer(readBufs[ch])
			if n > 0 {
				copy(channelData[ch][skipStart:], readBufs[ch].Data[:n])
				readPos[ch] += n
			}
			if readErr != nil || n == 0 {
				eof[ch] = true
			}
		}

		// Interleave: [ch0_s0, ch1_s0, ..., chN_s0, ch0_s1, ch1_s1, ...]
		interleaved := make([]int, remaining*numChannels)
		for s := 0; s < remaining; s++ {
			for ch := 0; ch < numChannels; ch++ {
				interleaved[s*numChannels+ch] = channelData[ch][s]
			}
		}

		outBuf.Data = interleaved
		if werr := enc.Write(outBuf); werr != nil {
			return nil, fmt.Errorf("write merged frame: %w", werr)
		}
	}

	// Build channel metadata.
	channels := make(map[string]ChannelInfo, numChannels)
	for i, input := range inputs {
		endMs := int64(float64(joinSamples[i]+readPos[i]) / float64(sampleRate) * 1000)
		if endMs > totalDuration.Milliseconds() {
			endMs = totalDuration.Milliseconds()
		}
		channels[input.LegID] = ChannelInfo{
			Channel: i,
			StartMs: input.JoinOffset.Milliseconds(),
			EndMs:   endMs,
		}
	}

	return &MultiChannelResult{
		FilePath: outPath,
		Channels: channels,
	}, nil
}

