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
