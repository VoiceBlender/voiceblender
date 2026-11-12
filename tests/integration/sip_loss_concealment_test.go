//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// TestSIPLossConcealment calls an instance from a bare SIP client and sends it
// a tone over RTP with every tenth packet missing. The leg must turn that into
// continuous audio, with the jitter buffer off and on: the lost packets must
// not show as silence or as missing frames.
func TestSIPLossConcealment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ct       codec.CodecType
		pt       uint8
		rtpmap   string
		jitterMs int
	}{
		{"PCMU", codec.CodecPCMU, 0, "PCMU/8000", 0},
		{"PCMU-jitter-buffer", codec.CodecPCMU, 0, "PCMU/8000", 60},
		{"opus", codec.CodecOpus, 111, "opus/48000/2", 0},
		{"opus-jitter-buffer", codec.CodecOpus, 111, "opus/48000/2", 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := newTestInstanceFull(t, "plc-"+tc.name, func(c *config.Config) {
				c.SIPJitterBufferMs = tc.jitterMs
				c.SIPJitterBufferMaxMs = 300
			}, []codec.CodecType{tc.ct})

			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			recipient := sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}
			pt := strconv.Itoa(int(tc.pt))
			offer := sdpOffer("127.0.0.1", conn.LocalAddr().(*net.UDPAddr).Port, pt, pt+" "+tc.rtpmap)
			dialog, inbound := answerInboundCall(t, ctx, inst, recipient, offer)
			answer, err := sipmod.ParseSDP(dialog.InviteResponse.Body())
			if err != nil {
				t.Fatalf("parse answer: %v", err)
			}
			if err := dialog.Ack(ctx); err != nil {
				t.Fatalf("ACK: %v", err)
			}
			waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)
			defer hangup(t, inst, inbound.ID)

			rawLeg, ok := inst.legMgr.Get(inbound.ID)
			if !ok {
				t.Fatalf("inbound leg %s not found", inbound.ID)
			}
			sipLeg, ok := rawLeg.(*leg.SIPLeg)
			if !ok {
				t.Fatalf("inbound leg is %T, want *leg.SIPLeg", rawLeg)
			}

			// The leg is in no room, so this reader is the only consumer of
			// what its read path produces.
			rate := tc.ct.SampleRate()
			var (
				mu    sync.Mutex
				heard []int16
			)
			go func() {
				reader := sipLeg.AudioReader()
				buf := make([]byte, rate/50*2)
				for {
					if _, err := io.ReadFull(reader, buf); err != nil {
						return
					}
					mu.Lock()
					for i := 0; i < len(buf); i += 2 {
						heard = append(heard, int16(binary.LittleEndian.Uint16(buf[i:])))
					}
					mu.Unlock()
				}
			}()

			enc, err := codec.NewEncoder(tc.ct)
			if err != nil {
				t.Fatal(err)
			}
			const packets, lostEvery = 150, 10
			remote := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: answer.RemotePort}
			spf := uint32(tc.ct.ClockRate() / 50)
			frame := make([]int16, rate/50)
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for f := 0; f < packets; f++ {
				<-ticker.C
				for i := range frame {
					frame[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(f*len(frame)+i)/float64(rate)))
				}
				payload, err := enc.Encode(frame)
				if err != nil {
					t.Fatal(err)
				}
				// Lose every tenth packet, and two in a row once.
				if f%lostEvery == lostEvery-1 || f == 71 || f == 72 {
					continue
				}
				raw, err := (&rtp.Packet{
					Header:  rtp.Header{Version: 2, PayloadType: tc.pt, SequenceNumber: uint16(f), Timestamp: uint32(f) * spf, SSRC: 0x504c43},
					Payload: payload,
				}).Marshal()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := conn.WriteToUDP(raw, remote); err != nil {
					t.Fatal(err)
				}
			}
			// Let the last frames through the jitter buffer.
			time.Sleep(200 * time.Millisecond)
			mu.Lock()
			pcm := slices.Clone(heard)
			mu.Unlock()

			// 5 ms blocks, from the first with audio to the last with audio.
			block := rate / 200
			var levels []float64
			for i := 0; i+block <= len(pcm); i += block {
				var sum float64
				for _, s := range pcm[i : i+block] {
					sum += float64(s) * float64(s)
				}
				levels = append(levels, math.Sqrt(sum/float64(block)))
			}
			const tone = 8000 / math.Sqrt2
			first, last := -1, -1
			for i, l := range levels {
				if l > tone/2 {
					if first < 0 {
						first = i
					}
					last = i
				}
			}
			if first < 0 {
				t.Fatal("leg delivered no audio")
			}
			if got := (last - first + 1) * 5; got < 2500 {
				t.Errorf("leg delivered %d ms of audio for 3 s sent", got)
			}
			var gaps int
			for _, l := range levels[first:last] {
				if l < tone/10 {
					gaps++
				}
			}
			// 17 packets were lost. With a jitter buffer and no concealment
			// each is 20 ms of silence, 68 blocks in all. A stall on a slow
			// test machine can outlast the concealment, so a third of that is
			// allowed.
			if gaps > 24 {
				t.Errorf("%d blocks of 5 ms under a tenth of the tone's level: lost packets were not concealed", gaps)
			}
			// With no buffer and no concealment a lost packet leaves no frame
			// at all, and the stream comes out 340 ms short.
			if got := len(pcm) * 1000 / rate; tc.jitterMs == 0 && got < packets*20-100 {
				t.Errorf("leg delivered %d ms for %d ms sent", got, packets*20)
			}
		})
	}
}
