package leg

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/pion/webrtc/v4"
)

// loopbackPair is a connected caller/callee PCMedia pair. The caller is the
// offerer and stands in for a browser; the callee is the side under test and
// keeps its own candidate buffer, drained by a pump like a polling client.
type loopbackPair struct {
	caller, callee *PCMedia

	mu      sync.Mutex
	held    bool
	pending []webrtc.ICECandidateInit

	calleeCands atomic.Int64
	calleeDone  atomic.Bool
}

func newLoopbackPair(t *testing.T, calleeCfg PCMediaConfig) *loopbackPair {
	t.Helper()
	caller, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecPCMU, Log: testLogger(), LoopbackICE: true})
	if err != nil {
		t.Fatalf("caller NewPCMedia: %v", err)
	}
	t.Cleanup(func() { caller.Close() })

	calleeCfg.Codec = codec.CodecPCMU
	calleeCfg.Log = testLogger()
	calleeCfg.LoopbackICE = true
	callee, err := NewPCMedia(calleeCfg)
	if err != nil {
		t.Fatalf("callee NewPCMedia: %v", err)
	}
	t.Cleanup(func() { callee.Close() })

	p := &loopbackPair{caller: caller, callee: callee}
	caller.PC().OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.held {
			p.pending = append(p.pending, c.ToJSON())
			return
		}
		_ = callee.AddICECandidate(c.ToJSON())
	})

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				cs, done := callee.DrainLocalCandidates()
				for _, c := range cs {
					_ = caller.PC().AddICECandidate(c)
				}
				p.calleeCands.Add(int64(len(cs)))
				p.calleeDone.Store(done)
			}
		}
	}()

	offer, err := caller.PC().CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := caller.PC().SetLocalDescription(offer); err != nil {
		t.Fatalf("caller SetLocalDescription: %v", err)
	}
	if err := callee.PC().SetRemoteDescription(offer); err != nil {
		t.Fatalf("callee SetRemoteDescription: %v", err)
	}
	answer, err := callee.PC().CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := callee.PC().SetLocalDescription(answer); err != nil {
		t.Fatalf("callee SetLocalDescription: %v", err)
	}
	if err := caller.PC().SetRemoteDescription(answer); err != nil {
		t.Fatalf("caller SetRemoteDescription: %v", err)
	}
	caller.Start()
	callee.Start()
	p.waitConnected(t)
	return p
}

func iceUp(pc *webrtc.PeerConnection) bool {
	s := pc.ICEConnectionState()
	return s == webrtc.ICEConnectionStateConnected || s == webrtc.ICEConnectionStateCompleted
}

func (p *loopbackPair) waitConnected(t *testing.T) {
	t.Helper()
	waitFor(t, 10*time.Second, "ICE connected on both sides", func() bool {
		return iceUp(p.caller.PC()) && iceUp(p.callee.PC())
	})
}

// holdCallerCandidates buffers the caller's trickled candidates until the
// returned release runs, as a client must while its restart offer is in flight.
func (p *loopbackPair) holdCallerCandidates() (release func()) {
	p.mu.Lock()
	p.held = true
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.pending {
			_ = p.callee.AddICECandidate(c)
		}
		p.pending, p.held = nil, false
	}
}

// restartOffer restarts ICE on the caller only, which drops its old sockets.
func (p *loopbackPair) restartOffer(t *testing.T) webrtc.SessionDescription {
	t.Helper()
	offer, err := p.caller.PC().CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("restart CreateOffer: %v", err)
	}
	if err := p.caller.PC().SetLocalDescription(offer); err != nil {
		t.Fatalf("restart SetLocalDescription: %v", err)
	}
	return offer
}

