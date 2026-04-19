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
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mp3 "github.com/hajimehoshi/go-mp3"

	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/zaf/g711"
)

const (
	wavFormatPCM  = 1
	wavFormatAlaw = 6
	wavFormatUlaw = 7
)

// Player manages audio playback to an io.Writer (PCM stream).
type Player struct {
	mu      sync.Mutex
	playing bool
	cancel  context.CancelFunc
	log     *slog.Logger
	onStart func()       // called once when streaming actually begins (after successful fetch)
	volume  atomic.Int32 // -8..8, 0 = unchanged; atomic so SetVolume is safe during playback
}

func NewPlayer(log *slog.Logger) *Player {
	return &Player{log: log}
}

// SetVolume sets the playback volume level. Range: -8 (quietest) to 8 (loudest).
// 0 means no change. Each unit is ~3dB. Safe to call during active playback.
func (p *Player) SetVolume(v int) {
	if v < -8 {
		v = -8
	}
	if v > 8 {
		v = 8
	}
	p.volume.Store(int32(v))
}

// OnStart registers a callback that fires once when audio streaming begins,
// after a successful fetch and format detection. Not called on fetch errors.
func (p *Player) OnStart(fn func()) {
	p.onStart = fn
}

// Play fetches audio from url and streams 16kHz mono 16-bit PCM to writer.
// WAV files are parsed and resampled/downmixed as needed.
// Supports PCM, mu-law, and A-law encoded WAV files.
// repeat controls looping: 0 or 1 = play once, -1 = infinite, N > 1 = N times.
// Blocks until playback completes or is stopped.
func (p *Player) Play(ctx context.Context, writer io.Writer, url string, mimeType string, repeat int) error {
	return p.playAt(ctx, writer, url, mimeType, uint32(mixer.DefaultSampleRate), repeat)
}

// PlayAt8kHz fetches audio and streams 8kHz mono 16-bit PCM to writer.
// Used for direct leg playback where the leg expects 8kHz PCM.
// repeat controls looping: 0 or 1 = play once, -1 = infinite, N > 1 = N times.
func (p *Player) PlayAt8kHz(ctx context.Context, writer io.Writer, url string, mimeType string, repeat int) error {
	return p.playAt(ctx, writer, url, mimeType, 8000, repeat)
}

// PlayReader streams audio from an io.Reader at 16kHz (default mixer rate).
// Unlike Play, it does not fetch from a URL and plays only once.
func (p *Player) PlayReader(ctx context.Context, writer io.Writer, reader io.Reader, mimeType string) error {
	return p.playReader(ctx, writer, reader, mimeType, uint32(mixer.DefaultSampleRate))
}

// PlayReaderAt8kHz streams audio from an io.Reader at 8kHz (leg native rate).
// Unlike PlayAt8kHz, it does not fetch from a URL and plays only once.
func (p *Player) PlayReaderAt8kHz(ctx context.Context, writer io.Writer, reader io.Reader, mimeType string) error {
	return p.playReader(ctx, writer, reader, mimeType, 8000)
}

// PlayAtRate fetches audio and streams mono 16-bit PCM at the given target rate.
// Used for direct leg playback at the leg's native sample rate.
func (p *Player) PlayAtRate(ctx context.Context, writer io.Writer, url, mimeType string, targetRate uint32, repeat int) error {
	return p.playAt(ctx, writer, url, mimeType, targetRate, repeat)
}

// PlayReaderAtRate streams audio from an io.Reader at the given target rate.
// Used for direct leg playback at the leg's native sample rate.
func (p *Player) PlayReaderAtRate(ctx context.Context, writer io.Writer, reader io.Reader, mimeType string, targetRate uint32) error {
	return p.playReader(ctx, writer, reader, mimeType, targetRate)
}

