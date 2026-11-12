package leg

import (
	"bytes"
	"log/slog"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/thesyncim/gopus"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/jitter"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// voicedFrame is 20 ms of a speech-like signal: four harmonics of 110 Hz under
// a slow swell, so no two pitch periods are quite the same.
func voicedFrame(rate, f int) []int16 {
	out := make([]int16, rate/50)
	for i := range out {
		t := float64(f*len(out)+i) / float64(rate)
		var v float64
		for h := 1; h <= 4; h++ {
			v += math.Sin(2*math.Pi*110*float64(h)*t+float64(h)) / float64(h*h)
		}
		out[i] = int16(5000 * (1 + 0.3*math.Sin(2*math.Pi*3*t)) * v)
	}
	return out
}

func largestStep(pcm []int16) int {
	var m int
	for i := 1; i < len(pcm); i++ {
		d := int(pcm[i]) - int(pcm[i-1])
		m = max(m, d, -d)
	}
	return m
}

func level(pcm []int16) float64 {
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

type lossFixture struct {
	payloads [][]byte  // one packet per 20 ms
	clean    [][]int16 // what a decoder with no loss makes of them
	spf      uint32
}

func newLossFixture(t *testing.T, ct codec.CodecType, frames int) *lossFixture {
	t.Helper()
	enc, err := codec.NewEncoder(ct)
	if err != nil {
		t.Fatal(err)
	}
	return encodeLossFixture(t, ct, frames, enc.Encode)
}

func encodeLossFixture(t *testing.T, ct codec.CodecType, frames int, encode func([]int16) ([]byte, error)) *lossFixture {
	t.Helper()
	fx := &lossFixture{spf: uint32(ct.ClockRate() / 50)}
	ref := plainDecoder(t, ct)
	for f := range frames {
		payload, err := encode(voicedFrame(ct.SampleRate(), f))
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := ref.Decode(payload)
		if err != nil {
			t.Fatal(err)
		}
		fx.payloads = append(fx.payloads, slices.Clone(payload))
		fx.clean = append(fx.clean, slices.Clone(pcm))
	}
	return fx
}

// plainDecoder is the decoder as it was before loss concealment.
func plainDecoder(t *testing.T, ct codec.CodecType) codec.Decoder {
	t.Helper()
	switch ct {
	case codec.CodecPCMU:
		return &codec.PCMUDecoder{}
	case codec.CodecPCMA:
		return &codec.PCMADecoder{}
	}
	dec, err := codec.NewDecoder(ct)
	if err != nil {
		t.Fatal(err)
	}
	return dec
}

// throughGapFiller decodes the fixture the way readLoop does without a jitter
// buffer, with the packets for which lost is true missing.
func (fx *lossFixture) throughGapFiller(t *testing.T, ct codec.CodecType, lost func(f int) bool) [][]int16 {
	t.Helper()
	dec, err := codec.NewDecoder(ct)
	if err != nil {
		t.Fatal(err)
	}
	g := newGapFiller(dec, fx.spf)
	if g == nil {
		t.Fatalf("no gap filler for %s", ct)
	}
	var out [][]int16
	last := -1
	emit := func(seq uint16, pcm []int16) {
		// Frames, filled or decoded, come out in sequence number order.
		if int(seq) <= last {
			t.Fatalf("frame %d came out with sequence number %d after %d", len(out), seq, last)
		}
		last = int(seq)
		out = append(out, slices.Clone(pcm))
	}
	var decoded []int16
	for f, payload := range fx.payloads {
		if lost(f) {
			continue
		}
		ts := uint32(f) * fx.spf
		if g.before(1, ts, uint16(f), payload, emit) {
			t.Fatalf("packet %d dropped", f)
		}
		decoded, err = codec.DecodeInto(dec, decoded, payload)
		if err != nil {
			t.Fatal(err)
		}
		g.after(1, ts, len(decoded), time.Now())
		emit(uint16(f), decoded)
	}
	return out
}

func lostFrames(frames ...int) func(int) bool {
	return func(f int) bool { return slices.Contains(frames, f) }
}

// checkGap checks the output around a gap of n frames that starts at frame
// from: no step larger than bound, and a level that starts near the signal's
// and does not rise.
func checkGap(t *testing.T, out, clean [][]int16, from, n, bound int) {
	t.Helper()
	around := slices.Concat(out[from-1 : from+n+1]...)
	if got := largestStep(around); got > bound {
		t.Errorf("gap of %d at frame %d: largest step %d, bound %d", n, from, got, bound)
	}
	ref := level(clean[from-1])
	prev := math.Inf(1)
	for f := from; f < from+n; f++ {
		got := level(out[f])
		if f == from && got < 0.5*ref {
			t.Errorf("gap of %d at frame %d: first concealed frame at %.0f rms, the signal was at %.0f", n, from, got, ref)
		}
		if got >= prev {
			t.Errorf("gap of %d at frame %d: level rose from %.0f to %.0f", n, from, prev, got)
		}
		prev = got
	}
}

// A lost packet and a burst of three are concealed on a PCMU and a PCMA leg:
// one frame comes out for every 20 ms, with no step at either edge of a gap
// larger than 1.5 times the largest step in the signal (plus the companding
// error), and the rest of the stream is what the plain decoder gives.
func TestGapFillerConcealsG711Loss(t *testing.T) {
	for _, ct := range []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA} {
		t.Run(ct.String(), func(t *testing.T) {
			fx := newLossFixture(t, ct, 60)
			out := fx.throughGapFiller(t, ct, lostFrames(19, 40, 41, 42))
			if len(out) != len(fx.clean) {
				t.Fatalf("%d frames out for %d intervals", len(out), len(fx.clean))
			}
			bound := largestStep(slices.Concat(fx.clean...))*3/2 + 200
			checkGap(t, out, fx.clean, 19, 1, bound)
			checkGap(t, out, fx.clean, 40, 3, bound)

			// Outside the gaps and the frame after each, nothing changed.
			for f := range out {
				if (f >= 19 && f <= 20) || (f >= 40 && f <= 43) {
					continue
				}
				if !slices.Equal(out[f], fx.clean[f]) {
					t.Fatalf("frame %d differs from the plain decoder", f)
				}
			}
			// The frame after a gap is cross-faded for at most 10 ms.
			for _, f := range []int{20, 43} {
				if !slices.Equal(out[f][80:], fx.clean[f][80:]) {
					t.Errorf("frame %d differs from the plain decoder after its first 10 ms", f)
				}
			}
		})
	}
}

func TestGapFillerConcealsOpusLoss(t *testing.T) {
	fx := newLossFixture(t, codec.CodecOpus, 60)
	out := fx.throughGapFiller(t, codec.CodecOpus, lostFrames(19, 40, 41, 42))
	if len(out) != len(fx.clean) {
		t.Fatalf("%d frames out for %d intervals", len(out), len(fx.clean))
	}
	// The encoder's first frames ramp up, so the signal is measured after them.
	bound := largestStep(slices.Concat(fx.clean[5:]...)) * 3 / 2
	checkGap(t, out, fx.clean, 19, 1, bound)
	checkGap(t, out, fx.clean, 40, 3, bound)

	// Three frames after a gap the decoder is back on the signal.
	for _, f := range []int{23, 46} {
		var errSum float64
		for i, s := range out[f] {
			d := float64(s) - float64(fx.clean[f][i])
			errSum += d * d
		}
		if e, ref := math.Sqrt(errSum/float64(len(out[f]))), level(fx.clean[f]); e > 0.05*ref {
			t.Errorf("frame %d is %.0f rms away from the signal (%.0f rms)", f, e, ref)
		}
	}
}

// With no packet lost, a leg sounds exactly as it did before: every frame is
// what the plain decoder gives, and no frame is added.
func TestGapFillerLeavesAnUnbrokenStreamAlone(t *testing.T) {
	for _, ct := range []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA, codec.CodecOpus} {
		fx := newLossFixture(t, ct, 100)
		out := fx.throughGapFiller(t, ct, lostFrames())
		if len(out) != len(fx.clean) {
			t.Fatalf("%s: %d frames out for %d packets", ct, len(out), len(fx.clean))
		}
		for f := range out {
			if !slices.Equal(out[f], fx.clean[f]) {
				t.Fatalf("%s: frame %d differs from the plain decoder", ct, f)
			}
		}
	}
}

