//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// natInstance advertises an address no peer can reach, the way a server behind
// NAT advertises its public IP, and optionally treats loopback as local.
func natInstance(t *testing.T, name, localNets string) *testInstance {
	t.Helper()
	return newTestInstanceWithOpts(t, name, func(c *config.Config) {
		c.SIPExternalIP = "203.0.113.9"
		c.SIPLocalNets = localNets
	})
}

func expectDisconnect(t *testing.T, inst *testInstance, legID string) {
	t.Helper()
	inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 5*time.Second)
}

func hangup(t *testing.T, inst *testInstance, legID string) {
	t.Helper()
	resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete leg: status %d", resp.StatusCode)
	}
}

// TestSIPLocalNets_OutboundRemoteHangup verifies that a peer on a local
// network is given a Contact it can reach: the BYE it sends lands, where it
// would otherwise go to SIP_EXTERNAL_IP and be lost.
func TestSIPLocalNets_OutboundRemoteHangup(t *testing.T) {
	instA := natInstance(t, "instance-a", "127.0.0.0/8")
	instB := newTestInstance(t, "instance-b")
	outboundID, inboundID := establishCall(t, instA, instB)

	hangup(t, instB, inboundID)
	expectDisconnect(t, instA, outboundID)
}

// TestSIPLocalNets_InboundRemoteHangup covers the answering side: the Contact
// in our 200 OK is what the caller's ACK and BYE are sent to.
func TestSIPLocalNets_InboundRemoteHangup(t *testing.T) {
	instA := natInstance(t, "instance-a", "127.0.0.0/8")
	instB := newTestInstance(t, "instance-b")
	outboundID, inboundID := establishCall(t, instB, instA)

	hangup(t, instB, outboundID)
	expectDisconnect(t, instA, inboundID)
}

// assertAdvertised checks the Contact host and the SDP c= address of a message
// VoiceBlender sent.
func assertAdvertised(t *testing.T, what string, contact *sip.ContactHeader, body []byte, want string) {
	t.Helper()
	if contact == nil {
		t.Errorf("%s: no Contact header", what)
	} else if contact.Address.Host != want {
		t.Errorf("%s: Contact host = %q, want %q", what, contact.Address.Host, want)
	}
	if line := "c=IN IP4 " + want; !strings.Contains(string(body), line) {
		t.Errorf("%s: SDP lacks %q:\n%s", what, line, body)
	}
}

// TestSIPLocalNets_RegisteredPhone dials a registered phone and inspects what
// it is sent: the INVITE, and the re-INVITE that puts it on hold, must both
// advertise the local address. The re-INVITE matters because it is a target
// refresh, so the external address there would undo the INVITE's Contact.
func TestSIPLocalNets_RegisteredPhone(t *testing.T) {
	tests := []struct {
		name      string
		localNets string
		want      string
	}{
		{"local network", "127.0.0.0/8", "127.0.0.1"},
		{"no local networks", "", "203.0.113.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := natInstance(t, "nat", tt.localNets)
			cli := newRawSIPClient(t, "alice-ua")
			cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)
			inst.collector.waitForMatch(t, events.SIPRegistrationActive, nil, time.Second)

			createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
				"type":   "sip",
				"to":     "sip:alice@vb.test",
				"from":   "support",
				"codecs": []string{"PCMU"},
			})
			if createResp.StatusCode != http.StatusCreated {
				t.Fatalf("create leg: %d", createResp.StatusCode)
			}
			var outbound legView
			decodeJSON(t, createResp, &outbound)

			invite := cli.waitInvite(t, 3*time.Second)
			assertAdvertised(t, "INVITE", invite.req.Contact(), invite.req.Body(), tt.want)
			cli.answerInvite(t, invite)
			waitForLegState(t, inst.baseURL(), outbound.ID, "connected", 5*time.Second)

			holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", inst.baseURL(), outbound.ID), nil)
			holdResp.Body.Close()
			reinvite := cli.waitInvite(t, 3*time.Second)
			assertAdvertised(t, "re-INVITE", reinvite.req.Contact(), reinvite.req.Body(), tt.want)
			cli.answerInvite(t, reinvite)
		})
	}
}

// TestSIPLocalNets_CallFromLocalPhone covers a call the phone places: the 200
// OK it gets back, and a later re-INVITE from us, must advertise the local
// address, or its ACK and BYE have nowhere to go.
func TestSIPLocalNets_CallFromLocalPhone(t *testing.T) {
	inst := natInstance(t, "nat", "127.0.0.0/8")
	cli := newRawSIPClient(t, "alice-ua")

	offer := []byte(strings.Join([]string{
		"v=0",
		"o=raw 1 1 IN IP4 127.0.0.1",
		"s=-",
		"c=IN IP4 127.0.0.1",
		"t=0 0",
		"m=audio 40002 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
		"",
	}, "\r\n"))
	dua := sipgo.DialogUA{
		Client:     cli.client,
		ContactHDR: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "alice", Host: cli.host, Port: cli.port}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dialog, err := dua.Invite(ctx, sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}, offer,
		sip.NewHeader("Content-Type", "application/sdp"))
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}

	inbound := waitForInboundLeg(t, inst.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", inst.baseURL(), inbound.ID), nil)
	answerResp.Body.Close()
	if err := dialog.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	assertAdvertised(t, "200 OK", dialog.InviteResponse.Contact(), dialog.InviteResponse.Body(), "127.0.0.1")
	if err := dialog.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)

	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", inst.baseURL(), inbound.ID), nil)
	holdResp.Body.Close()
	reinvite := cli.waitInvite(t, 3*time.Second)
	assertAdvertised(t, "re-INVITE", reinvite.req.Contact(), reinvite.req.Body(), "127.0.0.1")
	cli.answerInvite(t, reinvite)

	hangup(t, inst, inbound.ID)
}

// TestSIPLocalNets_UnsetKeepsExternalContact pins the default: with no local
// networks configured every peer is advertised SIP_EXTERNAL_IP, so a peer that
// cannot reach it never delivers its BYE.
func TestSIPLocalNets_UnsetKeepsExternalContact(t *testing.T) {
	instA := natInstance(t, "instance-a", "")
	instB := newTestInstance(t, "instance-b")
	outboundID, inboundID := establishCall(t, instA, instB)

	hangup(t, instB, inboundID)
	time.Sleep(1500 * time.Millisecond)
	if got := len(instA.collector.matchAll(events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	})); got != 0 {
		t.Errorf("leg.disconnected count = %d, want 0 (the BYE targets the unreachable external address)", got)
	}
	hangup(t, instA, outboundID)
}
