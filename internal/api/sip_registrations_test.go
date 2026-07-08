package api

import (
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// A zero-value engine has no registrar wired, so doDeleteRegistration must
// report 404 "registrar not enabled" rather than panicking. The registrar-
// backed paths (happy, contact-specific, unknown AOR) are covered end-to-end
// by tests/integration/sip_registration_vsi_test.go with a live registrar.
func TestDoDeleteRegistration_RegistrarDisabled(t *testing.T) {
	s := &Server{SIPEngine: &sipmod.Engine{}}
	err := s.doDeleteRegistration("sip:alice@vb.test", "")
	if err == nil {
		t.Fatal("expected error when registrar is not enabled")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("error type = %T, want *apiError", err)
	}
	if ae.Code != 404 {
		t.Errorf("code = %d, want 404", ae.Code)
	}
}
