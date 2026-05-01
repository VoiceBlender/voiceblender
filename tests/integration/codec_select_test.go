//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestCodecSelect_RingingExposesOffer verifies that the leg.ringing event
// payload includes the codecs offered by the remote SDP, in priority order.
func TestCodecSelect_RingingExposesOffer(t *testing.T) {
	instA := newTestInstanceWithCodecs(t, "instance-a", []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA})
	instB := newTestInstanceWithCodecs(t, "instance-b", []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA})

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU", "PCMA"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	bRing := instB.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
	d := bRing.Data.(*events.LegRingingData)

	if len(d.OfferedCodecs) < 2 {
		t.Fatalf("OfferedCodecs = %#v, want at least 2 entries (PCMU, PCMA)", d.OfferedCodecs)
	}
	if d.OfferedCodecs[0].Name != "PCMU" || d.OfferedCodecs[0].Priority != 1 {
		t.Errorf("priority 1 = %+v, want PCMU/1", d.OfferedCodecs[0])
	}
	if d.OfferedCodecs[1].Name != "PCMA" || d.OfferedCodecs[1].Priority != 2 {
		t.Errorf("priority 2 = %+v, want PCMA/2", d.OfferedCodecs[1])
	}
	if d.OfferedCodecs[0].PayloadType != 0 || d.OfferedCodecs[0].ClockRate != 8000 {
		t.Errorf("PCMU PT/rate = %d/%d, want 0/8000", d.OfferedCodecs[0].PayloadType, d.OfferedCodecs[0].ClockRate)
	}
}

// TestCodecSelect_AnswerWithExplicitCodec verifies that the answer endpoint
// accepts an explicit codec from the offered list and that the call still
// connects.
func TestCodecSelect_AnswerWithExplicitCodec(t *testing.T) {
	instA := newTestInstanceWithCodecs(t, "instance-a", []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA})
	instB := newTestInstanceWithCodecs(t, "instance-b", []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA})

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU", "PCMA"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	// Answer the inbound leg with an explicit, non-default codec choice.
	answerResp := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
		map[string]interface{}{"codec": "PCMA"},
	)
	if answerResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(answerResp.Body)
		answerResp.Body.Close()
		t.Fatalf("answer with codec=PCMA: status %d, body=%s", answerResp.StatusCode, body)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)
}

// TestCodecSelect_AnswerRejectsCodecNotInOffer verifies that the answer
// endpoint returns 400 when the requested codec is not in the remote offer.
func TestCodecSelect_AnswerRejectsCodecNotInOffer(t *testing.T) {
	// A advertises only PCMU; B requests answer with G722, which was not offered.
	instA := newTestInstanceWithCodecs(t, "instance-a", []codec.CodecType{codec.CodecPCMU})
	instB := newTestInstanceWithCodecs(t, "instance-b", []codec.CodecType{codec.CodecPCMU, codec.CodecG722})

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	resp := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
		map[string]interface{}{"codec": "G722"},
	)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("answer with unsupported codec: status %d, body=%s, want 400", resp.StatusCode, body)
	}
}