func (p *Player) playReader(ctx context.Context, writer io.Writer, reader io.Reader, mimeType string, targetRate uint32) error {
	p.mu.Lock()
	if p.playing {
		p.mu.Unlock()
		return fmt.Errorf("playback already in progress")
	}
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.playing = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.playing = false
		p.cancel = nil
		p.mu.Unlock()
	}()

	format := detectFormat("", mimeType, reader)

	if p.onStart != nil {
		p.onStart()
	}

	switch format.kind {
	case formatRawPCM:
		return p.streamRawPCM(ctx, format.reader, writer, format.sampleRate, targetRate)
	case formatMP3:
		return p.streamMP3(ctx, format.reader, writer, targetRate)
	default:
		return p.streamWAV(ctx, format.reader, writer, targetRate)
	}
}

func (p *Player) playAt(ctx context.Context, writer io.Writer, url string, mimeType string, targetRate uint32, repeat int) error {
	p.mu.Lock()
	if p.playing {
		p.mu.Unlock()
		return fmt.Errorf("playback already in progress")
	}
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.playing = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.playing = false
		p.cancel = nil
		p.mu.Unlock()
	}()

	// Normalize: 0 means play once.
	if repeat == 0 {
		repeat = 1
	}

	// Download the file to a temp file first. This avoids:
	// - HTTP timeouts killing long-running streams
	// - Re-fetching the URL on every loop iteration
	dl, err := p.downloadToTemp(ctx, url, mimeType)
	if err != nil {
		return err
	}
	defer os.Remove(dl.file.Name())
	defer dl.file.Close()

	for iteration := 0; repeat < 0 || iteration < repeat; iteration++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Seek back to beginning for each iteration.
		if _, err := dl.file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek temp file: %w", err)
		}

		if iteration == 0 && p.onStart != nil {
			p.onStart()
		}

		var streamErr error
		switch dl.format {
		case formatRawPCM:
			streamErr = p.streamRawPCM(ctx, dl.file, writer, dl.sampleRate, targetRate)
		case formatMP3:
			streamErr = p.streamMP3(ctx, dl.file, writer, targetRate)
		default:
			streamErr = p.streamWAV(ctx, dl.file, writer, targetRate)
		}

		if streamErr != nil {
			return streamErr
		}
	}

	return nil
}

type downloadResult struct {
	file       *os.File
	format     audioFormat
	sampleRate uint32 // only set for formatRawPCM
}

// downloadToTemp fetches url into a temp file and returns the file and detected format.
func (p *Player) downloadToTemp(ctx context.Context, url, mimeType string) (downloadResult, error) {
	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return downloadResult{}, fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return downloadResult{}, fmt.Errorf("fetch audio: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return downloadResult{}, fmt.Errorf("fetch audio: status %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp("", "vb-playback-*")
	if err != nil {
		return downloadResult{}, fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return downloadResult{}, fmt.Errorf("download audio: %w", err)
	}

	// Seek to start so detectFormat can read the header.
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return downloadResult{}, fmt.Errorf("seek temp file: %w", err)
	}

	det := detectFormat(url, mimeType, tmpFile)

	// Seek to start again for the actual playback.
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return downloadResult{}, fmt.Errorf("seek temp file: %w", err)
	}

	return downloadResult{file: tmpFile, format: det.kind, sampleRate: det.sampleRate}, nil
}

func (p *Player) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *Player) IsPlaying() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playing
}

// Audio format types.
type audioFormat int

const (
	formatWAV audioFormat = iota
	formatMP3
	formatRawPCM
)

// detectedFormat holds the result of format detection with a reader
// that includes the peeked bytes prepended back.
type detectedFormat struct {
	kind       audioFormat
	reader     io.Reader
	sampleRate uint32 // only set for formatRawPCM
}

