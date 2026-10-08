//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// inboundOffer is an offer listing telephone-event at two clock rates, the
// 48 kHz one on the lower payload type, with PCMU as the first codec.
func inboundOffer(extra ...string) []byte {
	lines := []string{
		"v=0",
		"o=raw 1 1 IN IP4 127.0.0.1",
		"s=-",
		"c=IN IP4 127.0.0.1",
		"t=0 0",
		"m=audio 40002 RTP/AVP 0 96 97 101",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:96 opus/48000/2",
		"a=rtpmap:97 telephone-event/48000",
		"a=fmtp:97 0-15",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-15",
		"a=sendrecv",
	}
	lines = append(lines, extra...)
	return []byte(strings.Join(append(lines, ""), "\r\n"))
}

// answerInboundCall places a call to inst from a bare SIP client, answers it
// through the API and returns the dialog with the 200 OK in hand, not yet ACKed.
func answerInboundCall(t *testing.T, ctx context.Context, inst *testInstance, recipient sip.Uri, offer []byte) (*sipgo.DialogClientSession, legView) {
	t.Helper()
	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("new UA: %v", err)
	}
	t.Cleanup(func() { ua.Close() })
	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	dua := sipgo.DialogUA{
		Client:     client,
		ContactHDR: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "caller", Host: "127.0.0.1", Port: 5060}},
	}
	dialog, err := dua.Invite(ctx, recipient, offer, sip.NewHeader("Content-Type", "application/sdp"))
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}

	inbound := waitForInboundLeg(t, inst.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", inst.baseURL(), inbound.ID), nil)
	answerResp.Body.Close()
	if err := dialog.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	return dialog, inbound
}

// TestSIPInboundAnswer_TCPContact verifies that a call arriving over TCP is
// answered with a Contact naming TCP. The caller sends its ACK and BYE to that
// Contact, so one without the transport sends them over UDP instead.
func TestSIPInboundAnswer_TCPContact(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "tcp-inbound", func(c *config.Config) { c.SIPTCPEnabled = true })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	recipient := sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort, UriParams: sip.NewParams()}
	recipient.UriParams.Add("transport", "tcp")
	dialog, inbound := answerInboundCall(t, ctx, inst, recipient, inboundOffer())

	if via := dialog.InviteRequest.Via(); via == nil || !strings.EqualFold(via.Transport, "TCP") {
		t.Fatalf("INVITE was not sent over TCP: Via = %v", via)
	}
	contact := dialog.InviteResponse.Contact()
	if contact == nil {
		t.Fatal("200 OK carries no Contact")
	}
	if got, _ := contact.Address.UriParams.Get("transport"); !strings.EqualFold(got, "tcp") {
		t.Errorf("200 OK Contact = %s, want ;transport=tcp", contact.Address.String())
	}
	if contact.Address.Port != inst.sipPort {
		t.Errorf("200 OK Contact port = %d, want %d", contact.Address.Port, inst.sipPort)
	}

	if err := dialog.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)

	if err := dialog.Bye(ctx); err != nil {
		t.Fatalf("BYE: %v", err)
	}
	expectDisconnect(t, inst, inbound.ID)
}

// TestSIPInboundAnswer_UDPContactUnchanged pins the default: a UDP call keeps
// the plain Contact it has always been answered with.
func TestSIPInboundAnswer_UDPContactUnchanged(t *testing.T) {
	inst := newTestInstance(t, "udp-inbound")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	recipient := sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}
	dialog, inbound := answerInboundCall(t, ctx, inst, recipient, inboundOffer())

	contact := dialog.InviteResponse.Contact()
	if contact == nil {
		t.Fatal("200 OK carries no Contact")
	}
	if want := fmt.Sprintf("sip:127.0.0.1:%d", inst.sipPort); contact.Address.String() != want {
		t.Errorf("200 OK Contact = %s, want %s", contact.Address.String(), want)
	}
	if err := dialog.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)
	hangup(t, inst, inbound.ID)
}

// TestSIPInboundAnswer_SDPMirrorsOffer verifies the answer stays within what
// the offer allows: telephone-event at the selected codec's clock rate, and
// a=rtcp-mux only when it was offered.
func TestSIPInboundAnswer_SDPMirrorsOffer(t *testing.T) {
	tests := []struct {
		name    string
		extra   []string
		wantMux bool
	}{
		{"offer without rtcp-mux", nil, false},
		{"offer with rtcp-mux", []string{"a=rtcp-mux"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := newTestInstance(t, "sdp-inbound")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			recipient := sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}
			dialog, inbound := answerInboundCall(t, ctx, inst, recipient, inboundOffer(tt.extra...))
			answer := string(dialog.InviteResponse.Body())

			if !strings.Contains(answer, " RTP/AVP 0 101\r\n") {
				t.Errorf("want PCMU with telephone-event on PT 101:\n%s", answer)
			}
			if !strings.Contains(answer, "a=rtpmap:101 telephone-event/8000") {
				t.Errorf("answer lacks telephone-event/8000:\n%s", answer)
			}
			if strings.Contains(answer, "telephone-event/48000") {
				t.Errorf("PCMU answered alongside telephone-event/48000:\n%s", answer)
			}
			if got := strings.Contains(answer, "a=rtcp-mux"); got != tt.wantMux {
				t.Errorf("answer rtcp-mux = %v, want %v:\n%s", got, tt.wantMux, answer)
			}

			if err := dialog.Ack(ctx); err != nil {
				t.Fatalf("ACK: %v", err)
			}
			waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)
			hangup(t, inst, inbound.ID)
		})
	}
}
