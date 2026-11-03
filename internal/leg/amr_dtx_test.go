package leg

import (
	"log/slog"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func newRTPPair(t *testing.T) (a, b *sipmod.RTPSession) {
	t.Helper()
	a, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatal(err)
	}
	b, err = sipmod.NewRTPSession()
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	if err := a.SetRemote("127.0.0.1", b.LocalPort()); err != nil {
		t.Fatal(err)
	}
	if err := b.SetRemote("127.0.0.1", a.LocalPort()); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func newAMRWBLoopLeg(t *testing.T, sess *sipmod.RTPSession, dir string) *SIPLeg {
	t.Helper()
	l := newTestSIPLeg(codec.CodecAMRWB)
	t.Cleanup(l.cancel)
	l.log = slog.New(slog.DiscardHandler)
	l.prim.rtpSess = sess
	l.prim.rtpPT = 96
	l.prim.amrwbMode = defaultAMRWBEncoderMode
	l.prim.amrwbOctetAligned = true
	l.prim.amrModeChangePeriod = 1
	l.prim.negotiatedDir = dir
	l.setupStreamMedia(l.prim)
	return l
}

func wbSpeechFrame() []byte {
	pcm := make([]byte, 640)
	for i := 0; i < 320; i++ {
		t := float64(i) / 16000
		v := int16(6000*math.Sin(2*math.Pi*300*t) + 3000*math.Sin(2*math.Pi*1100*t))
		pcm[2*i], pcm[2*i+1] = byte(v), byte(v>>8)
	}
	return pcm
}

// TestWriteLoopAMRDTX: with nothing to play the AMR encoder goes into DTX, so
// the leg sends far fewer than 50 packets a second while the RTP clock keeps
// running; speech afterwards starts a talkspurt with the marker bit.
func TestWriteLoopAMRDTX(t *testing.T) {
	tx, rx := newRTPPair(t)
	l := newAMRWBLoopLeg(t, tx, sipmod.DirSendOnly)

	var pkts []rtp.Packet
	read := func(until time.Time) {
		for time.Now().Before(until) {
			rx.SetReadDeadline(until)
			p, err := rx.ReadRTP()
			if err != nil {
				return
			}
			pkts = append(pkts, *p)
		}
	}

	read(time.Now().Add(2 * time.Second))
	silent := len(pkts)
	if silent == 0 || silent > 40 {
		t.Fatalf("%d packets in 2 s of silence, want DTX (well under 100)", silent)
	}

	speech := wbSpeechFrame()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case l.prim.outFrames <- speech:
			}
		}
	}()
	read(time.Now().Add(time.Second))
	close(stop)
	if len(pkts)-silent < 35 {
		t.Errorf("%d packets in 1 s of speech, want close to 50", len(pkts)-silent)
	}

	var jumped, marked bool
	for i := 1; i < len(pkts); i++ {
		if pkts[i].SequenceNumber != pkts[i-1].SequenceNumber+1 {
			t.Fatalf("sequence gap %d -> %d: DTX must not consume sequence numbers",
				pkts[i-1].SequenceNumber, pkts[i].SequenceNumber)
		}
		d := pkts[i].Timestamp - pkts[i-1].Timestamp
		if d%320 != 0 {
			t.Fatalf("timestamp step %d is not a whole number of frames", d)
		}
		if d > 320 {
			jumped = true
		}
		if i >= silent && pkts[i].Marker && d > 320 {
			marked = true
		}
	}
	if !jumped {
		t.Error("no timestamp jump: silent frames were not skipped")
	}
	if !marked {
		t.Error("no marker bit on the talkspurt that ended DTX")
	}
}

// TestReadLoopAMRComfortNoise: a DTX peer sends only SID updates during
// silence, and the leg still delivers one decoded frame per 20 ms.
func TestReadLoopAMRComfortNoise(t *testing.T) {
	peer, local := newRTPPair(t)
	l := newAMRWBLoopLeg(t, local, sipmod.DirRecvOnly)

	enc, _, err := codec.NewAMRWBSession(codec.AMRConfig{Mode: 8, OctetAligned: true})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, 1))
	frame := make([]int16, 320)
	noise := func() []int16 {
		for i := range frame {
			frame[i] = int16(300 * (2*rng.Float64() - 1))
		}
		return frame
	}
	// The encoder's VAD needs a few seconds of noise to settle into DTX.
	for range 300 {
		if _, err := enc.Encode(noise()); err != nil {
			t.Fatal(err)
		}
	}

	const frames = 100
	done := make(chan int)
	go func() {
		sent := 0
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var seq uint16
		for f := 0; f < frames; f++ {
			<-ticker.C
			payload, err := enc.Encode(noise())
			if err != nil || len(payload) == 0 {
				continue
			}
			peer.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(f * 320), SSRC: 1},
				Payload: payload,
			})
			seq++
			sent++
		}
		done <- sent
	}()

	var got, nonZero int
	timeout := time.After(frames*20*time.Millisecond + 500*time.Millisecond)
	var sent = -1
	for sent < 0 {
		select {
		case pcm := <-l.prim.inFrames:
			got++
			for _, b := range pcm {
				if b != 0 {
					nonZero++
					break
				}
			}
		case sent = <-done:
		case <-timeout:
			t.Fatal("peer did not finish")
		}
	}
	if sent > frames/2 {
		t.Fatalf("peer sent %d packets for %d frames; the fixture is not in DTX", sent, frames)
	}
	// Timer fills lag one frame behind the clock, and the last ones may not be
	// due yet when the peer stops.
	if got < frames-10 {
		t.Errorf("leg delivered %d frames for %d intervals (%d packets), want one per 20 ms", got, frames, sent)
	}
	if nonZero < got/2 {
		t.Errorf("only %d of %d delivered frames carry comfort noise", nonZero, got)
	}
}