// detectFormat peeks at the first bytes of the stream and the URL/mimeType
// to determine whether the audio is WAV or MP3.
func detectFormat(url, mimeType string, body io.Reader) detectedFormat {
	// Check mime type first.
	mt := strings.ToLower(mimeType)

	// Raw PCM: "audio/pcm;rate=16000" or "audio/l16;rate=16000"
	if strings.HasPrefix(mt, "audio/pcm") || strings.HasPrefix(mt, "audio/l16") {
		rate := uint32(16000)
		if idx := strings.Index(mt, "rate="); idx != -1 {
			if v, err := strconv.ParseUint(mt[idx+5:], 10, 32); err == nil && v > 0 {
				rate = uint32(v)
			}
		}
		return detectedFormat{kind: formatRawPCM, reader: body, sampleRate: rate}
	}

	if mt == "audio/mpeg" || mt == "audio/mp3" {
		return detectedFormat{kind: formatMP3, reader: body}
	}

	// Check URL extension.
	lower := strings.ToLower(url)
	if strings.HasSuffix(lower, ".mp3") {
		return detectedFormat{kind: formatMP3, reader: body}
	}
	if strings.HasSuffix(lower, ".wav") {
		return detectedFormat{kind: formatWAV, reader: body}
	}

	// Peek at first 4 bytes.
	header := make([]byte, 4)
	n, _ := io.ReadFull(body, header)
	combined := io.MultiReader(bytes.NewReader(header[:n]), body)

	if n >= 4 && string(header[:4]) == "RIFF" {
		return detectedFormat{kind: formatWAV, reader: combined}
	}
	if n >= 3 && string(header[:3]) == "ID3" {
		return detectedFormat{kind: formatMP3, reader: combined}
	}
	// MP3 frame sync: 0xFF followed by 0xE0+ (11 sync bits set).
	if n >= 2 && header[0] == 0xFF && header[1]&0xE0 == 0xE0 {
		return detectedFormat{kind: formatMP3, reader: combined}
	}

	// Default to WAV.
	return detectedFormat{kind: formatWAV, reader: combined}
}

// streamRawPCM streams raw 16-bit signed LE mono PCM, resampling if needed.
func (p *Player) streamRawPCM(ctx context.Context, body io.Reader, writer io.Writer, srcRate, targetRate uint32) error {
	// Source: one ptime frame at srcRate, mono 16-bit = 2 bytes/sample.
	srcSamplesPerFrame := int(srcRate) * mixer.Ptime / 1000
	srcReadSize := srcSamplesPerFrame * 2

	// Target output frame.
	outSamplesPerFrame := int(targetRate) * mixer.Ptime / 1000
	outFrameSize := outSamplesPerFrame * 2

	p.log.Info("raw PCM playback starting",
		"src_rate", srcRate,
		"target_rate", targetRate,
		"src_read_size", srcReadSize,
		"out_frame_size", outFrameSize,
	)

	srcBuf := make([]byte, srcReadSize)

	ticker := time.NewTicker(time.Duration(mixer.Ptime) * time.Millisecond)
	defer ticker.Stop()

	frameCount := 0
	for {
		select {
		case <-ctx.Done():
			p.log.Info("raw PCM playback cancelled", "frames_written", frameCount)
			return ctx.Err()
		case <-ticker.C:
		}

		n, err := io.ReadFull(body, srcBuf)
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				p.log.Info("raw PCM playback complete", "frames_written", frameCount)
				return nil
			}
			if err != nil {
				return fmt.Errorf("read raw pcm: %w", err)
			}
			continue
		}

		// Decode 16-bit LE mono samples.
		numSamples := n / 2
		samples := make([]int16, numSamples)
		for i := 0; i < numSamples; i++ {
			samples[i] = int16(binary.LittleEndian.Uint16(srcBuf[i*2:]))
		}

		// Resample if needed.
		resampled := resampleLinear(samples, srcRate, targetRate)
		applyVolume(resampled, int(p.volume.Load()))

		// Write as 16-bit LE PCM, frame-sized.
		out := make([]byte, outFrameSize)
		for i := 0; i < outSamplesPerFrame && i < len(resampled); i++ {
			binary.LittleEndian.PutUint16(out[i*2:], uint16(resampled[i]))
		}

		if _, werr := writer.Write(out); werr != nil {
			return fmt.Errorf("write audio: %w", werr)
		}
		frameCount++

		if err == io.EOF || err == io.ErrUnexpectedEOF {
			p.log.Info("raw PCM playback complete", "frames_written", frameCount)
			return nil
		}
	}
}

