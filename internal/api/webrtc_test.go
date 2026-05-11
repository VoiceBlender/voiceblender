package api

import (
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/pion/webrtc/v4"
)

// makeClientOffer creates a pion peer connection on the test (client) side
// and returns a valid SDP offer that can be fed into doWebRTCOffer. The
// caller owns the returned PC and must Close() it.
func makeClientOffer(t *testing.T) (*webrtc.PeerConnection, string) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		pc.Close()
		t.Fatalf("AddTransceiverFromKind: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		t.Fatalf("SetLocalDescription: %v", err)
	}
	return pc, pc.LocalDescription().SDP
}

func TestDoWebRTCOffer_HappyPath(t *testing.T) {
	s := newTestServer(t)
	clientPC, sdp := makeClientOffer(t)
	defer clientPC.Close()

	res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		t.Fatalf("doWebRTCOffer: %v", err)
	}
	if res.LegID == "" {
		t.Fatal("empty leg_id")
	}
	if !strings.HasPrefix(res.SDP, "v=0") {
		t.Fatalf("expected SDP answer starting with v=0, got: %q", res.SDP)
	}
	got, ok := s.LegMgr.Get(res.LegID)
	if !ok {
		t.Fatal("leg not registered with manager")
	}
	if _, ok := got.(*leg.WebRTCLeg); !ok {
		t.Fatalf("registered leg is %T, want *leg.WebRTCLeg", got)
	}
}

func TestDoWebRTCOffer_InvalidSDP(t *testing.T) {
	s := newTestServer(t)
	_, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: "not an sdp"})
	if err == nil {
		t.Fatal("expected error for invalid SDP")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("got %T %v, want *apiError", err, err)
	}
	if ae.Code != 400 {
		t.Fatalf("Code = %d, want 400", ae.Code)
	}
}

func TestDoWebRTCAddCandidate_NotFound(t *testing.T) {
	s := newTestServer(t)
	err := s.doWebRTCAddCandidate("does-not-exist", webrtc.ICECandidateInit{
		Candidate: "candidate:1 1 udp 1 1.1.1.1 1 typ host",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("got %T %v, want *apiError", err, err)
	}
	if ae.Code != 404 {
		t.Fatalf("Code = %d, want 404", ae.Code)
	}
}

func TestDoWebRTCGetCandidates_NotFound(t *testing.T) {
	s := newTestServer(t)
	_, err := s.doWebRTCGetCandidates("does-not-exist")
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("got %T %v, want *apiError", err, err)
	}
	if ae.Code != 404 {
		t.Fatalf("Code = %d, want 404", ae.Code)
	}
}

// TestDoWebRTCGetCandidates_HappyPath verifies that after a successful offer
// the server begins gathering ICE candidates and the drain endpoint surfaces
// them. Host candidates are produced without external network access, so
// this should be deterministic in any environment with a usable loopback.
func TestDoWebRTCGetCandidates_HappyPath(t *testing.T) {
	s := newTestServer(t)
	clientPC, sdp := makeClientOffer(t)
	defer clientPC.Close()

	res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		t.Fatalf("doWebRTCOffer: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.doWebRTCGetCandidates(res.LegID)
		if err != nil {
			t.Fatalf("doWebRTCGetCandidates: %v", err)
		}
		if len(got.Candidates) > 0 || got.Done {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no ICE candidates gathered within deadline")
}

func TestDoWebRTCAddCandidate_HappyPath(t *testing.T) {
	s := newTestServer(t)
	clientPC, sdp := makeClientOffer(t)
	defer clientPC.Close()
	res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		t.Fatalf("doWebRTCOffer: %v", err)
	}

	gather := webrtc.GatheringCompletePromise(clientPC)
	select {
	case <-gather:
	case <-time.After(3 * time.Second):
		t.Fatal("client ICE gathering timed out")
	}
	desc := clientPC.LocalDescription()
	if desc == nil {
		t.Fatal("client local description nil")
	}
	var candStr string
	for _, line := range strings.Split(desc.SDP, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=candidate:") {
			candStr = strings.TrimPrefix(line, "a=")
			break
		}
	}
	if candStr == "" {
		t.Fatal("no a=candidate line in client SDP")
	}
	mid := "0"
	idx := uint16(0)
	if err := s.doWebRTCAddCandidate(res.LegID, webrtc.ICECandidateInit{
		Candidate:     candStr,
		SDPMid:        &mid,
		SDPMLineIndex: &idx,
	}); err != nil {
		t.Fatalf("doWebRTCAddCandidate: %v", err)
	}
}

// TestVSIMetadata_WebRTCRegistered guards the asyncapi-gen contract in CLAUDE.md:
// every VSI command must appear in VSICommandsMetadata.
func TestVSIMetadata_WebRTCRegistered(t *testing.T) {
	want := map[string]bool{
		"webrtc_offer":          false,
		"webrtc_add_candidate":  false,
		"webrtc_get_candidates": false,
	}
	for _, cmd := range VSICommandsMetadata() {
		if _, ok := want[cmd.Name]; ok {
			want[cmd.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("VSI command %q missing from VSICommandsMetadata", name)
		}
	}
}