// A hole longer than gapMaxBurst is not concealed, and the frame after it is
// left alone.
func TestGapFillerLeavesALongHoleAlone(t *testing.T) {
	fx := newLossFixture(t, codec.CodecPCMU, 40)
	out := fx.throughGapFiller(t, codec.CodecPCMU, lostFrames(20, 21, 22, 23))
	if want := len(fx.clean) - 4; len(out) != want {
		t.Fatalf("%d frames out, want %d", len(out), want)
	}
	if !slices.Equal(out[20], fx.clean[24]) {
		t.Error("the frame after a long hole was changed")
	}
}

// When the packet after a lost one carries in-band redundancy, the lost frame
// is rebuilt from it.
func TestGapFillerUsesOpusRedundancy(t *testing.T) {
	enc, err := gopus.NewEncoder(gopus.EncoderConfig{SampleRate: 48000, Channels: 1, Application: gopus.ApplicationVoIP})
	if err != nil {
		t.Fatal(err)
	}
	enc.SetFEC(true)
	if err := enc.SetPacketLoss(20); err != nil {
		t.Fatal(err)
	}
	if err := enc.SetBitrate(24000); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4000)
	fx := encodeLossFixture(t, codec.CodecOpus, 40, func(pcm []int16) ([]byte, error) {
		n, err := enc.EncodeInt16(pcm, buf)
		return buf[:n], err
	})
	out := fx.throughGapFiller(t, codec.CodecOpus, lostFrames(20))
	if len(out) != len(fx.clean) {
		t.Fatalf("%d frames out for %d intervals", len(out), len(fx.clean))
	}
	var errSum float64
	for i, s := range out[20] {
		d := float64(s) - float64(fx.clean[20][i])
		errSum += d * d
	}
	if e, ref := math.Sqrt(errSum/float64(len(out[20]))), level(fx.clean[20]); e > 0.08*ref {
		t.Errorf("rebuilt frame is %.0f rms away from the lost one (%.0f rms)", e, ref)
	}
}

