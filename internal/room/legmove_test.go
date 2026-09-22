package room

import (
	"context"
	"io"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

func TestLegMoveKeepsFiltersAndReleasesState(t *testing.T) {
	if err := denoise.Install(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	defer denoise.Shutdown()

	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	// The chain-ready hook is how an observer (voice activity detection)
	// attaches to filtered audio: the detector is created before the leg joins
	// a room, so attaching once at start would leave it on the pre-filter tap,
	// and a move builds a new chain that would otherwise have no observer.
	var chainReady []string
	mgr.SetOnLegChainReady(func(legID string, obs ChainObserver) {
		chainReady = append(chainReady, legID)
	})

	a, _ := mgr.Create("room-a", "", 16000)
	b, _ := mgr.Create("room-b", "", 16000)

	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, 320)
		for {
			if _, err := pr.Read(buf); err != nil {
				return
			}
		}
	}()
	l := &mockLeg{
		id:      "leg-1",
		filters: []audiofilter.Spec{{Type: "denoise"}},
		reader:  pr,
		writer:  io.Discard,
	}
	legMgr.Add(l)

	live := func() int { n, _ := denoise.Stats(); return n }

	a.AddLeg(l)
	if live() != 1 {
		t.Fatalf("joining a room should build the chain: %d live states", live())
	}
	if len(chainReady) != 1 || chainReady[0] != l.ID() {
		t.Fatalf("the chain-ready hook should fire on join, got %v", chainReady)
	}

	// A move detaches from one room and adds to the other. The old chain must
	// hand its kernel state back, or every move leaks one.
	a.DetachLeg(l.ID())
	if live() != 0 {
		t.Errorf("detaching leaked %d denoise state(s)", live())
	}

	b.AddLeg(l)
	if live() != 1 {
		t.Fatalf("the leg should be denoising again in its new room: %d live states", live())
	}
	// The chain travels with the leg, so the new room rebuilds the same one.
	if got, ok := b.LegFilters(l.ID()); !ok || len(got) != 1 || got[0].Type != "denoise" {
		t.Errorf("new room should run the leg's chain, got %+v (found=%v)", got, ok)
	}
	if len(chainReady) != 2 {
		t.Fatalf("the hook should fire again after a move, or an observer is left on a dead chain: %v", chainReady)
	}
	t.Logf("moved room-a -> room-b: chain rebuilt, hook fired %d times, %d live state",
		len(chainReady), live())

	b.DetachLeg(l.ID())
	if live() != 0 {
		t.Errorf("final detach leaked %d denoise state(s)", live())
	}

	// The leg's own reader must survive the move: the mixer closes the
	// participant reader on teardown, and closing the leg's source would leave
	// it silent wherever it went next.
	if _, err := pw.Write(make([]byte, 2)); err != nil {
		t.Errorf("the leg's audio source was closed by the move: %v", err)
	}
}
