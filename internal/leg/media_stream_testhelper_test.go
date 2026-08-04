package leg

import (
	"context"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// newTestSIPLeg builds a bare leg with its primary stream initialized and a
// live context, matching what the real constructors guarantee, for tests that
// exercise media state without a dialog.
func newTestSIPLeg(c codec.CodecType) *SIPLeg {
	l := &SIPLeg{}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.initPrimaryStream()
	l.prim.codecType = c
	return l
}