func (p *loopbackPair) completeRestart(t *testing.T, offer webrtc.SessionDescription, release func()) string {
	t.Helper()
	answerSDP, err := p.callee.RestartICE(offer.SDP)
	if err != nil {
		t.Fatalf("RestartICE: %v", err)
	}
	release()
	answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}
	if err := p.caller.PC().SetRemoteDescription(answer); err != nil {
		t.Fatalf("caller SetRemoteDescription(restart answer): %v", err)
	}
	return answerSDP
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pumpPCM writes a PCM frame from the caller every 20 ms until the test ends.
func (p *loopbackPair) pumpPCM(t *testing.T) {
	t.Helper()
	pcm := make([]byte, 320)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		w := p.caller.AudioWriter()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_, _ = w.Write(pcm)
			}
		}
	}()
}

// requirePCM fails unless at least min bytes of decoded audio arrive on the
// callee within timeout.
func (p *loopbackPair) requirePCM(t *testing.T, min int, timeout time.Duration) {
	t.Helper()
	got := make(chan struct{})
	go func() {
		buf := make([]byte, min)
		if _, err := io.ReadFull(p.callee.AudioReader(), buf); err == nil {
			close(got)
		}
	}()
	select {
	case <-got:
	case <-time.After(timeout):
		t.Fatalf("callee did not receive %d bytes of PCM within %s", min, timeout)
	}
}

func mustICEUfrag(t *testing.T, sdp string) string {
	t.Helper()
	ufrag, _, err := sdpICECredentials(sdp)
	if err != nil {
		t.Fatalf("sdpICECredentials: %v", err)
	}
	return ufrag
}

func TestPCMedia_RestartICE_WhileConnected(t *testing.T) {
	if testing.Short() {
		t.Skip("involves real ICE/DTLS; skipped in -short")
	}
	var interrupted, restored, disconnected atomic.Int32
	p := newLoopbackPair(t, PCMediaConfig{
		TolerateICEDisconnect: true,
		OnInterrupted:         func() { interrupted.Add(1) },
		OnRestored:            func() { restored.Add(1) },
		OnDisconnect:          func(string) { disconnected.Add(1) },
	})
	p.pumpPCM(t)
	p.requirePCM(t, 320, 15*time.Second)

	waitFor(t, 5*time.Second, "initial gathering done", p.calleeDone.Load)
	candsBefore := p.calleeCands.Load()
	ufragBefore := mustICEUfrag(t, p.callee.PC().LocalDescription().SDP)

	release := p.holdCallerCandidates()
	offer := p.restartOffer(t)
	answerSDP := p.completeRestart(t, offer, release)

	if got := mustICEUfrag(t, answerSDP); got == ufragBefore {
		t.Fatalf("answer kept local ufrag %q; expected new ICE credentials", got)
	}
	p.waitConnected(t)
	waitFor(t, 5*time.Second, "new local candidates after restart", func() bool {
		return p.calleeCands.Load() > candsBefore
	})
	waitFor(t, 5*time.Second, "gathering done after restart", p.calleeDone.Load)

	// More than the inbound frame buffer can hold, so it proves fresh audio.
	p.requirePCM(t, 320*30, 10*time.Second)

	if interrupted.Load() != 0 || restored.Load() != 0 || disconnected.Load() != 0 {
		t.Errorf("unexpected callbacks: interrupted=%d restored=%d disconnected=%d",
			interrupted.Load(), restored.Load(), disconnected.Load())
	}
}

func TestPCMedia_RestartICE_AfterInterruption(t *testing.T) {
	if testing.Short() {
		t.Skip("involves real ICE/DTLS; skipped in -short")
	}
	var interrupted, restored, disconnected atomic.Int32
	p := newLoopbackPair(t, PCMediaConfig{
		TolerateICEDisconnect:  true,
		ICEDisconnectedTimeout: time.Second,
		ICEFailedTimeout:       30 * time.Second,
		OnInterrupted:          func() { interrupted.Add(1) },
		OnRestored:             func() { restored.Add(1) },
		OnDisconnect:           func(string) { disconnected.Add(1) },
	})
	p.pumpPCM(t)
	p.requirePCM(t, 320, 15*time.Second)

	release := p.holdCallerCandidates()
	offer := p.restartOffer(t)
	waitFor(t, 10*time.Second, "OnInterrupted", func() bool { return interrupted.Load() == 1 })
	if restored.Load() != 0 {
		t.Fatalf("OnRestored fired before the restart was applied")
	}

	p.completeRestart(t, offer, release)
	p.waitConnected(t)
	waitFor(t, 5*time.Second, "OnRestored", func() bool { return restored.Load() == 1 })
	p.requirePCM(t, 320*30, 10*time.Second)

	if interrupted.Load() != 1 || disconnected.Load() != 0 {
		t.Errorf("interrupted=%d disconnected=%d, want 1 and 0", interrupted.Load(), disconnected.Load())
	}
}

