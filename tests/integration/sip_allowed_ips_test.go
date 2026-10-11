//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Every peer in these tests is on 127.0.0.1, so a source is made "unlisted" by
// allowing a range that excludes loopback.
const sipAllowedElsewhere = "10.0.0.0/8"

func withSIPAllowedIPs(list string) func(*config.Config) {
	return func(c *config.Config) { c.SIPAllowedIPs = list }
}

func sipAllowlistInvite(sipPort int, transport string) *sip.Request {
	target := sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: sipPort}
	if transport != "" {
		target.UriParams = sip.NewParams()
		target.UriParams.Add("transport", transport)
	}
	req := sip.NewRequest(sip.INVITE, target)
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "caller", Host: "127.0.0.1", Port: 5060}})
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(inboundOffer())
	return req
}

// expectSIPSilence sends req and fails on any response. It returns once inst
// has counted the drop and stayed quiet past sipgo's automatic 100 Trying.
func expectSIPSilence(t *testing.T, inst *testInstance, client *sipgo.Client, req *sip.Request) {
	t.Helper()
	before := inst.engine.RequestsRejected()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := client.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("send %s: %v", req.Method, err)
	}
	defer tx.Terminate()

	deadline := time.After(3 * time.Second)
	for inst.engine.RequestsRejected() == before {
		select {
		case res := <-tx.Responses():
			if res != nil {
				t.Fatalf("%s from an unlisted source answered %d, want no response", req.Method, res.StatusCode)
			}
		case <-deadline:
			t.Fatalf("%s never reached the allowlist", req.Method)
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case res := <-tx.Responses():
		if res != nil {
			t.Fatalf("%s from an unlisted source answered %d, want no response", req.Method, res.StatusCode)
		}
	case <-time.After(500 * time.Millisecond):
	}
}

func expectNoLegs(t *testing.T, inst *testInstance) {
	t.Helper()
	var legs []legView
	decodeJSON(t, httpGet(t, inst.baseURL()+"/v1/legs"), &legs)
	if len(legs) != 0 {
		t.Errorf("instance has %d legs, want none: %+v", len(legs), legs)
	}
	if inst.collector.hasEvent(events.LegRinging, nil) {
		t.Error("leg.ringing published for a dropped INVITE")
	}
}

// TestSIPAllowedIPs_AcceptedCall covers every way a source ends up accepted:
// the default empty list, a /0 entry, and a list naming the peer. The call is
// torn down from the far end so an in-dialog request passes the filter too.
func TestSIPAllowedIPs_AcceptedCall(t *testing.T) {
	cases := []struct{ name, list string }{
		{"default", ""},
		{"zero-prefix", "0.0.0.0/0"},
		{"cidr", "127.0.0.0/8"},
		{"host-among-others", "10.0.0.0/8, 127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instA := newTestInstanceWithOpts(t, "sipallow-ok-a-"+tc.name, withSIPAllowedIPs(tc.list))
			instB := newTestInstanceWithOpts(t, "sipallow-ok-b-"+tc.name, withSIPAllowedIPs(tc.list))

			outboundID, inboundID := establishCall(t, instA, instB)
			hangup(t, instB, inboundID)
			expectDisconnect(t, instA, outboundID)

			for _, inst := range []*testInstance{instA, instB} {
				if n := inst.engine.RequestsRejected(); n != 0 {
					t.Errorf("[%s] RequestsRejected() = %d, want 0", inst.name, n)
				}
			}
		})
	}
}

// TestSIPAllowedIPs_UnlistedInviteDropped verifies an INVITE from outside the
// list gets no response of any kind and never becomes a leg.
func TestSIPAllowedIPs_UnlistedInviteDropped(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "sipallow-invite", withSIPAllowedIPs(sipAllowedElsewhere))
	cli := newRawSIPClient(t, "sipallow-invite-ua")

	expectSIPSilence(t, inst, cli.client, sipAllowlistInvite(inst.sipPort, ""))
	expectNoLegs(t, inst)
}

// TestSIPAllowedIPs_UnlistedRegisterAndOptionsDropped verifies the filter is
// not INVITE-specific: REGISTER creates no binding and OPTIONS is not answered.
func TestSIPAllowedIPs_UnlistedRegisterAndOptionsDropped(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "sipallow-register", withSIPAllowedIPs(sipAllowedElsewhere))
	cli := newRawSIPClient(t, "sipallow-register-ua")

	reg := cli.buildRegister("alice", inst.sipPort, "<"+cli.contactURI("alice")+">", 60)
	expectSIPSilence(t, inst, cli.client, reg)
	for _, typ := range []events.EventType{events.SIPRegistrationAttempt, events.SIPRegistrationActive} {
		if inst.collector.hasEvent(typ, nil) {
			t.Errorf("%s published for a dropped REGISTER", typ)
		}
	}

	options := sip.NewRequest(sip.OPTIONS, sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: inst.sipPort})
	expectSIPSilence(t, inst, cli.client, options)
}

