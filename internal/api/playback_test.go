package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/room"
)

// playbackTestLeg is a minimal leg.Leg for legPlaybackWriter tests.
// SampleRate and RoomID are settable; the writer reads them on every
// Write so tests can simulate mid-stream room moves.
type playbackTestLeg struct {
	id          string
	sampleRate  int
	mu          sync.Mutex
	roomID      string
	directBytes bytes.Buffer
}

func (m *playbackTestLeg) ID() string        { return m.id }
func (m *playbackTestLeg) Type() leg.LegType { return leg.TypeSIPInbound }
func (m *playbackTestLeg) State() leg.LegState {
	return leg.StateConnected
}
func (m *playbackTestLeg) SampleRate() int { return m.sampleRate }
func (m *playbackTestLeg) AudioReader() io.Reader {
	return nil
}
func (m *playbackTestLeg) AudioWriter() io.Writer {
	return &m.directBytes
}
func (m *playbackTestLeg) OnDTMF(func(rune))                      {}
func (m *playbackTestLeg) SendDTMF(context.Context, string) error { return nil }
func (m *playbackTestLeg) Hangup(context.Context) error           { return nil }
func (m *playbackTestLeg) Answer(context.Context) error           { return nil }
func (m *playbackTestLeg) Context() context.Context               { return context.Background() }
func (m *playbackTestLeg) RoomID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.roomID
}
func (m *playbackTestLeg) SetRoomID(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roomID = id
}
func (m *playbackTestLeg) AppID() string                          { return "" }
func (m *playbackTestLeg) SetAppID(string)                        {}
func (m *playbackTestLeg) Role() string                           { return "" }
func (m *playbackTestLeg) SetRole(string)                         {}
func (m *playbackTestLeg) IsMuted() bool                          { return false }
func (m *playbackTestLeg) SetMuted(bool)                          {}
func (m *playbackTestLeg) IsDeaf() bool                           { return false }
func (m *playbackTestLeg) SetDeaf(bool)                           {}
func (m *playbackTestLeg) AcceptDTMF() bool                       { return true }
func (m *playbackTestLeg) SetAcceptDTMF(bool)                     {}
func (m *playbackTestLeg) OnTextReceived(func(string, bool))      {}
func (m *playbackTestLeg) SendText(context.Context, string) error { return leg.ErrRTTNotNegotiated }
func (m *playbackTestLeg) AcceptText() bool                       { return false }
func (m *playbackTestLeg) SetAcceptText(bool)                     {}
func (m *playbackTestLeg) RTTNegotiated() bool                    { return false }
func (m *playbackTestLeg) SetSpeakingTap(io.Writer)               {}
func (m *playbackTestLeg) ClearSpeakingTap()                      {}
func (m *playbackTestLeg) IsHeld() bool                           { return false }
func (m *playbackTestLeg) CreatedAt() time.Time                   { return time.Now() }
func (m *playbackTestLeg) AnsweredAt() time.Time                  { return time.Time{} }
func (m *playbackTestLeg) SIPHeaders() map[string]string          { return nil }
func (m *playbackTestLeg) Headers() map[string]string             { return nil }
func (m *playbackTestLeg) RTPStats() leg.RTPStats                 { return leg.RTPStats{} }
func (m *playbackTestLeg) ClaimDisconnect() bool                  { return true }

