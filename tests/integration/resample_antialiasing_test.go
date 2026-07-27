//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// TestResampleAntiAliasing proves the polyphase resampler removes above-Nyquist
// energy end-to-end instead of folding it back into the voice band, which the
// old drop-every-other-sample downsampler could not do.
//
// Two 16 kHz WS legs share an 8 kHz room (destination Nyquist 4 kHz). Leg A
// injects a tone; the mixer routes it (mixed-minus-self) to leg B, which reads
// it back at 16 kHz. Feeding 6 kHz — above the room Nyquist — folds to
// |6000-8000| = 2000 Hz under naive decimation; the anti-aliasing filter must
// keep that 2 kHz alias far below the energy a genuine in-band tone delivers.
func TestResampleAntiAliasing(t *testing.T) {
	const (
		legRate    = 16000
		roomRate   = 8000
		roomID     = "aa-room"
		refHz      = 1500.0 // in band at 8 kHz: must pass through
		aboveNyqHz = 6000.0 // above the 4 kHz room Nyquist: must be filtered out
		aliasHz    = 2000.0 // where 6 kHz folds to under naive decimation
		amplitude  = 8000.0
	)

	inst := newTestInstanceWithOpts(t, "antialias",
		func(c *config.Config) { c.DefaultSampleRate = roomRate })

	wsURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=binary&room_id=" + roomID

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
		inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
			return e.Data.GetLegID() == legID
		}, 2*time.Second)
		inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
			return e.Data.GetLegID() == legID
		}, 2*time.Second)
		return conn, legID
	}

	connA, legA := dial("A")
	defer connA.Close()
	connB, legB := dial("B")
	defer connB.Close()
	t.Logf("legA=%s legB=%s room=%dHz leg=%dHz", legA, legB, roomRate, legRate)

	// Exact-zero mix: comfort noise would smear energy across every bin,
	// including the alias bin we are trying to measure as silent.
	quietRoom(t, inst, roomID)

	// captureAtB streams toneHz from leg A for ~1 s and returns the PCM leg B
	// reads back, dropping the mixer's settle frames.
	captureAtB := func(toneHz float64) []int16 {
		t.Helper()
		stop := make(chan struct{})
		go func() {
			var phase float64
			dPhase := 2 * math.Pi * toneHz / float64(legRate)
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					frame := make([]byte, legRate*20/1000*2)
					for i := 0; i < len(frame)/2; i++ {
						binary.LittleEndian.PutUint16(frame[i*2:], uint16(int16(amplitude*math.Sin(phase))))
						phase += dPhase
					}
					if err := wsutil.WriteClientBinary(connA, frame); err != nil {
						return
					}
				}
			}
		}()
		defer close(stop)

		const (
			frameBytes = legRate * 20 / 1000 * 2
			skipFrames = 8  // mixer settle + filter lead-in
			wantFrames = 50 // ~1 s
		)
		var pcm []byte
		got := 0
		deadline := time.Now().Add(5 * time.Second)
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
			if hdr.OpCode != ws.OpBinary || len(payload) != frameBytes {
				continue
			}
			got++
			if got > skipFrames {
				pcm = append(pcm, payload...)
			}
		}
		if got < skipFrames+wantFrames {
			t.Fatalf("captured only %d frames for %g Hz (want %d)", got, toneHz, skipFrames+wantFrames)
		}
		return bytesToInt16(pcm)
	}

	// Reference: an in-band tone proves the pipeline passes signal at all and
	// sets the energy scale the alias is measured against.
	ref := goertzelPower(captureAtB(refHz), legRate, refHz)
	if ref <= 0 {
		t.Fatalf("no %g Hz energy reached leg B; pipeline delivered nothing to measure against", refHz)
	}

	// The tone above the room Nyquist must not reappear as a low-frequency alias.
	above := captureAtB(aboveNyqHz)
	alias := goertzelPower(above, legRate, aliasHz)
	leak := goertzelPower(above, legRate, aboveNyqHz)
	t.Logf("ref(%gHz)=%.3g  alias(%gHz)=%.3g  passthrough-leak(%gHz)=%.3g", refHz, ref, aliasHz, alias, aboveNyqHz, leak)

	// Naive decimation folds 6 kHz onto 2 kHz at roughly full strength, so the
	// alias energy would rival the reference. The filter must hold it well below
	// — a 10x (~20 dB) margin fails loudly on the old code and passes on the new.
	if alias > ref/10 {
		t.Errorf("2 kHz alias energy %.3g is within 10x of the %g Hz reference %.3g — above-Nyquist energy is folding into the voice band",
			alias, refHz, ref)
	}
}
