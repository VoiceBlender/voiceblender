//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// TestG722_DTMF places a G.722 call and verifies out-of-band DTMF (RFC 4733)
// flows in both directions. G.722 samples at 16 kHz but its RTP clock is 8 kHz
// (RFC 3551), so the telephone-event must stay at 8 kHz — this guards against
// the 16 kHz sample rate leaking into the DTMF clock the way it would for
// AMR-WB.
func TestG722_DTMF(t *testing.T) {
	instA := newTestInstanceWithCodecs(t, "g722-dtmf-a", []codec.CodecType{codec.CodecG722})
	instB := newTestInstanceWithCodecs(t, "g722-dtmf-b", []codec.CodecType{codec.CodecG722})

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"G722"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
		map[string]interface{}{"codec": "G722"},
	)
	if answerResp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(answerResp.Body)
		answerResp.Body.Close()
		t.Fatalf("answer with codec=G722: status %d, body=%s", answerResp.StatusCode, body)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	// Caller emits DTMF over the G.722 leg; the far end must receive it.
	sendDTMFFrom(t, instA, outbound.ID, "5")
	waitForDTMF(t, instB, inbound.ID, "5")

	// And the reverse direction.
	sendDTMFFrom(t, instB, inbound.ID, "7")
	waitForDTMF(t, instA, outbound.ID, "7")
}