// newJitterStream is a stream with a jitter buffer and no loops running, to
// drive what popLoop does one tick at a time.
func newJitterStream(ct codec.CodecType, targetMs int) *mediaStream {
	l := newTestSIPLeg(ct)
	s := l.prim
	s.liveCodec.Store(snapshotCodec(s, nil, nil))
	s.jb = jitter.NewMs(targetMs, 300, 20)
	return s
}

// playDepth is the jitter buffer depth the playout tests run with, in frames.
// A burst longer than the buffer makes it wait for the frames that follow, so
// the tests keep their bursts shorter.
const playDepth = 4

// playTicks runs a jitter buffer and the playout concealer for a number of
// 20 ms ticks. On each tick the packet arrive returns, if any, is pushed and
// one frame is played.
func playTicks(ct codec.CodecType, ticks int, arrive func(tick int) (seq uint16, pcm []int16)) (out [][]int16) {
	s := newJitterStream(ct, playDepth*20)
	var plc playoutConcealer
	for tick := range ticks {
		if seq, pcm := arrive(tick); pcm != nil {
			s.jb.Push(seq, samplesToBytes(pcm))
		}
		out = append(out, bytesToSamples(plc.pop(s)))
	}
	return out
}

// throughJitterBuffer plays frames, one arriving per tick unless it is lost,
// and returns what was played for each.
func throughJitterBuffer(ct codec.CodecType, frames [][]int16, lost func(f int) bool) [][]int16 {
	const fill = playDepth - 1
	out := playTicks(ct, len(frames)+fill, func(tick int) (uint16, []int16) {
		if tick >= len(frames) || lost(tick) {
			return 0, nil
		}
		return uint16(tick), frames[tick]
	})
	// The first ticks only fill the buffer.
	return out[fill:]
}