// streamMP3 decodes an MP3 stream and writes mono PCM at the target rate.
func (p *Player) streamMP3(ctx context.Context, body io.Reader, writer io.Writer, targetRate uint32) error {
	dec, err := mp3.NewDecoder(body)
	if err != nil {
		return fmt.Errorf("mp3 decode: %w", err)
	}

	srcRate := uint32(dec.SampleRate())
	const srcChannels = 2 // go-mp3 always outputs stereo

	// Source bytes per ptime frame: stereo 16-bit samples.
	srcSamplesPerFrame := int(srcRate) * mixer.Ptime / 1000
	srcReadSize := srcSamplesPerFrame * srcChannels * 2

	// Target output frame size.
	outSamplesPerFrame := int(targetRate) * mixer.Ptime / 1000
	outFrameSize := outSamplesPerFrame * 2

	totalBytes := dec.Length()
	totalFrames := int(totalBytes) / srcReadSize

	p.log.Info("MP3 playback starting",
		"sample_rate", srcRate,
		"channels", srcChannels,
		"target_rate", targetRate,
		"src_read_size", srcReadSize,
		"out_frame_size", outFrameSize,
		"total_frames", totalFrames,
		"duration_ms", totalFrames*mixer.Ptime,
	)

	srcBuf := make([]byte, srcReadSize)

	ticker := time.NewTicker(time.Duration(mixer.Ptime) * time.Millisecond)
	defer ticker.Stop()

	frameCount := 0
	for {
		select {
		case <-ctx.Done():
			p.log.Info("MP3 playback cancelled", "frames_written", frameCount)
			return ctx.Err()
		case <-ticker.C:
		}

		n, err := io.ReadFull(dec, srcBuf)
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				p.log.Info("MP3 playback complete", "frames_written", frameCount)
				return nil
			}
			if err != nil {
				return fmt.Errorf("read mp3 data: %w", err)
			}
			continue
		}

		// Decode interleaved stereo 16-bit LE to mono.
		monoSamples := pcmToMono(srcBuf[:n], srcChannels)

		// Resample to target rate.
		resampled := resampleLinear(monoSamples, srcRate, targetRate)
		applyVolume(resampled, int(p.volume.Load()))

		// Write as 16-bit LE PCM, frame-sized.
		out := make([]byte, outFrameSize)
		for i := 0; i < outSamplesPerFrame && i < len(resampled); i++ {
			binary.LittleEndian.PutUint16(out[i*2:], uint16(resampled[i]))
		}

		if _, werr := writer.Write(out); werr != nil {
			return fmt.Errorf("write audio: %w", werr)
		}
		frameCount++

		if err == io.EOF || err == io.ErrUnexpectedEOF {
			p.log.Info("MP3 playback complete", "frames_written", frameCount)
			return nil
		}
	}
}

// wavHeader holds parsed WAV file header info.
type wavHeader struct {
	Format        uint16 // 1=PCM, 6=A-law, 7=mu-law
	NumChannels   uint16
	SampleRate    uint32
	BitsPerSample uint16
	DataSize      uint32
}

