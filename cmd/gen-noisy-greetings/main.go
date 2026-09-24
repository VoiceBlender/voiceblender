// Command gen-noisy-greetings builds AMD test fixtures by mixing the clean
// human greetings with real background noise, so answering-machine detection
// can be measured on the kind of audio a noisy trunk actually delivers.
//
// The clean corpus proves AMD works; it cannot show whether denoise helps,
// because there is nothing to remove. These fixtures close that gap.
//
// Usage:
//
//	go run ./cmd/gen-noisy-greetings -noise tests/data/noise
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/VoiceBlender/voiceblender/internal/mixer"
	mp3 "github.com/hajimehoshi/go-mp3"
)

const (
	sampleRate    = 16000
	bitsPerSample = 16
	numChannels   = 1

	// Noise recordings often open and close quietly — a room settling, a fade —
	// so a margin at each end is skipped and segments are drawn from the middle,
	// which is the whole point of the fixture. The margin is capped in seconds
	// rather than taken as a fraction throughout: on a recording of a minute or
	// less, discarding a fifth of each end leaves too little to draw varied
	// segments from, and the level guard below is what actually keeps a segment
	// off a quiet patch.
	edgeMarginFrac = 0.2
	maxEdgeMargin  = 3 * sampleRate

	// A segment whose level is far below the file's own average is a gap in
	// the recording; redraw rather than mix near-silence and call it noise.
	minSegmentRatio = 0.5
	maxDraws        = 25
)

func main() {
	humanDir := flag.String("human", "tests/data/greetings/human", "directory of clean human greetings")
	noiseDir := flag.String("noise", "tests/data/noise", "directory of background-noise recordings (.wav or .mp3)")
	outRoot := flag.String("out", "tests/data/greetings", "root to write the noisy corpora under")
	snrList := flag.String("snr", "15,5", "comma-separated SNRs in dB, relative to speech level")
	seed := flag.Int64("seed", 1, "random seed, so a corpus can be reproduced")
	flag.Parse()

	snrs, err := parseSNRs(*snrList)
	if err != nil {
		log.Fatalf("-snr: %v", err)
	}
	humans, err := wavsIn(*humanDir)
	if err != nil {
		log.Fatalf("human greetings: %v", err)
	}
	if len(humans) == 0 {
		log.Fatalf("no .wav files in %s — run 'make gen-human-greetings' first", *humanDir)
	}
	noises, err := noiseFilesIn(*noiseDir)
	if err != nil {
		log.Fatalf("noise: %v", err)
	}
	if len(noises) == 0 {
		log.Fatalf("no .wav or .mp3 files in %s", *noiseDir)
	}

	rng := rand.New(rand.NewSource(*seed))
	for _, noisePath := range noises {
		nf, err := openNoise(noisePath)
		if err != nil {
			log.Fatalf("%s: %v", noisePath, err)
		}
		label := strings.TrimSuffix(filepath.Base(noisePath), filepath.Ext(noisePath))
		log.Printf("noise %-14s %.0f s @ %d Hz, overall RMS %.0f",
			label, nf.Duration().Seconds(), nf.rate, nf.overallRMS)

		for _, snr := range snrs {
			outDir := filepath.Join(*outRoot, fmt.Sprintf("human-%s-%gdb", label, snr))
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				log.Fatal(err)
			}
			for _, h := range humans {
				if err := mixOne(h, nf, snr, outDir, rng); err != nil {
					log.Fatalf("%s: %v", filepath.Base(h), err)
				}
			}
			log.Printf("  wrote %d files at %+g dB SNR -> %s", len(humans), snr, outDir)
		}
		nf.Close()
	}
}

func mixOne(humanPath string, nf *wavFile, snrDB float64, outDir string, rng *rand.Rand) error {
	speech, rate, err := readWAV(humanPath)
	if err != nil {
		return err
	}
	if rate != nf.rate {
		return fmt.Errorf("sample rate %d does not match the noise's %d", rate, nf.rate)
	}

	noise, _, err := nf.drawSegment(len(speech), rng)
	if err != nil {
		return err
	}

	// Scale the noise so it sits snrDB below the speech's *active* level.
	// Measuring against the whole file would let the leading and trailing
	// silence drag the reference down and make every mix louder than asked.
	speechRMS := activeRMS(speech)
	noiseRMS := rms(noise)
	if speechRMS == 0 || noiseRMS == 0 {
		return fmt.Errorf("silent input")
	}
	gain := (speechRMS / math.Pow(10, snrDB/20)) / noiseRMS

	out := make([]int16, len(speech))
	for i := range speech {
		out[i] = clamp(float64(speech[i]) + float64(noise[i])*gain)
	}

	name := fmt.Sprintf("%s.wav", strings.TrimSuffix(filepath.Base(humanPath), filepath.Ext(humanPath)))
	return writeWAV(filepath.Join(outDir, name), out, rate)
}

