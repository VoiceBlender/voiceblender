package leg

import (
	"bytes"
	"testing"
)

// TestSIPLegClearAMDTapIfOnlyClearsItsOwnTap pins the tap's identity check.
// AMD can be started twice on one leg, and the second start overwrites the tap
// slot. The superseded analysis still reaches its own deadline and calls clear,
// which must be a no-op: clearing unconditionally would starve the live
// analysis of frames mid-window.
func TestSIPLegClearAMDTapIfOnlyClearsItsOwnTap(t *testing.T) {
	first := &bytes.Buffer{}
	second := &bytes.Buffer{}

	l := &SIPLeg{}
	l.SetAMDTap(first)
	l.SetAMDTap(second) // a second AMD start supersedes the first

	if l.ClearAMDTapIf(first) {
		t.Error("clearing a superseded tap must report that it did not own the slot")
	}
	if l.amdTap != second {
		t.Fatal("a superseded tap must not clear the live tap")
	}

	if !l.ClearAMDTapIf(second) {
		t.Error("the installed tap must report that it owned the slot")
	}
	if l.amdTap != nil {
		t.Error("the installed tap must clear the slot")
	}
}