// parseWAVHeader reads a WAV file by scanning RIFF chunks properly.
// Handles extra chunks (fact, LIST, etc.) between fmt and data.
func parseWAVHeader(r io.Reader) (*wavHeader, error) {
	// Read RIFF header (12 bytes)
	var riffHdr [12]byte
	if _, err := io.ReadFull(r, riffHdr[:]); err != nil {
		return nil, fmt.Errorf("read RIFF header: %w", err)
	}
	if string(riffHdr[0:4]) != "RIFF" || string(riffHdr[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a valid WAV file")
	}

	h := &wavHeader{}
	foundFmt := false
	foundData := false

	// Scan chunks
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
				return nil, fmt.Errorf("fmt chunk too small: %d bytes", chunkSize)
			}
			fmtData := make([]byte, chunkSize)
			if _, err := io.ReadFull(r, fmtData); err != nil {
				return nil, fmt.Errorf("read fmt chunk: %w", err)
			}
			h.Format = binary.LittleEndian.Uint16(fmtData[0:2])
			h.NumChannels = binary.LittleEndian.Uint16(fmtData[2:4])
			h.SampleRate = binary.LittleEndian.Uint32(fmtData[4:8])
			// bytes 8-11: byte rate, 12-13: block align
			h.BitsPerSample = binary.LittleEndian.Uint16(fmtData[14:16])
			foundFmt = true

		case "data":
			if !foundFmt {
				return nil, fmt.Errorf("data chunk before fmt chunk")
			}
			h.DataSize = chunkSize
			foundData = true
			// Reader is now positioned at the start of audio data

		default:
			// Skip unknown chunks (fact, LIST, etc.)
			if chunkSize > 0 {
				if _, err := io.CopyN(io.Discard, r, int64(chunkSize)); err != nil {
					return nil, fmt.Errorf("skip %s chunk: %w", chunkID, err)
				}
			}
		}

		// WAV chunks are word-aligned (padded to even size)
		if chunkID != "data" && chunkSize%2 != 0 {
			io.CopyN(io.Discard, r, 1)
		}
	}

	// Validate format
	switch h.Format {
	case wavFormatPCM:
		if h.BitsPerSample != 16 {
			return nil, fmt.Errorf("unsupported PCM bits per sample: %d (only 16-bit supported)", h.BitsPerSample)
		}
	case wavFormatUlaw, wavFormatAlaw:
		if h.BitsPerSample != 8 {
			return nil, fmt.Errorf("unsupported %s bits per sample: %d (expected 8)", formatName(h.Format), h.BitsPerSample)
		}
	default:
		return nil, fmt.Errorf("unsupported WAV format: %d (supported: PCM=1, A-law=6, mu-law=7)", h.Format)
	}

	if h.NumChannels != 1 && h.NumChannels != 2 {
		return nil, fmt.Errorf("unsupported channel count: %d", h.NumChannels)
	}

	return h, nil
}

func formatName(f uint16) string {
	switch f {
	case wavFormatUlaw:
		return "mu-law"
	case wavFormatAlaw:
		return "A-law"
	default:
		return fmt.Sprintf("format(%d)", f)
	}
}

// streamWAV parses a WAV file and streams it as mono PCM at the target rate.
func (p *Player) streamWAV(ctx context.Context, body io.Reader, writer io.Writer, targetRate uint32) error {
	hdr, err := parseWAVHeader(body)
	if err != nil {
		return err
	}

	// Limit reads to the data chunk size to avoid reading trailing metadata.
	dataReader := io.LimitReader(body, int64(hdr.DataSize))

	// Calculate frame sizes based on target rate
	samplesPerFrame := int(targetRate) * mixer.Ptime / 1000
	frameSizeBytes := samplesPerFrame * 2

	// Read source in chunks sized for one ptime at the source rate
	srcSamplesPerFrame := int(hdr.SampleRate) * mixer.Ptime / 1000
	srcBytesPerSample := int(hdr.NumChannels) * int(hdr.BitsPerSample) / 8
	srcReadSize := srcSamplesPerFrame * srcBytesPerSample

	totalFrames := int(hdr.DataSize) / srcReadSize
	p.log.Info("WAV playback starting",
		"format", formatName(hdr.Format),
		"sample_rate", hdr.SampleRate,
		"channels", hdr.NumChannels,
		"bits", hdr.BitsPerSample,
		"data_size", hdr.DataSize,
		"target_rate", targetRate,
		"src_read_size", srcReadSize,
		"out_frame_size", frameSizeBytes,
		"samples_per_frame", samplesPerFrame,
		"total_frames", totalFrames,
		"duration_ms", totalFrames*mixer.Ptime,
	)

	srcBuf := make([]byte, srcReadSize)

	// Pace output at real-time: one frame every ptime interval.
	ticker := time.NewTicker(time.Duration(mixer.Ptime) * time.Millisecond)
	defer ticker.Stop()

	frameCount := 0
	for {
		select {
		case <-ctx.Done():
			p.log.Info("WAV playback cancelled", "frames_written", frameCount)
			return ctx.Err()
		case <-ticker.C:
		}

		n, err := io.ReadFull(dataReader, srcBuf)
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				p.log.Info("WAV playback complete", "frames_written", frameCount)
				return nil
			}
			if err != nil {
				return fmt.Errorf("read audio data: %w", err)
			}
			continue
		}

		// Decode to mono int16 samples
		monoSamples := decodeToMono(srcBuf[:n], hdr)

		// Resample to target rate
		resampled := resampleLinear(monoSamples, hdr.SampleRate, targetRate)
		applyVolume(resampled, int(p.volume.Load()))

		// Write as 16-bit LE PCM, frame-sized
		out := make([]byte, frameSizeBytes)
		for i := 0; i < samplesPerFrame && i < len(resampled); i++ {
			binary.LittleEndian.PutUint16(out[i*2:], uint16(resampled[i]))
		}

		if _, werr := writer.Write(out); werr != nil {
			return fmt.Errorf("write audio: %w", werr)
		}
		frameCount++

		if err == io.EOF || err == io.ErrUnexpectedEOF {
			p.log.Info("WAV playback complete", "frames_written", frameCount)
			return nil
		}
	}
}