// wavFile is one background recording, read as 16-bit mono PCM at the working
// rate. A WAV that already matches is read by seeking, so an hour-long
// recording does not have to be held in memory; anything needing conversion
// (MP3, stereo, another rate) is decoded once into pcm instead.
type wavFile struct {
	f          *os.File
	pcm        []int16 // non-nil when the recording is held in memory
	rate       int
	dataOff    int64
	dataLen    int64
	overallRMS float64
}

func (w *wavFile) Close() error {
	if w.f == nil {
		return nil
	}
	return w.f.Close()
}

func (w *wavFile) Duration() duration {
	return duration(float64(w.samples()) / float64(w.rate))
}

type duration float64

func (d duration) Seconds() float64 { return float64(d) }

func (w *wavFile) samples() int64 {
	if w.pcm != nil {
		return int64(len(w.pcm))
	}
	return w.dataLen / 2
}

// drawSegment picks n samples from the middle of the recording, retrying until
// the segment actually carries noise rather than landing in a quiet gap.
func (w *wavFile) drawSegment(n int, rng *rand.Rand) ([]int16, int64, error) {
	total := w.samples()
	margin := int64(float64(total) * edgeMarginFrac)
	if margin > maxEdgeMargin {
		margin = maxEdgeMargin
	}
	lo := margin
	hi := total - margin - int64(n)
	if hi <= lo {
		return nil, 0, fmt.Errorf("noise file is too short for a %d-sample segment", n)
	}
	for draw := 0; draw < maxDraws; draw++ {
		off := lo + rng.Int63n(hi-lo)
		seg, err := w.readAt(off, n)
		if err != nil {
			return nil, 0, err
		}
		if rms(seg) >= w.overallRMS*minSegmentRatio {
			return seg, off, nil
		}
	}
	return nil, 0, fmt.Errorf("no segment with enough noise found after %d draws", maxDraws)
}

func (w *wavFile) readAt(sampleOff int64, n int) ([]int16, error) {
	if w.pcm != nil {
		if sampleOff < 0 || sampleOff+int64(n) > int64(len(w.pcm)) {
			return nil, io.ErrUnexpectedEOF
		}
		return w.pcm[sampleOff : sampleOff+int64(n)], nil
	}
	buf := make([]byte, n*2)
	if _, err := w.f.ReadAt(buf, w.dataOff+sampleOff*2); err != nil {
		return nil, err
	}
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
	}
	return out, nil
}

// openNoise loads a background recording as 16-bit mono PCM at the working
// rate. A WAV already in that shape keeps the seek-backed path; everything else
// is decoded, downmixed and resampled once into memory.
func openNoise(path string) (*wavFile, error) {
	if strings.ToLower(filepath.Ext(path)) == ".mp3" {
		return openMP3(path)
	}
	w, err := openWAV(path)
	if err != nil {
		return nil, err
	}
	if w.rate == sampleRate {
		return w, nil
	}
	// A WAV at another rate cannot be read by seeking into 16 kHz samples, so
	// it is converted the same way an MP3 is.
	defer w.Close()
	pcm, err := w.readAt(0, int(w.samples()))
	if err != nil {
		return nil, err
	}
	return inMemoryNoise(resampleTo(pcm, w.rate, sampleRate)), nil
}

// openMP3 decodes an MP3 to mono PCM at the working rate. go-mp3 always emits
// 16-bit stereo, so the channels are averaged rather than one being dropped:
// background recordings are rarely identical across the pair, and taking one
// side alone throws away half the noise.
func openMP3(path string) (*wavFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec, err := mp3.NewDecoder(f)
	if err != nil {
		return nil, fmt.Errorf("mp3 decode: %w", err)
	}
	raw, err := io.ReadAll(dec)
	if err != nil {
		return nil, fmt.Errorf("read mp3: %w", err)
	}
	const bytesPerStereoFrame = 4
	mono := make([]int16, len(raw)/bytesPerStereoFrame)
	for i := range mono {
		l := int16(binary.LittleEndian.Uint16(raw[i*bytesPerStereoFrame:]))
		r := int16(binary.LittleEndian.Uint16(raw[i*bytesPerStereoFrame+2:]))
		mono[i] = int16((int32(l) + int32(r)) / 2)
	}
	if len(mono) == 0 {
		return nil, fmt.Errorf("decoded to no audio")
	}
	return inMemoryNoise(resampleTo(mono, dec.SampleRate(), sampleRate)), nil
}