// TestSIPAllowedIPs_TCP_ListedAccepted verifies a listed source completes a
// full call over TCP, in-dialog ACK and BYE included.
func TestSIPAllowedIPs_TCP_ListedAccepted(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "sipallow-tcp-ok", func(c *config.Config) {
		c.SIPTCPEnabled = true
		c.SIPAllowedIPs = "127.0.0.0/8"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	recipient := sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort, UriParams: sip.NewParams()}
	recipient.UriParams.Add("transport", "tcp")
	dialog, inbound := answerInboundCall(t, ctx, inst, recipient, inboundOffer())
	if via := dialog.InviteRequest.Via(); via == nil || !strings.EqualFold(via.Transport, "TCP") {
		t.Fatalf("INVITE was not sent over TCP: Via = %v", via)
	}

	if err := dialog.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)
	if err := dialog.Bye(ctx); err != nil {
		t.Fatalf("BYE: %v", err)
	}
	expectDisconnect(t, inst, inbound.ID)

	if n := inst.engine.RequestsRejected(); n != 0 {
		t.Errorf("RequestsRejected() = %d, want 0", n)
	}
}

// TestSIPAllowedIPs_TCP_UnlistedDropped verifies the filter also holds on a
// stream transport, where the request arrives on an accepted connection.
func TestSIPAllowedIPs_TCP_UnlistedDropped(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "sipallow-tcp-drop", func(c *config.Config) {
		c.SIPTCPEnabled = true
		c.SIPAllowedIPs = sipAllowedElsewhere
	})
	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("new UA: %v", err)
	}
	t.Cleanup(func() { ua.Close() })
	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	invite := sipAllowlistInvite(inst.sipPort, "tcp")
	expectSIPSilence(t, inst, client, invite)
	if via := invite.Via(); via == nil || !strings.EqualFold(via.Transport, "TCP") {
		t.Fatalf("INVITE was not sent over TCP: Via = %v", via)
	}
	expectNoLegs(t, inst)
}

// TestSIPAllowedIPs_InDialogRequestDropped pins that the list covers in-dialog
// requests: a call dialed to an unlisted peer connects, because responses are
// not filtered, but that peer's BYE is ignored.
func TestSIPAllowedIPs_InDialogRequestDropped(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "sipallow-dialog-a", withSIPAllowedIPs(sipAllowedElsewhere))
	instB := newTestInstance(t, "sipallow-dialog-b")

	outboundID, inboundID := establishCall(t, instA, instB)
	if n := instA.engine.RequestsRejected(); n != 0 {
		t.Fatalf("RequestsRejected() = %d after setup, want 0", n)
	}

	hangup(t, instB, inboundID)
	deadline := time.Now().Add(5 * time.Second)
	for instA.engine.RequestsRejected() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the peer's BYE never reached the allowlist")
		}
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(300 * time.Millisecond)
	if instA.collector.hasEvent(events.LegDisconnected, func(e events.Event) bool { return e.Data.GetLegID() == outboundID }) {
		t.Fatal("leg disconnected by a BYE from an unlisted source")
	}
	waitForLegState(t, instA.baseURL(), outboundID, "connected", time.Second)

	hangup(t, instA, outboundID)
	expectDisconnect(t, instA, outboundID)
}

// TestSIPAllowedIPs_RejectedMetric verifies drops show up on /metrics. The
// counter is process-wide, so it is read from a second instance.
func TestSIPAllowedIPs_RejectedMetric(t *testing.T) {
	const name = "voiceblender_sip_requests_rejected_total"
	metricsInst := newTestInstanceWithMetrics(t, "sipallow-metrics")
	inst := newTestInstanceWithOpts(t, "sipallow-metric-drop", withSIPAllowedIPs(sipAllowedElsewhere))
	cli := newRawSIPClient(t, "sipallow-metric-ua")

	before := parseGaugeValue(t, metricsBody(t, metricsInst.baseURL()), name)
	if before < 0 {
		t.Fatalf("%s is not exported", name)
	}
	rejectedBefore := inst.engine.RequestsRejected()

	options := sip.NewRequest(sip.OPTIONS, sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: inst.sipPort})
	expectSIPSilence(t, inst, cli.client, options)

	dropped := float64(inst.engine.RequestsRejected() - rejectedBefore)
	if got := parseGaugeValue(t, metricsBody(t, metricsInst.baseURL()), name); got < before+dropped {
		t.Errorf("%s = %g, want at least %g", name, got, before+dropped)
	}
}