func cleanFrames(rate, n int) [][]int16 {
	frames := make([][]int16, n)
	for f := range frames {
		frames[f] = voicedFrame(rate, f)
	}
	return frames
}

// With a jitter buffer, a lost packet and a burst of three are concealed when
// their turn to play comes, at 8 kHz (G.711) and at 48 kHz (Opus).
func TestPlayoutConcealsLoss(t *testing.T) {
	for _, ct := range []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA, codec.CodecOpus} {
		t.Run(ct.String(), func(t *testing.T) {
			clean := cleanFrames(ct.SampleRate(), 60)
			out := throughJitterBuffer(ct, clean, lostFrames(19, 40, 41, 42))
			bound := largestStep(slices.Concat(clean...)) * 3 / 2
			checkGap(t, out, clean, 19, 1, bound)
			checkGap(t, out, clean, 40, 3, bound)

			tenMs := ct.SampleRate() / 100
			for f := range out {
				switch {
				case f == 19 || (f >= 40 && f <= 42):
				case f == 20 || f == 43:
					if !slices.Equal(out[f][tenMs:], clean[f][tenMs:]) {
						t.Errorf("frame %d differs from the signal after its first 10 ms", f)
					}
				default:
					if !slices.Equal(out[f], clean[f]) {
						t.Fatalf("frame %d differs from the signal", f)
					}
				}
			}
		})
	}
}

// With no packet lost, a jitter-buffered leg plays exactly the bytes it was
// given.
func TestPlayoutLeavesAnUnbrokenStreamAlone(t *testing.T) {
	for _, ct := range []codec.CodecType{codec.CodecPCMU, codec.CodecOpus} {
		clean := cleanFrames(ct.SampleRate(), 100)
		out := throughJitterBuffer(ct, clean, lostFrames())
		for f := range clean {
			if !slices.Equal(out[f], clean[f]) {
				t.Fatalf("%s: frame %d was changed", ct, f)
			}
		}
	}
}

// A stream that stops fades out within 60 ms and then plays silence. When it
// starts again its first frame is not touched.
func TestPlayoutFadesToSilenceWhenTheStreamStops(t *testing.T) {
	const ct = codec.CodecPCMU
	clean := cleanFrames(ct.SampleRate(), 40)
	// The peer sends frames 0 to 19, nothing for ten ticks, then frames 30
	// to 39 on the next sequence numbers, as after a hold.
	out := playTicks(ct, 40, func(tick int) (uint16, []int16) {
		switch {
		case tick < 20:
			return uint16(tick), clean[tick]
		case tick < 30:
			return 0, nil
		}
		return uint16(tick - 10), clean[tick]
	})

	// The buffered frames run out on tick 22. Ticks 23 to 29 have nothing to
	// play.
	const last = 20 + playDepth - 2
	if !slices.Equal(out[last], clean[19]) {
		t.Fatalf("tick %d does not play the last frame sent", last)
	}
	stop := slices.Concat(out[last : last+4]...)
	if got, bound := largestStep(stop), largestStep(slices.Concat(clean...))*3/2; got > bound {
		t.Errorf("largest step at the stop %d, bound %d", got, bound)
	}
	if got, ref := level(out[last+3]), level(clean[19]); got > 0.25*ref {
		t.Errorf("40 to 60 ms into the stop the level is %.0f of %.0f", got, ref)
	}
	silence := make([]int16, len(clean[0]))
	for tick := last + 4; tick < 30; tick++ {
		if !slices.Equal(out[tick], silence) {
			t.Fatalf("tick %d, more than 60 ms into the stop, is not silence", tick)
		}
	}
	for tick := 30; tick < 40; tick++ {
		if !slices.Equal(out[tick], clean[tick]) {
			t.Fatalf("frame %d after the stop was changed", tick)
		}
	}
}

