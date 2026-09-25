//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// renegStreamView is legStreamView plus the sample rate, which is the field
// that matters here.
type renegStreamView struct {
	ID         string `json:"id"`
	Primary    bool   `json:"primary"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
}

func primaryStream(t *testing.T, baseURL, legID string) renegStreamView {
	t.Helper()
	resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s/streams", baseURL, legID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list streams: status %d", resp.StatusCode)
	}
	var streams []renegStreamView
	decodeJSON(t, resp, &streams)
	for _, s := range streams {
		if s.Primary {
			return s
		}
	}
	t.Fatalf("no primary stream on leg %s (got %+v)", legID, streams)
	return renegStreamView{}
}

// sdpOffer builds a single-codec audio offer.
func sdpOffer(host string, port int, pt, rtpmap string) []byte {
	return []byte(strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=raw 1 2 IN IP4 %s", host),
		"s=-",
		fmt.Sprintf("c=IN IP4 %s", host),
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP %s", port, pt),
		"a=rtpmap:" + rtpmap,
		"a=sendrecv",
		"",
	}, "\r\n"))
}

// TestCodecRenegotiation_MidCallRateChange drives the whole path over a real
// dialog: VoiceBlender calls a raw UA, the UA answers PCMU (8 kHz), then
// re-INVITEs to G.722 (16 kHz) mid-call.
//
// Before, VoiceBlender answered "yes, G.722" and carried on decoding PCMU —
// the call was accepted into a codec it was not running, and the room went on
// resampling from a rate the leg no longer produced.
func TestCodecRenegotiation_MidCallRateChange(t *testing.T) {
	inst := newTestInstance(t, "instance-a")
	ua := newRawSIPClient(t, "reneg-ua")

	// Offer both, so the later G.722-only re-offer is something this leg is
	// allowed to accept.
	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     ua.contactURI("reneg"),
		"codecs":  []string{"PCMU", "G722"},
		"room_id": "reneg-room",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)

	e := ua.waitInvite(t, 5*time.Second)

	// Answer with PCMU only, keeping the dialog state we need to re-offer on.
	toTag := "reneg-uas-tag"
	answer := sip.NewResponseFromRequest(e.req, sip.StatusOK, "OK", sdpOffer(ua.host, 40000, "0", "0 PCMU/8000"))
	answer.To().Params = sip.NewParams()
	answer.To().Params.Add("tag", toTag)
	answer.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	answer.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: ua.host, Port: ua.port}})
	if err := e.tx.Respond(answer); err != nil {
		t.Fatalf("respond 200: %v", err)
	}
	close(e.done)

	waitForLegState(t, inst.baseURL(), lv.ID, "connected", 5*time.Second)
	if got := primaryStream(t, inst.baseURL(), lv.ID); got.Codec != "PCMU" || got.SampleRate != 8000 {
		t.Fatalf("before the re-INVITE: codec=%s rate=%d, want PCMU/8000", got.Codec, got.SampleRate)
	}
	t.Log("call up on PCMU at 8000 Hz")

	// Now re-INVITE to G.722. The dialog's remote target is VoiceBlender's
	// Contact; From/To swap because we are the UAS re-offering.
	contact := e.req.Contact()
	if contact == nil {
		t.Fatal("no Contact on the INVITE; cannot address an in-dialog request")
	}
	reoffer := sip.NewRequest(sip.INVITE, contact.Address)
	reoffer.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<%s>;tag=%s", e.req.To().Address.String(), toTag)))
	reoffer.AppendHeader(sip.NewHeader("To", e.req.From().Value()))
	reoffer.AppendHeader(e.req.CallID())
	reoffer.AppendHeader(&sip.CSeqHeader{SeqNo: 2, MethodName: sip.INVITE})
	reoffer.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: ua.host, Port: ua.port}})
	reoffer.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	reoffer.SetBody(sdpOffer(ua.host, 40002, "9", "9 G722/8000"))
	reoffer.SetDestination(fmt.Sprintf("127.0.0.1:%d", inst.sipPort))

	tx, err := ua.client.TransactionRequest(t.Context(), reoffer)
	if err != nil {
		t.Fatalf("send re-INVITE: %v", err)
	}
	defer tx.Terminate()

	var res *sip.Response
	select {
	case res = <-tx.Responses():
	case <-time.After(5 * time.Second):
		t.Fatal("no response to the re-INVITE")
	}
	for res != nil && res.StatusCode < 200 {
		select {
		case res = <-tx.Responses():
		case <-time.After(5 * time.Second):
			t.Fatal("no final response to the re-INVITE")
		}
	}
	if res.StatusCode != 200 {
		t.Fatalf("re-INVITE answered %d %s", res.StatusCode, res.Reason)
	}
	if !strings.Contains(string(res.Body()), "G722") {
		t.Fatalf("the answer should accept G.722:\n%s", res.Body())
	}
	t.Log("re-INVITE accepted, answer selects G.722")

	// The observable that used to lie: the leg must actually be running the
	// codec it just agreed to.
	deadline := time.Now().Add(3 * time.Second)
	var got renegStreamView
	for time.Now().Before(deadline) {
		got = primaryStream(t, inst.baseURL(), lv.ID)
		if got.Codec == "G722" && got.SampleRate == 16000 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.Codec != "G722" || got.SampleRate != 16000 {
		t.Errorf("after the re-INVITE: codec=%s rate=%d, want G722/16000", got.Codec, got.SampleRate)
	}
	waitForLegState(t, inst.baseURL(), lv.ID, "connected", 2*time.Second)
	t.Logf("leg now running %s at %d Hz, call still up", got.Codec, got.SampleRate)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), lv.ID)).Body.Close()
}
