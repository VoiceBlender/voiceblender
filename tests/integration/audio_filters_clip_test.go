//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// fullScaleSpeech is a voiced, formant-shaped signal with a syllabic envelope,
// peak-normalised to int16 full scale. Harmonics of 120 Hz and a 2.5 Hz
// envelope make any whole number of seconds loop seamlessly.
func fullScaleSpeech(rate int, d time.Duration) []int16 {
	n := int(int64(rate) * int64(d) / int64(time.Second))
	formants := [3][2]float64{{700, 90}, {1220, 110}, {2600, 170}}
	x := make([]float64, n)
	var peak float64
	for i := range x {
		ts := float64(i) / float64(rate)
		env := math.Max(0, math.Sin(2*math.Pi*2.5*ts))
		var v float64
		for h := 1; h <= 60; h++ {
			f := 120.0 * float64(h)
			if f > float64(rate)/2*0.95 {
				break
			}
			var g float64
			for _, fm := range formants {
				dd := (f - fm[0]) / fm[1]
				g += 1 / (1 + dd*dd)
			}
			v += g / float64(h) * math.Cos(2*math.Pi*f*ts+float64(h*h%7))
		}
		x[i] = env * v
		peak = math.Max(peak, math.Abs(x[i]))
	}
	out := make([]int16, n)
	for i, v := range x {
		out[i] = int16(v / peak * 32767)
	}
	return out
}

// wrapClicks counts samples that jump to the opposite extreme from both
// neighbours, which is how a denoiser output wrapped past int16 range shows up.
func wrapClicks(s []int16) (clicks int, firstAt int) {
	const swing = 32768
	firstAt = -1
	for i := 1; i+1 < len(s); i++ {
		a, b, c := int(s[i-1]), int(s[i]), int(s[i+1])
		if (b-a > swing && b-c > swing) || (a-b > swing && c-b > swing) {
			if firstAt < 0 {
				firstAt = i
			}
			clicks++
		}
	}
	return clicks, firstAt
}

// TestAudioFilters_DenoiseFullScaleNoClicks sends full-scale speech through
// the denoise filter at each native room rate and checks the denoiser's
// overshoot past int16 range is clipped rather than wrapped into a click.
func TestAudioFilters_DenoiseFullScaleNoClicks(t *testing.T) {
	if err := denoise.Install(); err != nil {
		t.Fatalf("install denoise: %v", err)
	}
	t.Cleanup(func() { denoise.Shutdown() })

	for _, rate := range []int{8000, 16000, 48000} {
		t.Run(fmt.Sprintf("%dHz", rate), func(t *testing.T) {
			roomID := fmt.Sprintf("denoise-clip-%d", rate)
			inst := newTestInstanceWithOpts(t, "denoise-clip",
				func(c *config.Config) { c.DefaultSampleRate = rate })
			wsURL := fmt.Sprintf("ws://%s/v1/legs/websocket?sample_rate=%d&wire_format=binary&room_id=%s",
				inst.httpAddr, rate, roomID)

			seen := map[string]bool{}
			dial := func(label string) (net.Conn, string) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				conn, err := wsDial(ctx, wsURL)
				if err != nil {
					t.Fatalf("dial %s: %v", label, err)
				}
				ringing := inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
					return !seen[e.Data.GetLegID()]
				}, 2*time.Second)
				legID := ringing.Data.GetLegID()
				seen[legID] = true
				inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
					return e.Data.GetLegID() == legID
				}, 2*time.Second)
				return conn, legID
			}
			connA, legA := dial("A")
			defer connA.Close()
			connB, _ := dial("B")
			defer connB.Close()
			quietRoom(t, inst, roomID)

			if code := putFilters(t, inst, legA, []map[string]interface{}{{"type": "denoise"}}); code != http.StatusOK {
				t.Fatalf("enable denoise on leg A: status %d", code)
			}
			waitStreams(t, "rnnoise", denoise.Stats, func(n int) bool { return n > 0 })

			speech := fullScaleSpeech(rate, 2*time.Second)
			frameSamples := rate * 20 / 1000
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				frame := make([]byte, frameSamples*2)
				for off := 0; ; off = (off + frameSamples) % len(speech) {
					select {
					case <-stop:
						return
					case <-ticker.C:
					}
					for i := 0; i < frameSamples; i++ {
						binary.LittleEndian.PutUint16(frame[i*2:], uint16(speech[off+i]))
					}
					if err := wsutil.WriteClientBinary(connA, frame); err != nil {
						return
					}
				}
			}()

			const (
				skipFrames = 25 // let the denoiser's gains converge
				wantFrames = 150
			)
			var pcm []byte
			got := 0
			deadline := time.Now().Add(10 * time.Second)
			for got < skipFrames+wantFrames && time.Now().Before(deadline) {
				wsutilx.SetReadDeadline(connB, time.Until(deadline))
				hdr, err := ws.ReadHeader(connB)
				if err != nil {
					break
				}
				payload := make([]byte, hdr.Length)
				if _, err := io.ReadFull(connB, payload); err != nil {
					break
				}
				if hdr.Masked {
					ws.Cipher(payload, hdr.Mask, 0)
				}
				if hdr.OpCode != ws.OpBinary || len(payload) != frameSamples*2 {
					continue
				}
				got++
				if got > skipFrames {
					pcm = append(pcm, payload...)
				}
			}
			if got < skipFrames+wantFrames {
				t.Fatalf("leg B read only %d frames, want %d", got, skipFrames+wantFrames)
			}

			out := bytesToInt16(pcm)
			var peak, fullScale int
			for _, v := range out {
				a := int(v)
				if a < 0 {
					a = -a
				}
				peak = max(peak, a)
				if a >= 32767 {
					fullScale++
				}
			}
			clicks, at := wrapClicks(out)
			t.Logf("%d Hz: %d samples, peak %d, %d at full scale, %d wrap clicks", rate, len(out), peak, fullScale, clicks)
			if clicks > 0 {
				t.Errorf("%d wrap clicks in the denoised audio, first at sample %d (%d, %d, %d)",
					clicks, at, out[at-1], out[at], out[at+1])
			} else if fullScale == 0 {
				t.Errorf("no sample reached the int16 limit (peak %d); the denoiser no longer overshoots this input, so the test exercises nothing", peak)
			}
		})
	}
}