// Before the first packet, and for a codec the playout concealer does not
// cover, a tick with nothing to play is silence, as before.
func TestPlayoutSilenceWhereNothingIsConcealed(t *testing.T) {
	s := newJitterStream(codec.CodecPCMU, playDepth*20)
	var plc playoutConcealer
	if pcm := plc.pop(s); !bytes.Equal(pcm, make([]byte, 320)) {
		t.Error("the tick before the first packet is not 20 ms of silence")
	}

	clean := cleanFrames(16000, 30)
	out := throughJitterBuffer(codec.CodecG722, clean, lostFrames(20))
	if !slices.Equal(out[20], make([]int16, 320)) {
		t.Error("a lost G.722 frame is not silence")
	}
	if !slices.Equal(out[21], clean[21]) {
		t.Error("the G.722 frame after a lost one was changed")
	}
}

func newLoopLeg(t *testing.T, ct codec.CodecType, pt uint8, sess *sipmod.RTPSession, jitterMs int) *SIPLeg {
	t.Helper()
	l := newTestSIPLeg(ct)
	t.Cleanup(l.cancel)
	l.log = slog.New(slog.DiscardHandler)
	l.prim.rtpSess = sess
	l.prim.rtpPT = pt
	l.prim.negotiatedDir = sipmod.DirRecvOnly
	l.prim.jbTargetMs = jitterMs
	l.prim.jbMaxMs = 300
	l.setupStreamMedia(l.prim)
	return l
}

// TestReadLoopConcealsLoss: without a jitter buffer the leg delivers a frame
// for every 20 ms of the peer's stream, lost packets included.
func TestReadLoopConcealsLoss(t *testing.T) {
	for _, tc := range []struct {
		ct codec.CodecType
		pt uint8
	}{{codec.CodecPCMU, 0}, {codec.CodecOpus, 111}} {
		t.Run(tc.ct.String(), func(t *testing.T) {
			peer, local := newRTPPair(t)
			l := newLoopLeg(t, tc.ct, tc.pt, local, 0)
			fx := newLossFixture(t, tc.ct, 60)
			lost := lostFrames(19, 40, 41, 42)

			go func() {
				for f, payload := range fx.payloads {
					if lost(f) {
						continue
					}
					peer.WriteRTP(&rtp.Packet{
						Header:  rtp.Header{Version: 2, PayloadType: tc.pt, SequenceNumber: uint16(f), Timestamp: uint32(f) * fx.spf, SSRC: 1},
						Payload: payload,
					})
					// Slow enough for the reader below to keep up.
					time.Sleep(2 * time.Millisecond)
				}
			}()

			var out [][]int16
			timeout := time.After(5 * time.Second)
			for len(out) < len(fx.clean) {
				select {
				case pcm := <-l.prim.inFrames:
					out = append(out, bytesToSamples(pcm))
				case <-timeout:
					t.Fatalf("leg delivered %d frames for %d intervals", len(out), len(fx.clean))
				}
			}
			bound := largestStep(slices.Concat(fx.clean[5:]...))*3/2 + 200
			checkGap(t, out, fx.clean, 19, 1, bound)
			checkGap(t, out, fx.clean, 40, 3, bound)
		})
	}
}

