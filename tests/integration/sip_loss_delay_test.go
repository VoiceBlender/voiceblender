//go:build integration

package integration

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// bareClient is a SIP client with nothing behind it but a UDP socket for RTP.
type bareClient struct {
	conn   *net.UDPConn
	remote *net.UDPAddr
	legID  string
}

func newBareClient(t *testing.T, ctx context.Context, inst *testInstance, user string) *bareClient {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	recipient := sip.Uri{Scheme: "sip", User: user, Host: "127.0.0.1", Port: inst.sipPort}
	offer := sdpOffer("127.0.0.1", conn.LocalAddr().(*net.UDPAddr).Port, "0", "0 PCMU/8000")
	dialog, inbound := answerInboundCall(t, ctx, inst, recipient, offer)
	answer, err := sipmod.ParseSDP(dialog.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parse answer: %v", err)
	}
	if err := dialog.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)
	t.Cleanup(func() { hangup(t, inst, inbound.ID) })
	return &bareClient{
		conn:   conn,
		remote: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: answer.RemotePort},
		legID:  inbound.ID,
	}
}

func (c *bareClient) join(t *testing.T, inst *testInstance, room string) {
	t.Helper()
	resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", inst.baseURL(), room), map[string]interface{}{"leg_id": c.legID})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add leg %s to room %s: status %d", c.legID, room, resp.StatusCode)
	}
}

// clickDelay is one click heard at the far end of a room.
type clickDelay struct {
	frame   int     // frame of the sender's stream that carried the click
	ms      float64 // from that packet leaving the sender to the click reaching the listener
	shiftMs float64 // the same, counted in samples of the two streams, against the first click
	starved uint64  // mix ticks so far that found the sender's queue empty
}

const (
	clickEvery = 10 // frames
	clickLevel = 20000
)

