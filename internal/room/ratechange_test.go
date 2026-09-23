package room

import (
	"io"
	"sync"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// countingWriter stands in for a detector attached to the leg's filtered audio.
type countingWriter struct {
	mu sync.Mutex
	n  int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.n += len(p)
	c.mu.Unlock()
	return len(p), nil
}

func (c *countingWriter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestRebuildLegMediaFollowsTheNewRate: a leg that renegotiates to a different
// codec mid-call needs its resamplers resized. They were built from the rate
// the leg had when it joined, so without this the room keeps converting from a
// rate the leg no longer produces and the audio plays at the wrong speed.
func TestRebuildLegMediaFollowsTheNewRate(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	r, err := mgr.Create("room-rate", "", 16000)
	if err != nil {
		t.Fatal(err)
	}

	l := newMockLeg("switcher")
	l.sampleRate = 8000
	l.reader = readerOfZeros{}
	l.writer = io.Discard
	r.AddLeg(l)

	before := r.legParts[l.ID()]
	if before == nil {
		t.Fatal("leg should have a mixer participant")
	}

	// The re-INVITE lands: the leg now produces 16 kHz.
	l.sampleRate = 16000
	if !r.RebuildLegMedia(l.ID()) {
		t.Fatal("RebuildLegMedia reported no rebuild")
	}

	after := r.legParts[l.ID()]
	if after == nil {
		t.Fatal("the leg lost its mixer participant")
	}
	if after == before {
		t.Error("the participant must be rebuilt: its resamplers are sized for the old rate")
	}
	t.Log("participant rebuilt at the leg's new rate")
}

// TestRebuildLegMediaKeepsChainAndObservers is the part that is easy to get
// wrong and silent when you do. The chain carries the filters actually running
// — which a mid-call change may have altered — and the observers are how voice
// activity and answering-machine detection see the leg at all. Dropping either
// on a rebuild leaves a call that sounds fine and has stopped detecting.
func TestRebuildLegMediaKeepsChainAndObservers(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	r, err := mgr.Create("room-rate-chain", "", 16000)
	if err != nil {
		t.Fatal(err)
	}

	l := newMockLeg("filtered")
	l.sampleRate = 8000
	l.reader = readerOfZeros{}
	l.writer = io.Discard
	l.filters = []audiofilter.Spec{{Type: "bandpass"}}
	r.AddLeg(l)

	// A mid-call filter change, so the running chain differs from the leg's
	// configured one and only the running chain is correct to carry over.
	if _, err := r.SetLegFilters(l.ID(), []audiofilter.Spec{{Type: "gain", Params: audiofilter.Params{"volume": 2}}}); err != nil {
		t.Fatal(err)
	}
	obs := &countingWriter{}
	if !r.SetLegAudioObserver(l.ID(), "vad", obs) {
		t.Fatal("failed to attach the observer")
	}

	l.sampleRate = 16000
	if !r.RebuildLegMedia(l.ID()) {
		t.Fatal("RebuildLegMedia reported no rebuild")
	}

	got, ok := r.LegFilters(l.ID())
	if !ok {
		t.Fatal("the rebuilt leg has no chain")
	}
	if len(got) != 1 || got[0].Type != "gain" {
		t.Errorf("chain after the rebuild = %+v, want the running [gain], not the configured [bandpass]", got)
	}
	rd := r.legFilterReaders[l.ID()]
	if rd == nil {
		t.Fatal("no chain reader after the rebuild")
	}
	if _, kept := rd.Observers()["vad"]; !kept {
		t.Error("the observer was dropped: detection would stop silently for the rest of the call")
	}
	t.Log("running chain and its observers carried across the rebuild")
}

// TestRateChangeWatchIsClearedOnLeave: the callback closes over this room. A
// leg that moved on must not have its old room rebuilding participants it no
// longer owns.
func TestRateChangeWatchIsClearedOnLeave(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	r, err := mgr.Create("room-watch", "", 16000)
	if err != nil {
		t.Fatal(err)
	}

	l := &rateNotifyLeg{mockLeg: newMockLeg("notifier")}
	l.reader = readerOfZeros{}
	l.writer = io.Discard
	r.AddLeg(l)
	if l.callback() == nil {
		t.Fatal("joining a room should install a rate-change watch")
	}

	r.RemoveLeg(l.ID())
	if l.callback() != nil {
		t.Error("leaving a room must clear the watch, or the old room rebuilds for a leg it lost")
	}
	t.Log("rate-change watch installed on join and cleared on leave")
}

// rateNotifyLeg is a mockLeg that also implements mediaRateNotifier.
type rateNotifyLeg struct {
	*mockLeg
	mu sync.Mutex
	fn func(string, int)
}

func (l *rateNotifyLeg) SetOnMediaRateChange(fn func(streamID string, rate int)) {
	l.mu.Lock()
	l.fn = fn
	l.mu.Unlock()
}

func (l *rateNotifyLeg) callback() func(string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fn
}

// TestRebuildRoutesToTheStreamThatMoved: a multi-stream leg negotiates each
// m-line separately, so a rate change on one stream must rebuild that stream's
// participant and leave the leg's own alone. Rebuilding the wrong one would
// resize a participant that never moved and leave the one that did wrong.
func TestRebuildRoutesToTheStreamThatMoved(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	r, err := mgr.Create("room-stream-rate", "", 16000)
	if err != nil {
		t.Fatal(err)
	}

	sm := leg.StreamMedia{
		ID:         "1",
		SampleRate: 8000,
		Direction:  "sendrecv",
		Reader:     readerOfZeros{},
		Writer:     io.Discard,
	}
	l := newStreamMockLeg("multi", sm)
	l.reader = readerOfZeros{}
	l.writer = io.Discard
	r.AddLeg(l)

	p, ok := r.AddLegStream(l, "1", "")
	if !ok {
		t.Fatal("failed to attach the secondary stream")
	}
	legPart := r.legParts[l.ID()]

	// The secondary stream renegotiates to 16 kHz.
	sm.SampleRate = 16000
	l.streams["1"] = sm
	r.rebuildForRateChange(l.ID(), "1")

	pid := StreamParticipantID(l.ID(), "1")
	if got := r.legStreams[pid]; got == nil || got.part == p {
		t.Error("the stream that changed rate should have been rebuilt")
	}
	if r.legParts[l.ID()] != legPart {
		t.Error("the leg's own participant did not change rate and must be left alone")
	}
	t.Log("rate change routed to the stream that moved, leg participant untouched")
}

// TestRebuildForPrimaryFallsBackToTheLeg: the primary stream has no participant
// of its own — it is the leg participant — so the routing must fall through.
func TestRebuildForPrimaryFallsBackToTheLeg(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	r, err := mgr.Create("room-primary-rate", "", 16000)
	if err != nil {
		t.Fatal(err)
	}

	l := newMockLeg("primary")
	l.sampleRate = 8000
	l.reader = readerOfZeros{}
	l.writer = io.Discard
	r.AddLeg(l)
	before := r.legParts[l.ID()]

	l.sampleRate = 16000
	r.rebuildForRateChange(l.ID(), "0")

	if r.legParts[l.ID()] == before {
		t.Error("a primary-stream rate change must rebuild the leg participant")
	}
	t.Log("primary-stream rate change fell through to the leg participant")
}