// sineFrame returns a `ptimeMs` 16-bit LE PCM mono frame at sampleRate
// holding a `freqHz` sine wave with amplitude 16000.
func sineFrame(sampleRate int, freqHz, ptimeMs int) []byte {
	n := sampleRate * ptimeMs / 1000
	out := make([]byte, n*2)
	inc := 2 * math.Pi * float64(freqHz) / float64(sampleRate)
	for i := 0; i < n; i++ {
		s := int16(math.Sin(float64(i)*inc) * 16000)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

// countZeroCrossings counts sign changes between adjacent samples,
// ignoring the all-zero leading region. Used as a cheap pitch check:
// a tone at freq Hz produces ~2·freq crossings per second of audio.
func countZeroCrossings(pcm []byte) int {
	samples := len(pcm) / 2
	if samples < 2 {
		return 0
	}
	prev := int16(binary.LittleEndian.Uint16(pcm[0:]))
	zc := 0
	for i := 1; i < samples; i++ {
		cur := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		if (prev < 0 && cur >= 0) || (prev >= 0 && cur < 0) {
			zc++
		}
		prev = cur
	}
	return zc
}

// loudWindowsZCRate scans `pcm` in `windowMs` windows at `sampleRate`,
// keeps the windows whose RMS is above `minRMS`, and returns the
// aggregated zero-crossing rate per second on just those windows.
// Ignores silence gaps produced when the mixer ticks with an empty
// inject channel.
func loudWindowsZCRate(pcm []byte, sampleRate, windowMs int, minRMS float64) (zcPerSec float64, loudWindows int) {
	samplesPerWindow := sampleRate * windowMs / 1000
	bytesPerWindow := samplesPerWindow * 2
	var totalZC, totalSamples int
	for off := 0; off+bytesPerWindow <= len(pcm); off += bytesPerWindow {
		win := pcm[off : off+bytesPerWindow]
		var sumSq float64
		for i := 0; i < samplesPerWindow; i++ {
			s := int16(binary.LittleEndian.Uint16(win[i*2:]))
			sumSq += float64(s) * float64(s)
		}
		rms := math.Sqrt(sumSq / float64(samplesPerWindow))
		if rms < minRMS {
			continue
		}
		totalZC += countZeroCrossings(win)
		totalSamples += samplesPerWindow
		loudWindows++
	}
	if totalSamples == 0 {
		return 0, 0
	}
	return float64(totalZC) * float64(sampleRate) / float64(totalSamples), loudWindows
}

func TestResamplePCM16_Identity(t *testing.T) {
	in := sineFrame(16000, 1000, 20)
	out := resamplePCM16(in, 16000, 16000)
	if !bytes.Equal(in, out) {
		t.Fatal("identity resample should return input bytes unchanged")
	}
}

func TestResamplePCM16_LengthAndPitch(t *testing.T) {
	// Resample a 1 kHz tone between every supported room rate and the
	// telephony codec rates. After resampling, the dominant frequency
	// must stay at 1 kHz (zero-crossings per second within 20%).
	cases := []struct{ src, dst int }{
		{8000, 16000},
		{8000, 48000},
		{16000, 8000},
		{16000, 48000},
		{48000, 8000},
		{48000, 16000},
	}
	for _, c := range cases {
		// 500 ms of audio gives the zero-crossing rate room to average.
		var srcBuf bytes.Buffer
		for i := 0; i < 25; i++ {
			// 25 × 20 ms = 500 ms. Each call regenerates the wave
			// starting at phase 0, which is fine for a per-frame
			// crossing-rate check (boundaries match zero).
			srcBuf.Write(sineFrame(c.src, 1000, 20))
		}
		var dstBuf bytes.Buffer
		for off := 0; off < srcBuf.Len(); off += c.src * 20 / 1000 * 2 {
			frame := srcBuf.Bytes()[off : off+c.src*20/1000*2]
			dstBuf.Write(resamplePCM16(frame, uint32(c.src), uint32(c.dst)))
		}

		wantBytes := c.dst * 20 / 1000 * 2 * 25
		if got := dstBuf.Len(); got < wantBytes-4 || got > wantBytes+4 {
			t.Errorf("%d→%d: output %d bytes, want ~%d", c.src, c.dst, got, wantBytes)
		}

		durSec := float64(dstBuf.Len()) / 2 / float64(c.dst)
		zc := countZeroCrossings(dstBuf.Bytes())
		zcPerSec := float64(zc) / durSec
		expected := 2 * 1000.0 // 2·freq for a sine wave
		if zcPerSec < expected*0.8 || zcPerSec > expected*1.2 {
			t.Errorf("%d→%d: zero-crossings/s = %.0f, want %.0f ±20%%",
				c.src, c.dst, zcPerSec, expected)
		}
	}
}

// newPlaybackRoomMgr makes a Manager + Room(s) at the given rates,
// ready to receive inject writes.
func newPlaybackRoomMgr(t *testing.T) *room.Manager {
	t.Helper()
	bus := events.NewBus("test")
	legMgr := leg.NewManager()
	return room.NewManager(legMgr, bus, slog.Default())
}

// addInjectableParticipant adds a participant whose reader is silent
// and whose writer captures all received PCM frames. Used to read back
// the room mixer's output (which equals the injected samples when this
// is the only non-silent input).
type captureWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *captureWriter) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.buf.Len())
	copy(out, c.buf.Bytes())
	return out
}

