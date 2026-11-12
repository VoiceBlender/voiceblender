package leg

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

type frameCounter struct{ n atomic.Int64 }

func (c *frameCounter) Write(p []byte) (int, error) {
	c.n.Add(1)
	return len(p), nil
}

// TestReadLoopLossAndPlayoutDelay measures how late a PCMU leg's audio plays
// across lost packets, with the jitter buffer off. The consumer here does what
// the mixer does: one frame per tick, and nothing when the leg has none ready.
// It reads the leg's own queue, so the mixer holds nothing besides.
//
// The fill for a lost packet comes with the next packet, a tick after it was
// due. It is played only if a frame is still waiting at that point, which
// means the lost frame's tick is still to come. Otherwise the hole is passed
// over. A lost packet then never makes the audio after it later, and makes it
// a tick earlier when the one frame in hand had just been played.
func TestReadLoopLossAndPlayoutDelay(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inHand     int // frames the consumer has queued when it starts to play
		lost       []int
		wantSilent int // ticks with nothing to play
		wantFills  int // fills that were played
		wantAfter  int // ticks from a frame's arrival to its playout, after the loss
	}{
		{"one lost, none in hand", 0, []int{30}, 1, 0, 0},
		{"one lost, one in hand", 1, []int{30}, 0, 0, 0},
		{"one lost, two in hand", 2, []int{30}, 0, 1, 2},
		{"two lost, one in hand", 1, []int{30, 31}, 1, 0, 0},
		{"two lost, two in hand", 2, []int{30, 31}, 0, 0, 0},
		{"three lost, none in hand", 0, []int{30, 31, 32}, 3, 0, 0},
		{"four lost, not filled", 0, []int{30, 31, 32, 33}, 4, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const frames = 60
			peer, local := newRTPPair(t)
			l := newLoopLeg(t, codec.CodecPCMU, 0, local, 0)
			l.SetPlayoutQueued(func() int { return 0 })
			var tapped frameCounter
			l.mu.Lock()
			l.prim.inTap = &tapped
			l.mu.Unlock()
			fx := newLossFixture(t, codec.CodecPCMU, frames)
			lost := lostFrames(tc.lost...)

			// A packet of another payload type follows each audio packet. The
			// read loop counts it only once the audio packet is fully handled.
			var sent uint32
			arrive := func(f int) {
				t.Helper()
				for _, pkt := range []*rtp.Packet{
					{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: uint16(f), Timestamp: uint32(f) * fx.spf, SSRC: 1}, Payload: fx.payloads[f]},
					{Header: rtp.Header{Version: 2, PayloadType: 13, SSRC: 1}, Payload: []byte{0}},
				} {
					if err := peer.WriteRTP(pkt); err != nil {
						t.Fatal(err)
					}
					sent++
				}
				deadline := time.Now().Add(2 * time.Second)
				for {
					l.prim.rtpStatsMu.Lock()
					got := l.prim.rtpReceived
					l.prim.rtpStatsMu.Unlock()
					if got >= sent {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("leg read %d of %d packets", got, sent)
					}
					time.Sleep(50 * time.Microsecond)
				}
			}

			// The second half of a frame is never touched by concealment, so
			// it tells which packet a played frame came from. The fixture
			// repeats every 50 frames: only recent packets are candidates.
			source := func(pcm []int16, tick int) int {
				for f := min(tick, frames-1); f >= max(tick-8, 0); f-- {
					clean := fx.clean[f]
					if slices.Equal(pcm[len(pcm)/2:], clean[len(clean)/2:]) {
						return f
					}
				}
				return -1
			}

			playedAt := make([]int, frames)
			for f := range playedAt {
				playedAt[f] = -1
			}
			var silent, fills int
			for tick := 0; tick < frames+tc.inHand+2; tick++ {
				if tick < frames && !lost(tick) {
					arrive(tick)
				}
				if tick < tc.inHand {
					continue
				}
				select {
				case pcm := <-l.prim.inFrames:
					if f := source(bytesToSamples(pcm), tick); f >= 0 {
						playedAt[f] = tick
					} else {
						fills++
					}
				default:
					if tick < frames {
						silent++
					}
				}
			}

			first, last := tc.lost[0], tc.lost[len(tc.lost)-1]
			t.Logf("audio played %d ms after it arrived before the loss and %d ms after it, with %d silent ticks and %d fills",
				20*(playedAt[first-1]-(first-1)), 20*(playedAt[last+1]-(last+1)), silent, fills)
			if silent != tc.wantSilent {
				t.Errorf("%d ticks had nothing to play, want %d", silent, tc.wantSilent)
			}
			if fills != tc.wantFills {
				t.Errorf("%d fills were played, want %d", fills, tc.wantFills)
			}
			// A tap hears what is queued to play.
			if got, want := int(tapped.n.Load()), frames-len(tc.lost)+fills; got != want {
				t.Errorf("the tap got %d frames, want %d", got, want)
			}
			for f, at := range playedAt {
				switch {
				case lost(f):
					if at >= 0 {
						t.Errorf("lost frame %d was played", f)
					}
				case at < 0:
					t.Errorf("frame %d was never played", f)
				case f < first && at-f != tc.inHand:
					t.Errorf("before the loss, frame %d played %d ticks after it arrived, want %d", f, at-f, tc.inHand)
				case f > last && at-f != tc.wantAfter:
					t.Errorf("after the loss, frame %d played %d ticks after it arrived, want %d", f, at-f, tc.wantAfter)
				}
			}
		})
	}
}