func TestPCMedia_ICEFailureAfterInterruption(t *testing.T) {
	if testing.Short() {
		t.Skip("involves real ICE/DTLS; skipped in -short")
	}
	var interrupted atomic.Int32
	reasons := make(chan string, 4)
	p := newLoopbackPair(t, PCMediaConfig{
		TolerateICEDisconnect:  true,
		ICEDisconnectedTimeout: time.Second,
		ICEFailedTimeout:       time.Second,
		OnInterrupted:          func() { interrupted.Add(1) },
		OnDisconnect:           func(r string) { reasons <- r },
	})

	// A caller-side restart that is never signalled drops the caller's
	// sockets without a DTLS close, like a peer that lost its network.
	p.holdCallerCandidates()
	p.restartOffer(t)
	select {
	case r := <-reasons:
		if r != "failed" {
			t.Errorf("OnDisconnect reason = %q, want failed", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("OnDisconnect never fired")
	}
	if interrupted.Load() != 1 {
		t.Errorf("OnInterrupted fired %d times before failure, want 1", interrupted.Load())
	}
}

func TestPCMedia_DisconnectNotTolerated(t *testing.T) {
	if testing.Short() {
		t.Skip("involves real ICE/DTLS; skipped in -short")
	}
	var interrupted atomic.Int32
	reasons := make(chan string, 4)
	p := newLoopbackPair(t, PCMediaConfig{
		ICEDisconnectedTimeout: time.Second,
		ICEFailedTimeout:       30 * time.Second,
		OnInterrupted:          func() { interrupted.Add(1) },
		OnDisconnect:           func(r string) { reasons <- r },
	})

	p.holdCallerCandidates()
	p.restartOffer(t)
	select {
	case r := <-reasons:
		if r != "disconnected" {
			t.Errorf("OnDisconnect reason = %q, want disconnected", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("OnDisconnect never fired")
	}
	if interrupted.Load() != 0 {
		t.Errorf("OnInterrupted fired %d times without TolerateICEDisconnect", interrupted.Load())
	}
}

func TestPCMedia_RestartICE_Rejects(t *testing.T) {
	caller, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecPCMU, Log: testLogger(), LoopbackICE: true})
	if err != nil {
		t.Fatalf("caller NewPCMedia: %v", err)
	}
	defer caller.Close()
	callee, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecPCMU, Log: testLogger(), LoopbackICE: true})
	if err != nil {
		t.Fatalf("callee NewPCMedia: %v", err)
	}
	defer callee.Close()

	offer, err := caller.PC().CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if _, err := callee.RestartICE(offer.SDP); err == nil {
		t.Error("RestartICE before any negotiation: expected error")
	}

	if err := callee.PC().SetRemoteDescription(offer); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	answer, err := callee.PC().CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := callee.PC().SetLocalDescription(answer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}

	if _, err := callee.RestartICE(offer.SDP); !errors.Is(err, ErrNotICERestart) {
		t.Errorf("same credentials: err = %v, want ErrNotICERestart", err)
	}
	if _, err := callee.RestartICE("not sdp"); !errors.Is(err, ErrInvalidOffer) {
		t.Errorf("garbage offer: err = %v, want ErrInvalidOffer", err)
	}
	if _, done := callee.DrainLocalCandidates(); !waitDone(callee, done) {
		t.Error("rejected offers must not reset the gathering-complete flag")
	}
}

// waitDone reports whether gathering completes, tolerating a first poll that
// raced the initial gathering round.
func waitDone(m *PCMedia, done bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !done && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		_, done = m.DrainLocalCandidates()
	}
	return done
}
