package api

import (
	"io"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

// TestAMDFeedFollowsTheEffectiveChain pins two things AMD must get right: it
// never enables filtering on its own, and it follows the chain the leg will
// actually run — including one inherited from AUDIO_FILTERS, which leaves the
// leg's own field empty.
func TestAMDFeedFollowsTheEffectiveChain(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		name       string
		legFilters []audiofilter.Spec
		serverWide []audiofilter.Spec
		wantChain  bool
	}{
		{"nothing configured", nil, nil, false},
		{"leg chose corrective filters", []audiofilter.Spec{{Type: "bandpass"}}, nil, true},
		{"leg inherits the server default", nil, []audiofilter.Spec{{Type: "bandpass"}}, true},
		{"leg opted out of a server default", []audiofilter.Spec{}, []audiofilter.Spec{{Type: "bandpass"}}, false},
		{"effects only are not fed to AMD", []audiofilter.Spec{{Type: "robotic"}}, nil, false},
		{"effects dropped, corrective kept", []audiofilter.Spec{{Type: "robotic"}, {Type: "bandpass"}}, nil, true},
	}
	for _, c := range cases {
		s.RoomMgr.SetDefaultFilters(c.serverWide)
		l := &apiMockLeg{id: "amd-leg", filters: c.legFilters}

		w := s.amdTapWriter(l, io.Discard)
		// The plain resampling writer also has a Close, so the concrete type
		// is the only reliable discriminator.
		fw, filtered := w.(*audiofilter.Writer)
		if filtered != c.wantChain {
			t.Errorf("%s: filtered feed = %v, want %v", c.name, filtered, c.wantChain)
			continue
		}
		if filtered {
			fw.Close()
		}
		t.Logf("%-38s -> filtered feed: %v", c.name, filtered)
	}
}

// TestAMDFeedSharesTheRoomChain covers the point of amdFeed: when the leg is
// in a room, its chain is already denoising for the mixer, recordings and
// speech-to-text. AMD observes that rather than running a second denoise pass
// over the same samples — which is what it did before, at the cost of a
// duplicate kernel state for the whole analysis window.
func TestAMDFeedSharesTheRoomChain(t *testing.T) {
	s := newTestServer(t)
	s.RoomMgr.SetDefaultFilters([]audiofilter.Spec{{Type: "bandpass"}})

	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, 320)
		for {
			if _, err := pr.Read(buf); err != nil {
				return
			}
		}
	}()
	defer pw.Close()
	l := &apiMockLeg{id: "amd-shared", reader: pr, writer: io.Discard}
	s.LegMgr.Add(l)

	// Not in a room: nothing to share, so AMD builds its own chain.
	tap, detach := s.amdFeed(l, io.Discard)
	if tap == nil {
		t.Fatal("a roomless leg must get its own feed")
	}
	if detach != nil {
		t.Error("the private-chain path should not report an observer to detach")
	}
	if c, ok := tap.(io.Closer); ok {
		c.Close()
	}
	t.Log("roomless leg: private chain, as before")

	// In a room: share the chain that is already running.
	if _, err := s.RoomMgr.Create("amd-room", "", 16000); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMgr.AddLeg("amd-room", l.ID()); err != nil {
		t.Fatal(err)
	}
	tap, detach = s.amdFeed(l, io.Discard)
	if tap != nil {
		t.Error("a leg in a room should observe the room's chain, not build a second one")
	}
	if detach == nil {
		t.Fatal("observing the room's chain must report a detach")
	}
	detach()
	t.Log("leg in a room: observes the shared chain, no duplicate denoise pass")
}
