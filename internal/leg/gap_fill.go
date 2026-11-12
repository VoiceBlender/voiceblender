package leg

import (
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

const (
	gapFrame = 20 * time.Millisecond
	// gapSlack holds timer fills back behind the wall clock, so the peer's
	// next SID or speech packet normally lands before its interval is filled.
	gapSlack = 20 * time.Millisecond
	// gapMaxBurst caps the frames filled at once on a packet's arrival; a
	// longer hole is a discontinuity, not loss worth concealing.
	gapMaxBurst = 3
	// gapMaxFill bounds comfort noise without any packet: three missed SID
	// updates' worth, after which the peer is taken to have stopped sending.
	gapMaxFill = 25
)

// gapFiller keeps the decoded stream of a GapDecoder continuous. A DTX sender
// goes quiet between SID updates, and loss leaves holes anywhere: both are
// decoded as missing frames, comfort noise in DTX and concealment otherwise.
// Timer fills run only in DTX, where silence is announced; elsewhere a hole is
// filled once the next packet shows it.
type gapFiller struct {
	dec    codec.GapDecoder
	fec    codec.FECDecoder // nil when the decoder has no in-band redundancy
	spf    uint32           // RTP timestamp units per frame
	nextTS uint32           // timestamp of the next frame due
	due    time.Time
	filled int // timer fills since the last packet
	ssrc   uint32
	synced bool
	buf    []int16

	// queued is how many of this stream's frames wait to be played. nil, or
	// negative, when no mixer plays the stream.
	queued func() int
	// join is the substitute for the first frame of a hole that was passed
	// over. The frame after the hole is cross-faded in from it.
	join []int16
}

func newGapFiller(dec codec.Decoder, spf uint32) *gapFiller {
	gd, ok := dec.(codec.GapDecoder)
	if !ok {
		return nil
	}
	fec, _ := dec.(codec.FECDecoder)
	return &gapFiller{dec: gd, fec: fec, spf: spf}
}

// gapEmit takes a filled frame. seq is the sequence number of the lost packet
// it stands for, which a jitter buffer needs; timer fills have none.
type gapEmit func(seq uint16, pcm []int16)

// before runs ahead of decoding the packet with timestamp ts, sequence number
// seq and the given payload. It fills a short hole before it and reports
// whether the packet must be dropped because its interval was already filled
// (late, reordered or duplicate).
func (g *gapFiller) before(ssrc, ts uint32, seq uint16, payload []byte, emit gapEmit) (drop bool) {
	if !g.synced || ssrc != g.ssrc {
		g.synced = false
		g.join = g.join[:0]
		return false
	}
	gap := int32(ts-g.nextTS) / int32(g.spf)
	switch {
	case gap < 0 && gap >= -gapMaxFill:
		return true
	case gap > 0 && gap <= gapMaxBurst:
		// With none of the stream's frames waiting, the mixer has run the
		// lost frame's tick empty, or will before a fill reaches it. A fill
		// queued now would play a tick late and leave the stream 20 ms behind
		// for good. So the hole is passed over: the decoder is still told of
		// each lost frame, and the mixer conceals the tick if it runs empty.
		if g.queued != nil && g.queued() == 0 {
			emit = g.passOver
		} else {
			// What comes next follows the fills, not an earlier hole.
			g.join = g.join[:0]
		}
		for back := uint16(gap); back > 1; back-- {
			g.fill(seq-back, nil, emit)
		}
		// Only the frame just before the packet can be in its redundancy.
		g.fill(seq-1, payload, emit)
	case gap != 0:
		// A jump too long to fill starts the stream afresh.
		g.join = g.join[:0]
	}
	return false
}

// passOver takes the frames of a hole that is passed over. It keeps the first,
// which carries on from the frame before the hole, for joinTo.
func (g *gapFiller) passOver(_ uint16, pcm []int16) {
	if len(g.join) == 0 {
		g.join = append(g.join, pcm...)
	}
}

// joinTo cross-fades the first 4 ms of pcm, the frame after a hole that was
// passed over, in from the substitute for the hole's first frame, so the
// frames either side of the hole join with no step.
func (g *gapFiller) joinTo(pcm []int16) {
	n := min(len(pcm), len(g.join)) / 5
	for i := range n {
		w := float32(i+1) / float32(n+1)
		pcm[i] = int16(float32(g.join[i])*(1-w) + float32(pcm[i])*w)
	}
	g.join = g.join[:0]
}

// after records a packet with timestamp ts that decoded to n samples.
func (g *gapFiller) after(ssrc, ts uint32, n int, now time.Time) {
	g.ssrc = ssrc
	frames := uint32(n) / g.spf
	if frames == 0 {
		frames = 1
	}
	g.nextTS = ts + frames*g.spf
	g.due = now.Add(time.Duration(frames) * gapFrame)
	g.filled = 0
	g.synced = true
}

// deadline returns when the next comfort-noise frame is due, if one is.
func (g *gapFiller) deadline() (time.Time, bool) {
	if !g.synced || g.filled >= gapMaxFill || !g.dec.InDTX() {
		return time.Time{}, false
	}
	return g.due.Add(gapSlack), true
}

// tick fills every comfort-noise frame due by now.
func (g *gapFiller) tick(now time.Time, emit gapEmit) {
	for {
		at, ok := g.deadline()
		if !ok || now.Before(at) {
			return
		}
		g.fill(0, nil, emit)
		g.filled++
	}
}

// fill emits one missing frame. next is the payload of the packet that
// follows it, when that packet is in hand.
func (g *gapFiller) fill(seq uint16, next []byte, emit gapEmit) {
	var (
		pcm []int16
		err error
	)
	if g.fec != nil && next != nil {
		pcm, err = g.fec.DecodeFEC(g.buf, next)
	} else {
		pcm, err = g.dec.DecodeGap(g.buf)
	}
	g.nextTS += g.spf
	g.due = g.due.Add(gapFrame)
	if err != nil {
		return
	}
	g.buf = pcm
	emit(seq, pcm)
}