func inMemoryNoise(pcm []int16) *wavFile {
	w := &wavFile{pcm: pcm, rate: sampleRate}
	w.overallRMS = w.sampleRMS()
	return w
}

func resampleTo(pcm []int16, srcRate, dstRate int) []int16 {
	if srcRate == dstRate {
		return pcm
	}
	return mixer.NewPCMResampler(srcRate, dstRate).ResampleSamples(pcm)
}

func openWAV(path string) (*wavFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 4096)
	n, err := f.Read(head)
	if err != nil {
		f.Close()
		return nil, err
	}
	head = head[:n]
	if len(head) < 12 || string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		f.Close()
		return nil, fmt.Errorf("not a RIFF/WAVE file")
	}
	w := &wavFile{f: f}
	for i := 12; i+8 <= len(head); {
		id := string(head[i : i+4])
		sz := int64(binary.LittleEndian.Uint32(head[i+4 : i+8]))
		switch id {
		case "fmt ":
			if binary.LittleEndian.Uint16(head[i+10:i+12]) != numChannels {
				f.Close()
				return nil, fmt.Errorf("want mono")
			}
			if binary.LittleEndian.Uint16(head[i+22:i+24]) != bitsPerSample {
				f.Close()
				return nil, fmt.Errorf("want 16-bit")
			}
			w.rate = int(binary.LittleEndian.Uint32(head[i+12 : i+16]))
		case "data":
			w.dataOff = int64(i + 8)
			w.dataLen = sz
			if st, err := f.Stat(); err == nil && w.dataOff+w.dataLen > st.Size() {
				w.dataLen = st.Size() - w.dataOff
			}
			w.overallRMS = w.sampleRMS()
			return w, nil
		}
		i += 8 + int(sz) + int(sz&1)
	}
	f.Close()
	return nil, fmt.Errorf("no data chunk in the first %d bytes", len(head))
}

// sampleRMS estimates the recording's overall level from spread-out probes,
// which is the reference a segment is judged against.
func (w *wavFile) sampleRMS() float64 {
	const probes, probeLen = 40, 16000
	total := w.samples()
	if total < probeLen {
		return 0
	}
	var sum float64
	var used int
	for i := 0; i < probes; i++ {
		off := total * int64(i) / int64(probes)
		if off+probeLen > total {
			break
		}
		seg, err := w.readAt(off, probeLen)
		if err != nil {
			break
		}
		sum += rms(seg) * rms(seg)
		used++
	}
	if used == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(used))
}

func rms(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

// activeRMS is the level over the loud frames only, so leading and trailing
// silence does not drag the speech reference down.
func activeRMS(s []int16) float64 {
	const frame = sampleRate / 50
	var frames []float64
	peak := 0.0
	for off := 0; off+frame <= len(s); off += frame {
		r := rms(s[off : off+frame])
		frames = append(frames, r)
		peak = math.Max(peak, r)
	}
	var sum float64
	var n int
	for _, r := range frames {
		if r >= 0.1*peak {
			sum += r * r
			n++
		}
	}
	if n == 0 {
		return rms(s)
	}
	return math.Sqrt(sum / float64(n))
}

func clamp(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(math.Round(v))
}

func wavsIn(dir string) ([]string, error) { return filesIn(dir, ".wav") }

// noiseFilesIn lists the background recordings. MP3 is accepted alongside WAV
// because that is the form most usable field recordings arrive in, and
// converting them by hand is a step that gets skipped.
func noiseFilesIn(dir string) ([]string, error) { return filesIn(dir, ".wav", ".mp3") }

func filesIn(dir string, exts ...string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		for _, want := range exts {
			if ext == want {
				out = append(out, filepath.Join(dir, e.Name()))
				break
			}
		}
	}
	return out, nil
}

func parseSNRs(s string) ([]float64, error) {
	var out []float64
	for _, tok := range strings.Split(s, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(tok), 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}

func readWAV(path string) ([]int16, int, error) {
	w, err := openWAV(path)
	if err != nil {
		return nil, 0, err
	}
	defer w.Close()
	s, err := w.readAt(0, int(w.samples()))
	return s, w.rate, err
}

func writeWAV(path string, samples []int16, rate int) error {
	data := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], numChannels)
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*numChannels*bitsPerSample/8))
	binary.LittleEndian.PutUint16(h[32:], numChannels*bitsPerSample/8)
	binary.LittleEndian.PutUint16(h[34:], bitsPerSample)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	return os.WriteFile(path, append(h, data...), 0o644)
}
