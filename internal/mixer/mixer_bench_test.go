package mixer

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"testing"
)

// BenchmarkMixTick measures the per-tick CPU cost of the mix loop at different
// sample rates and participant counts. Since mixTick runs on a 20 ms ticker in
// production, ns/op divided by 20_000_000 gives the fraction of a CPU core
// used per room.
func BenchmarkMixTick(b *testing.B) {
	rates := []int{8000, 16000, 48000}
	counts := []int{2, 4, 8}

	for _, rate := range rates {
		for _, n := range counts {
			name := fmt.Sprintf("rate=%d/parts=%d", rate, n)
			b.Run(name, func(b *testing.B) {
				runMixTickBench(b, rate, n)
			})
		}
	}
}

func runMixTickBench(b *testing.B, sampleRate, partCount int) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(log, sampleRate)

	frame := makeToneFrame(m.samplesPerFrame, sampleRate)

	participants := make([]*Participant, partCount)
	for i := 0; i < partCount; i++ {
		id := fmt.Sprintf("p%d", i)
		gw := &guardedWriter{w: io.Discard}
		p := &Participant{
			ID:       id,
			Writer:   gw,
			incoming: make(chan []byte, 4),
			outgoing: make(chan []byte, 4),
			inject:   make(chan []byte, 4),
			done:     make(chan struct{}),
			guard:    gw,
		}
		m.participants[id] = p
		participants[i] = p
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, p := range participants {
			select {
			case p.incoming <- frame:
			default:
			}
		}
		m.mixTick()
		for _, p := range participants {
			select {
			case <-p.outgoing:
			default:
			}
		}
	}
}

// makeToneFrame produces a single 20 ms frame of a 1 kHz sine tone at the
// given sample rate. Non-silent input keeps the comfort-noise branch cold and
// exercises clamp16 on realistic amplitudes.
func makeToneFrame(samples, sampleRate int) []byte {
	buf := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		v := int16(math.Sin(2*math.Pi*1000*float64(i)/float64(sampleRate)) * 0.5 * math.MaxInt16)
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	return buf
}