// TestReadLoopFillsOpusLossIntoTheJitterBuffer: with a jitter buffer an Opus
// decoder is still told of each lost packet when the next one arrives, and
// what it makes of the loss takes the lost packet's place in the buffer.
// Without that the frame after a loss comes out more than half wrong. G.711
// has no decoder state to keep, so its lost packets stay empty slots, which a
// late packet can still take and which are concealed at playout.
func TestReadLoopFillsOpusLossIntoTheJitterBuffer(t *testing.T) {
	const frames, lostEvery = 100, 10
	for _, tc := range []struct {
		ct     codec.CodecType
		pt     uint8
		filled bool
	}{{codec.CodecOpus, 111, true}, {codec.CodecPCMU, 0, false}} {
		t.Run(tc.ct.String(), func(t *testing.T) {
			peer, local := newRTPPair(t)
			l := newTestSIPLeg(tc.ct)
			t.Cleanup(l.cancel)
			l.log = slog.New(slog.DiscardHandler)
			s := l.prim
			s.rtpSess, s.rtpPT = local, tc.pt
			enc, dec, err := l.buildCodecPair(s)
			if err != nil {
				t.Fatal(err)
			}
			s.liveCodec.Store(snapshotCodec(s, enc, dec))
			// Deep enough to hold the whole stream: nothing plays it out here.
			s.jb = jitter.New(3, 2*frames)
			go l.readLoop(s)

			fx := newLossFixture(t, tc.ct, frames)
			lost := func(f int) bool { return f%lostEvery == lostEvery-1 }
			want := 0
			for f, payload := range fx.payloads {
				if lost(f) {
					continue
				}
				want++
				// The packet after a lost one brings its fill with it.
				if tc.filled && f > 0 && lost(f-1) {
					want++
				}
				peer.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, PayloadType: tc.pt, SequenceNumber: uint16(f), Timestamp: uint32(f) * fx.spf, SSRC: 1},
					Payload: payload,
				})
				time.Sleep(time.Millisecond)
			}
			for deadline := time.Now().Add(3 * time.Second); s.jb.Len() < want; {
				if time.Now().After(deadline) {
					t.Fatalf("jitter buffer holds %d frames, want %d", s.jb.Len(), want)
				}
				time.Sleep(5 * time.Millisecond)
			}
			if got := s.jb.Len(); got != want {
				t.Fatalf("jitter buffer holds %d frames, want %d", got, want)
			}

			for f := 0; f < frames-1; f++ {
				pcm, ok := s.jb.Pop()
				switch {
				case lost(f) && ok != tc.filled:
					t.Fatalf("slot of lost packet %d: filled %v, want %v", f, ok, tc.filled)
				case !lost(f) && !ok:
					t.Fatalf("slot of packet %d is empty", f)
				case !ok:
					continue
				}
				got := bytesToSamples(pcm)
				if lost(f) && level(got) < 0.5*level(fx.clean[f]) {
					t.Errorf("fill for lost packet %d at %.0f rms, the signal was at %.0f", f, level(got), level(fx.clean[f]))
				}
				if f > 0 && lost(f-1) {
					var errSum float64
					for i, v := range got {
						d := float64(v) - float64(fx.clean[f][i])
						errSum += d * d
					}
					if e, ref := math.Sqrt(errSum/float64(len(got))), level(fx.clean[f]); e > 0.25*ref {
						t.Errorf("frame %d, after a lost packet, is %.0f rms away from the signal (%.0f rms)", f, e, ref)
					}
				}
			}
		})
	}
}

// TestPopLoopConcealsLoss: with a jitter buffer the leg plays concealment, not
// silence, for a lost packet.
func TestPopLoopConcealsLoss(t *testing.T) {
	peer, local := newRTPPair(t)
	l := newLoopLeg(t, codec.CodecPCMU, 0, local, 40)
	fx := newLossFixture(t, codec.CodecPCMU, 100)
	const lostEvery = 10

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for f, payload := range fx.payloads {
			<-ticker.C
			if f%lostEvery == lostEvery-1 {
				continue
			}
			peer.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: uint16(f), Timestamp: uint32(f) * fx.spf, SSRC: 1},
				Payload: payload,
			})
		}
	}()

	// Silent frames between the first and the last frame with audio.
	var silent, pending int
	started := false
	timeout := time.After(5 * time.Second)
	for running := true; running; {
		select {
		case pcm := <-l.prim.inFrames:
			switch {
			case level(bytesToSamples(pcm)) > 0:
				started = true
				silent += pending
				pending = 0
			case started:
				pending++
			}
		case <-done:
			running = false
		case <-timeout:
			t.Fatal("peer did not finish")
		}
	}
	if !started {
		t.Fatal("leg delivered no audio")
	}
	// Ten packets were lost. A slow test machine may add an underrun or two.
	if lostPackets := len(fx.payloads) / lostEvery; silent >= lostPackets/2 {
		t.Errorf("%d silent frames in the stream for %d lost packets", silent, lostPackets)
	}
}