// silenceReader returns zeros (PCM silence) forever, gated by ctx.
type silenceReader struct {
	ctx    context.Context
	closed atomic.Bool
}

func (s *silenceReader) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.EOF
	}
	select {
	case <-s.ctx.Done():
		return 0, io.EOF
	default:
	}
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
func (s *silenceReader) Close() error {
	s.closed.Store(true)
	return nil
}

// TestLegPlaybackWriter_DirectPath verifies writes go to the leg's
// direct writer when the leg is not in any room, and resample to the
// leg's native rate.
func TestLegPlaybackWriter_DirectPath(t *testing.T) {
	rmMgr := newPlaybackRoomMgr(t)
	l := &playbackTestLeg{id: "leg-1", sampleRate: 8000}

	w := &legPlaybackWriter{
		legID:        l.id,
		leg:          l,
		directWriter: l.AudioWriter(),
		roomMgr:      rmMgr,
		srcRate:      16000,
	}

	// Write 100 ms of 1 kHz @ 16 kHz; expect 100 ms of 1 kHz @ 8 kHz on
	// the direct writer (length halved, pitch preserved).
	for i := 0; i < 5; i++ {
		frame := sineFrame(16000, 1000, 20)
		n, err := w.Write(frame)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if n != len(frame) {
			t.Errorf("write n=%d, want %d", n, len(frame))
		}
	}

	got := l.directBytes.Bytes()
	wantBytes := 8000 * 20 / 1000 * 2 * 5
	if len(got) < wantBytes-4 || len(got) > wantBytes+4 {
		t.Errorf("direct writer received %d bytes, want ~%d (8 kHz × 100 ms)", len(got), wantBytes)
	}

	zc := countZeroCrossings(got)
	zcPerSec := float64(zc) / (float64(len(got)) / 2 / 8000)
	if zcPerSec < 1600 || zcPerSec > 2400 {
		t.Errorf("zero-crossings/s = %.0f, want ~2000 (1 kHz tone preserved)", zcPerSec)
	}
}

func TestLegPlaybackWriter_DirectPath_RateMatch(t *testing.T) {
	rmMgr := newPlaybackRoomMgr(t)
	l := &playbackTestLeg{id: "leg-1", sampleRate: 16000}

	w := &legPlaybackWriter{
		legID:        l.id,
		leg:          l,
		directWriter: l.AudioWriter(),
		roomMgr:      rmMgr,
		srcRate:      16000,
	}

	in := sineFrame(16000, 1000, 20)
	if _, err := w.Write(in); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(l.directBytes.Bytes(), in) {
		t.Error("rate-match direct path should pass bytes through unchanged")
	}
}

