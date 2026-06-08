package lkmedia

import (
	"strings"
	"testing"
)

// Synthetic LK subscriber offer with three m-sections (app/audio/video)
// bundled under mids 0, 1, 2.
const testOffer = "v=0\r\n" +
	"o=- 1 1 IN IP4 0.0.0.0\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0 1 2\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=mid:0\r\n" +
	"a=sctp-port:5000\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=mid:1\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=sendrecv\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=mid:2\r\n" +
	"a=rtpmap:96 VP8/90000\r\n" +
	"a=sendrecv\r\n"

// Answer mirroring pion v4's actual output when the MediaEngine has no
// video codec — the rejected video m-section has port 0 and no mid.
const testAnswerMissingMid = "v=0\r\n" +
	"o=- 2 1 IN IP4 0.0.0.0\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0 1\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=mid:0\r\n" +
	"a=sctp-port:5000\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=mid:1\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=recvonly\r\n" +
	"m=video 0 UDP/TLS/RTP/SAVPF 0\r\n" +
	"c=IN IP4 0.0.0.0\r\n"

func TestFixRejectedMids_InjectsMissingMid(t *testing.T) {
	got, err := fixRejectedMids(testOffer, testAnswerMissingMid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The injected mid for the rejected video section must be 2 (matches
	// the offer's video m-section position).
	videoIdx := strings.Index(got, "m=video 0")
	if videoIdx < 0 {
		t.Fatalf("rejected video section missing from output:\n%s", got)
	}
	tail := got[videoIdx:]
	if !strings.Contains(tail, "a=mid:2") {
		t.Fatalf("video section did not gain a=mid:2 (full tail follows):\n%s", tail)
	}
	// The accepted audio section must keep its original mid intact.
	if !strings.Contains(got, "a=mid:1") {
		t.Fatalf("audio section lost its mid:\n%s", got)
	}
}

func TestFixRejectedMids_NoOpWhenAllMidsPresent(t *testing.T) {
	// Same answer but with the mid already present on the rejected
	// section. Function should return the input unchanged.
	answer := strings.Replace(
		testAnswerMissingMid,
		"m=video 0 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\n",
		"m=video 0 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\na=mid:2\r\n",
		1,
	)
	got, err := fixRejectedMids(testOffer, answer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != answer {
		t.Fatalf("expected SDP unchanged, but got:\n%s", got)
	}
}

func TestFixRejectedMids_NoOpWhenNoRejectedSections(t *testing.T) {
	// Both offer m-sections accepted with non-zero ports — fixup must
	// be a no-op (no mid injection needed, exact bytes preserved).
	smallOffer := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\n" +
		"a=group:BUNDLE 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\nc=IN IP4 0.0.0.0\r\n" +
		"a=mid:0\r\na=rtpmap:111 opus/48000/2\r\na=sendrecv\r\n"
	smallAnswer := "v=0\r\no=- 2 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\n" +
		"a=group:BUNDLE 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\nc=IN IP4 0.0.0.0\r\n" +
		"a=mid:0\r\na=rtpmap:111 opus/48000/2\r\na=recvonly\r\n"
	got, err := fixRejectedMids(smallOffer, smallAnswer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != smallAnswer {
		t.Fatalf("expected SDP unchanged, got:\n%s", got)
	}
}