// decodeToMono converts raw WAV audio data to mono int16 samples,
// handling PCM, mu-law, and A-law formats.
func decodeToMono(data []byte, hdr *wavHeader) []int16 {
	switch hdr.Format {
	case wavFormatPCM:
		return pcmToMono(data, hdr.NumChannels)
	case wavFormatUlaw:
		return compandedToMono(data, hdr.NumChannels, g711.DecodeUlawFrame)
	case wavFormatAlaw:
		return compandedToMono(data, hdr.NumChannels, g711.DecodeAlawFrame)
	default:
		return nil
	}
}

// pcmToMono converts 16-bit PCM data to mono int16 samples.
func pcmToMono(data []byte, numChannels uint16) []int16 {
	bytesPerFrame := int(numChannels) * 2
	numFrames := len(data) / bytesPerFrame
	out := make([]int16, numFrames)

	if numChannels == 1 {
		for i := 0; i < numFrames; i++ {
			out[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
		}
	} else {
		for i := 0; i < numFrames; i++ {
			left := int32(int16(binary.LittleEndian.Uint16(data[i*4:])))
			right := int32(int16(binary.LittleEndian.Uint16(data[i*4+2:])))
			out[i] = int16((left + right) / 2)
		}
	}
	return out
}

// compandedToMono converts 8-bit companded (mu-law or A-law) data to mono int16 samples.
func compandedToMono(data []byte, numChannels uint16, decode func(uint8) int16) []int16 {
	numFrames := len(data) / int(numChannels)
	out := make([]int16, numFrames)

	if numChannels == 1 {
		for i := 0; i < numFrames; i++ {
			out[i] = decode(data[i])
		}
	} else {
		for i := 0; i < numFrames; i++ {
			left := int32(decode(data[i*2]))
			right := int32(decode(data[i*2+1]))
			out[i] = int16((left + right) / 2)
		}
	}
	return out
}

// applyVolume scales PCM samples in-place by a gain factor derived from volume level.
// Each unit represents ~3dB. Volume 0 = no change.
func applyVolume(samples []int16, volume int) {
	if volume == 0 {
		return
	}
	gain := math.Pow(10, float64(volume)*0.15) // 3dB per step
	for i, s := range samples {
		v := int32(float64(s) * gain)
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		samples[i] = int16(v)
	}
}

// resampleLinear performs linear interpolation to convert between sample rates.
func resampleLinear(samples []int16, srcRate, dstRate uint32) []int16 {
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
			s0 := int32(samples[idx])
			s1 := int32(samples[idx+1])
			out[i] = int16(s0 + int32(float64(s1-s0)*frac))
		} else if idx < len(samples) {
			out[i] = samples[idx]
		}
	}

	return out
}