// runInjectScenario sets up a single-participant room with `roomRate`,
// runs the mixer, exercises legPlaybackWriter at `srcRate`, and
// returns what came back on the participant's writer. The participant
// reads silence, so its mixed-minus-self equals the injected audio.
func runInjectScenario(t *testing.T, roomRate int, srcRate uint32, writes int) []byte {
	t.Helper()
	rmMgr := newPlaybackRoomMgr(t)
	rm, err := rmMgr.Create("room-x", "", roomRate)
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	t.Cleanup(func() { _ = rmMgr.Delete(rm.ID) })

	l := &playbackTestLeg{id: "inject-leg", sampleRate: 8000, roomID: rm.ID}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sr := &silenceReader{ctx: ctx}
	cw := &captureWriter{}
	rm.Mixer().SetComfortNoise(false) // make captured output purely a function of inject
	rm.Mixer().AddParticipant(l.id, sr, cw)
	rm.Mixer().Start()
	t.Cleanup(func() {
		rm.Mixer().RemoveParticipant(l.id)
		rm.Mixer().Stop()
	})

	w := &legPlaybackWriter{
		legID:        l.id,
		leg:          l,
		directWriter: l.AudioWriter(),
		roomMgr:      rmMgr,
		srcRate:      srcRate,
	}

	// Give the mixer one tick to prime readLoop on the silence source.
	time.Sleep(40 * time.Millisecond)

	for i := 0; i < writes; i++ {
		frame := sineFrame(int(srcRate), 1000, 20)
		if _, err := w.Write(frame); err != nil {
			t.Fatalf("write: %v", err)
		}
		// Pace slightly faster than the mixer's 20 ms tick so the
		// inject channel stays primed; surplus writes are absorbed by
		// its 3-slot buffer with the rest silently dropped.
		time.Sleep(18 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)

	return cw.Bytes()
}

func TestLegPlaybackWriter_InjectPath_RateMatch(t *testing.T) {
	got := runInjectScenario(t, 16000, 16000, 20)
	// 4 ms windows: short enough to sit inside a single inject burst,
	// long enough to contain multiple cycles of a 1 kHz tone (so the
	// zc count is stable). A pitch-shifted burst would still elevate
	// the per-window rate; averaging over a longer window would mask
	// it by averaging with the surrounding silence padding.
	zcRate, loud := loudWindowsZCRate(got, 16000, 4, 4000)
	if loud < 20 {
		t.Fatalf("only %d loud windows captured (need ≥20)", loud)
	}
	if zcRate < 1700 || zcRate > 2300 {
		t.Errorf("zc/s on loud windows = %.0f, want ~2000 (1 kHz preserved at room rate)", zcRate)
	}
}

func TestLegPlaybackWriter_InjectPath_UpsampleToRoom(t *testing.T) {
	// THE BUG SCENARIO: producer at 16 kHz, room at 48 kHz. Without
	// resampling on the inject path, the captured tone would jump from
	// 1 kHz → 3 kHz (per-burst zc/s 2000 → 6000).
	got := runInjectScenario(t, 48000, 16000, 20)
	zcRate, loud := loudWindowsZCRate(got, 48000, 4, 4000)
	if loud < 20 {
		t.Fatalf("only %d loud windows captured (need ≥20)", loud)
	}
	if zcRate < 1700 || zcRate > 2300 {
		t.Errorf("zc/s on loud windows = %.0f, want ~2000 (1 kHz tone, not pitch-shifted by upsample)", zcRate)
	}
}

func TestLegPlaybackWriter_InjectPath_DownsampleToRoom(t *testing.T) {
	// Producer at 48 kHz, room at 8 kHz: a SIP leg in a low-rate room
	// receiving wideband playback should still hear 1 kHz, not ~167 Hz.
	got := runInjectScenario(t, 8000, 48000, 20)
	zcRate, loud := loudWindowsZCRate(got, 8000, 4, 4000)
	if loud < 20 {
		t.Fatalf("only %d loud windows captured (need ≥20)", loud)
	}
	if zcRate < 1700 || zcRate > 2300 {
		t.Errorf("zc/s on loud windows = %.0f, want ~2000 (1 kHz preserved across downsample)", zcRate)
	}
}

// TestLegPlaybackWriter_RoomMoveMidStream emulates the IVR race that
// triggered the original bug: playback starts while the leg is not in
// any room, the leg gets added to a different-rate room, and later
// writes must resample to the new room's rate instead of being
// reinterpreted at the wrong rate (the cause of the high-pitch glitch).
func TestLegPlaybackWriter_RoomMoveMidStream(t *testing.T) {
	rmMgr := newPlaybackRoomMgr(t)
	rm, err := rmMgr.Create("room-48k", "", 48000)
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	t.Cleanup(func() { _ = rmMgr.Delete(rm.ID) })

	l := &playbackTestLeg{id: "moving-leg", sampleRate: 8000} // not yet in room

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sr := &silenceReader{ctx: ctx}
	cw := &captureWriter{}
	rm.Mixer().SetComfortNoise(false)
	rm.Mixer().AddParticipant(l.id, sr, cw)
	rm.Mixer().Start()
	t.Cleanup(func() {
		rm.Mixer().RemoveParticipant(l.id)
		rm.Mixer().Stop()
	})

	// Producer rate is 16 kHz (the hardcoded mixer default at TTS-start).
	w := &legPlaybackWriter{
		legID:        l.id,
		leg:          l,
		directWriter: l.AudioWriter(),
		roomMgr:      rmMgr,
		srcRate:      16000,
	}

	time.Sleep(40 * time.Millisecond)

	// Frames 1..3: leg not in a room → direct writer at 8 kHz.
	for i := 0; i < 3; i++ {
		if _, err := w.Write(sineFrame(16000, 1000, 20)); err != nil {
			t.Fatalf("pre-move write: %v", err)
		}
	}

	// Race trigger: leg joins the 48 kHz room mid-stream.
	l.SetRoomID(rm.ID)

	// Post-move: leg now in 48 kHz room → inject path, must resample.
	for i := 0; i < 20; i++ {
		if _, err := w.Write(sineFrame(16000, 1000, 20)); err != nil {
			t.Fatalf("post-move write: %v", err)
		}
		time.Sleep(18 * time.Millisecond) // slightly faster than mixer tick to keep inject channel busy
	}
	time.Sleep(40 * time.Millisecond)

	// Direct writer should have ~3 × 8 kHz × 20 ms = 960 bytes.
	directBytes := l.directBytes.Bytes()
	if d := len(directBytes); d < 940 || d > 980 {
		t.Errorf("direct writer received %d bytes, want ~960 (3 × 8 kHz × 20 ms)", d)
	}
	// Pitch must be preserved on the direct path.
	if zc := countZeroCrossings(directBytes); zc != 0 {
		dur := float64(len(directBytes)) / 2 / 8000
		zcPerSec := float64(zc) / dur
		if zcPerSec < 1600 || zcPerSec > 2400 {
			t.Errorf("direct path zc/s = %.0f, want ~2000", zcPerSec)
		}
	}

	// Captured (post-move) audio must have a 1 kHz fundamental at 48 kHz.
	zcRate, loud := loudWindowsZCRate(cw.Bytes(), 48000, 4, 4000)
	if loud < 20 {
		t.Fatalf("only %d loud windows captured post-move", loud)
	}
	if zcRate < 1700 || zcRate > 2300 {
		t.Errorf("post-move zc/s on loud windows = %.0f, want ~2000 (1 kHz, not pitch-shifted by room rate change)", zcRate)
	}
}

// trimLeadingSilence drops PCM samples until the first sample with
// abs value > 200. Matches the helper in webrtc_audio_test.go.
func trimLeadingSilence(p []byte) []byte {
	samples := len(p) / 2
	start := 0
	for start < samples {
		s := int16(binary.LittleEndian.Uint16(p[start*2:]))
		if s > 200 || s < -200 {
			break
		}
		start++
	}
	return p[start*2:]
}

func TestPlaybackReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"reached end of audio", nil, "completed"},
		{"cancelled", context.Canceled, "stopped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := playbackReason(tt.err); got != tt.want {
				t.Errorf("playbackReason(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