// probeClickDelays puts two bare SIP clients in a room. The first sends a quiet
// tone with a click every 200 ms, less the frames in lost; the second times
// each click. prefill starts the audio before the legs join the room.
func probeClickDelays(t *testing.T, frames int, lost map[int]bool, prefill bool, offset time.Duration) []clickDelay {
	t.Helper()
	inst := newTestInstanceFull(t, "plc-delay", func(c *config.Config) {
		c.SIPJitterBufferMs = 0
	}, []codec.CodecType{codec.CodecPCMU})
	createRoom(t, inst, "delay", 16000)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(frames)*20*time.Millisecond+20*time.Second)
	defer cancel()

	a := newBareClient(t, ctx, inst, "501")
	b := newBareClient(t, ctx, inst, "502")
	if !prefill {
		a.join(t, inst, "delay")
		b.join(t, inst, "delay")
	}
	rm, ok := inst.roomMgr.Get("delay")
	if !ok {
		t.Fatal("room not found")
	}

	type heard struct {
		at  time.Time
		pos int // sample position in the listener's stream
	}
	var (
		mu     sync.Mutex
		clicks []heard
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dec, _ := codec.NewDecoder(codec.CodecPCMU)
		buf := make([]byte, 1500)
		var (
			pkt     rtp.Packet
			ts0     uint32
			haveTS  bool
			lastPos = -1 << 30
		)
		for {
			select {
			case <-stop:
				return
			default:
			}
			b.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, _, err := b.conn.ReadFromUDP(buf)
			now := time.Now()
			if err != nil {
				continue
			}
			if err := pkt.Unmarshal(buf[:n]); err != nil || pkt.PayloadType != 0 {
				continue
			}
			if !haveTS {
				ts0, haveTS = pkt.Timestamp, true
			}
			pcm, err := dec.Decode(pkt.Payload)
			if err != nil {
				continue
			}
			for i, s := range pcm {
				if s > clickLevel/3 || s < -clickLevel/3 {
					pos := int(int32(pkt.Timestamp-ts0)) + i
					// A click is smeared over a few samples and may cross a packet edge.
					if pos-lastPos > 400 {
						mu.Lock()
						clicks = append(clicks, heard{now.Add(time.Duration(i) * time.Second / 8000), pos})
						mu.Unlock()
					}
					lastPos = pos
					break
				}
			}
		}
	}()

	// The listener sends silence, as a quiet party on a real call does.
	enc, err := codec.NewEncoder(codec.CodecPCMU)
	if err != nil {
		t.Fatal(err)
	}
	silence, err := enc.Encode(make([]int16, 160))
	if err != nil {
		t.Fatal(err)
	}
	silence = append([]byte(nil), silence...)
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		for i := 0; ; i++ {
			time.Sleep(time.Until(start.Add(time.Duration(i) * 20 * time.Millisecond)))
			select {
			case <-stop:
				return
			default:
			}
			raw, _ := (&rtp.Packet{
				Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: uint16(i), Timestamp: uint32(i) * 160, SSRC: 0xb0b},
				Payload: silence,
			}).Marshal()
			b.conn.WriteToUDP(raw, b.remote)
		}
	}()

	type sent struct {
		frame   int
		at      time.Time
		starved uint64
	}
	var sends []sent
	time.Sleep(offset)
	start := time.Now()
	frame := make([]int16, 160)
	for f := 0; f < frames; f++ {
		time.Sleep(time.Until(start.Add(time.Duration(f) * 20 * time.Millisecond)))
		if prefill && f == 10 {
			a.join(t, inst, "delay")
			b.join(t, inst, "delay")
		}
		for i := range frame {
			frame[i] = int16(1000 * math.Sin(2*math.Pi*300*float64(f*160+i)/8000))
		}
		click := f%clickEvery == 0
		if click {
			for i := 0; i < 16; i++ {
				frame[i] = clickLevel
			}
		}
		if lost[f] {
			continue
		}
		payload, err := enc.Encode(frame)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := (&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: uint16(f), Timestamp: uint32(f) * 160, SSRC: 0xa11ce},
			Payload: payload,
		}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.conn.WriteToUDP(raw, a.remote); err != nil {
			t.Fatal(err)
		}
		if click {
			_, starved, _, _ := rm.Mixer().ParticipantFeed(a.legID)
			sends = append(sends, sent{f, time.Now(), starved})
		}
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Each click heard belongs to the last one sent before it.
	var out []clickDelay
	base := math.NaN()
	for _, h := range clicks {
		i := sort.Search(len(sends), func(i int) bool { return sends[i].at.After(h.at) }) - 1
		if i < 0 {
			continue
		}
		s := sends[i]
		shift := float64(h.pos-s.frame*160) / 8
		if math.IsNaN(base) {
			base = shift
		}
		out = append(out, clickDelay{
			frame:   s.frame,
			ms:      float64(h.at.Sub(s.at).Microseconds()) / 1000,
			shiftMs: shift - base,
			starved: s.starved,
		})
	}
	return out
}

// TestSIPLossDelayProbe prints the delay through a room, click by click, as
// packets are lost. It is a measuring tool and asserts nothing, because the
// figures depend on how the clocks of the sender, the mixer and the leg fall.
// LOSS_DELAY_LOST lists the frames to lose, in place of the default.
func TestSIPLossDelayProbe(t *testing.T) {
	if os.Getenv("LOSS_DELAY_PROBE") == "" {
		t.Skip("set LOSS_DELAY_PROBE=1 to run the delay probe")
	}
	lost := map[int]bool{}
	spec := os.Getenv("LOSS_DELAY_LOST")
	if spec == "" {
		// One, two, one, three and four in a row. A hole of four is not filled.
		spec = "105,205,206,305,355,356,357,405,406,407,408"
	}
	for _, s := range strings.Split(spec, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			lost[n] = true
		}
	}
	for _, prefill := range []bool{false, true} {
		for _, offset := range []time.Duration{0, 7 * time.Millisecond, 14 * time.Millisecond} {
			t.Run(fmt.Sprintf("prefill=%v/offset=%v", prefill, offset), func(t *testing.T) {
				var line strings.Builder
				for _, d := range probeClickDelays(t, 500, lost, prefill, offset) {
					fmt.Fprintf(&line, "\n  frame %3d  delay %6.1f ms  shift %+6.1f ms  starved %d", d.frame, d.ms, d.shiftMs, d.starved)
				}
				t.Logf("lost %s%s", spec, line.String())
			})
		}
	}
}
