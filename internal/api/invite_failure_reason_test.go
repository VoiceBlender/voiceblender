package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestInviteFailureReason_NotAcceptable(t *testing.T) {
	ctx := context.Background()
	err := fmt.Errorf("all INVITE branches failed: %w", fmt.Errorf("%w: audio stream 0", sipmod.ErrNotAcceptable))
	if got := inviteFailureReason(err, false, ctx); got != "not_acceptable" {
		t.Errorf("SRTP answer mismatch: reason %q, want not_acceptable", got)
	}
	if got := inviteFailureReason(errors.New("boom"), false, ctx); got != "invite_failed" {
		t.Errorf("other error: reason %q, want invite_failed", got)
	}
}
