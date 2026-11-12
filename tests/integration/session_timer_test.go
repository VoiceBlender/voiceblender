//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func (c *rawSIPClient) audioSDP(direction string) []byte {
	return []byte(strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=raw 1 1 IN IP4 %s", c.host),
		"s=-",
		fmt.Sprintf("c=IN IP4 %s", c.host),
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=" + direction,
		"",
	}, "\r\n"))
}

// answerWith answers an INVITE or re-INVITE with 200 OK and the given SDP
// direction. A non-empty sessionExpires goes out as the Session-Expires value.
func (c *rawSIPClient) answerWith(t *testing.T, e inviteEvent, direction, sessionExpires string) *sip.Response {
	t.Helper()
	res := sip.NewResponseFromRequest(e.req, sip.StatusOK, "OK", c.audioSDP(direction))
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: c.host, Port: c.port}})
	if sessionExpires != "" {
		res.AppendHeader(sip.NewHeader("Session-Expires", sessionExpires))
	}
	if err := e.tx.Respond(res); err != nil {
		t.Fatalf("respond 200: %v", err)
	}
	close(e.done)
	return res
}

// dialRawWithSessionTimer has inst call the raw UA, which answers with the
// given Session-Expires value. On this leg VoiceBlender is the UAC.
func dialRawWithSessionTimer(t *testing.T, inst *testInstance, cli *rawSIPClient, sessionExpires string) (legID string, inv inviteEvent, okRes *sip.Response) {
	t.Helper()
	createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:alice@%s:%d", cli.host, cli.port),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inv = cli.waitInvite(t, 5*time.Second)
	okRes = cli.answerWith(t, inv, sipmod.DirSendRecv, sessionExpires)
	waitForLegState(t, inst.baseURL(), outbound.ID, "connected", 5*time.Second)
	return outbound.ID, inv, okRes
}

func offerDirection(t *testing.T, req *sip.Request) string {
	t.Helper()
	parsed, err := sipmod.ParseSDP(req.Body())
	if err != nil {
		t.Fatalf("parse offer SDP: %v", err)
	}
	if parsed.Direction == "" {
		return sipmod.DirSendRecv
	}
	return parsed.Direction
}

// TestSessionTimer_OutboundLegRefreshesAsUAC: on an outbound leg VoiceBlender
// is the UAC, so it sends the refreshes when the answer names uac, or names
// nobody. Without them the leg ends as session_expired.
func TestSessionTimer_OutboundLegRefreshesAsUAC(t *testing.T) {
	cases := []struct{ name, sessionExpires string }{
		{"refresher=uac", "2;refresher=uac"},
		{"no refresher named", "2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := newTestInstance(t, "st-uac")
			cli := newRawSIPClient(t, "st-uac-ua")
			legID, _, _ := dialRawWithSessionTimer(t, inst, cli, tc.sessionExpires)

			// A 2 s interval puts each refresh 1 s after the last.
			for i := 1; i <= 2; i++ {
				select {
				case e := <-cli.invites:
					cli.answerWith(t, e, sipmod.DirSendRecv, "2;refresher=uac")
				case <-time.After(3 * time.Second):
					t.Fatalf("outbound leg did not send session refresh %d", i)
				}
			}

			waitForLegState(t, inst.baseURL(), legID, "connected", 2*time.Second)
			httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
		})
	}
}

// TestSessionTimer_OutboundLegLeavesRefreshToUAS: with refresher=uas the far
// end refreshes, so an outbound leg must not send refreshes of its own.
func TestSessionTimer_OutboundLegLeavesRefreshToUAS(t *testing.T) {
	inst := newTestInstance(t, "st-uas")
	cli := newRawSIPClient(t, "st-uas-ua")
	legID, inv, okRes := dialRawWithSessionTimer(t, inst, cli, "2;refresher=uas")

	// A refresh from VoiceBlender would be due after 1 s.
	select {
	case <-cli.invites:
		t.Fatal("outbound leg sent a session refresh, but the far end is the refresher")
	case <-time.After(2500 * time.Millisecond):
	}

	// The far end sends this UPDATE, so in it uac is still the far end.
	resp := cli.sendInDialogUpdate(t, inv.req, okRes, nil, "2;refresher=uac")
	if resp.StatusCode != sip.StatusOK {
		t.Fatalf("UPDATE status = %d %s, want 200", resp.StatusCode, resp.Reason)
	}

	waitForLegState(t, inst.baseURL(), legID, "connected", 2*time.Second)
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
}

// TestSessionTimer_RefreshCarriesSessionExpires: a refresh with no
// Session-Expires turns the timer off at the far end (RFC 4028 §7.4). The
// sender of a refresh is its UAC, so refresher=uac keeps the role here.
func TestSessionTimer_RefreshCarriesSessionExpires(t *testing.T) {
	inst := newTestInstance(t, "st-headers")
	cli := newRawSIPClient(t, "st-headers-ua")
	legID, _, _ := dialRawWithSessionTimer(t, inst, cli, "2;refresher=uac")

	refresh := cli.waitInvite(t, 3*time.Second)
	defer close(refresh.done)

	se := refresh.req.GetHeader("Session-Expires")
	if se == nil {
		t.Fatal("refresh re-INVITE has no Session-Expires header")
	}
	if interval, refresher := sipmod.ParseSessionExpires(se.Value()); interval != 2 || refresher != "uac" {
		t.Errorf("Session-Expires = %q, want 2;refresher=uac", se.Value())
	}
	sup := refresh.req.GetHeader("Supported")
	if sup == nil || !strings.Contains(sup.Value(), "timer") {
		t.Errorf("refresh re-INVITE does not carry Supported: timer (got %v)", sup)
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
}

// TestSessionTimer_RefreshKeepsHold: a refresh must repeat the direction the
// call already has. One that offers sendrecv takes a held call off hold.
func TestSessionTimer_RefreshKeepsHold(t *testing.T) {
	// A 6 s interval puts the refresh 3 s out, which leaves time to set up
	// the hold first.
	const sessionExpires = "6;refresher=uac"

	t.Run("held by VoiceBlender", func(t *testing.T) {
		inst := newTestInstance(t, "st-hold-local")
		cli := newRawSIPClient(t, "st-hold-local-ua")
		legID, _, _ := dialRawWithSessionTimer(t, inst, cli, sessionExpires)

		holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", inst.baseURL(), legID), nil)
		if holdResp.StatusCode != http.StatusAccepted {
			t.Fatalf("hold: unexpected status %d", holdResp.StatusCode)
		}
		holdResp.Body.Close()

		hold := cli.waitInvite(t, 2*time.Second)
		if dir := offerDirection(t, hold.req); dir != sipmod.DirSendOnly {
			t.Fatalf("hold re-INVITE direction = %q, want sendonly", dir)
		}
		cli.answerWith(t, hold, sipmod.DirRecvOnly, "")

		refresh := cli.waitInvite(t, 5*time.Second)
		if dir := offerDirection(t, refresh.req); dir != sipmod.DirSendOnly {
			t.Errorf("refresh direction = %q, want sendonly", dir)
		}
		cli.answerWith(t, refresh, sipmod.DirRecvOnly, sessionExpires)

		httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
	})

	t.Run("held by the far end", func(t *testing.T) {
		inst := newTestInstance(t, "st-hold-remote")
		cli := newRawSIPClient(t, "st-hold-remote-ua")
		legID, inv, okRes := dialRawWithSessionTimer(t, inst, cli, sessionExpires)

		resp := cli.sendInDialogUpdate(t, inv.req, okRes, cli.audioSDP(sipmod.DirSendOnly), "")
		if resp.StatusCode != sip.StatusOK {
			t.Fatalf("hold UPDATE status = %d %s, want 200", resp.StatusCode, resp.Reason)
		}
		if !strings.Contains(string(resp.Body()), "a=recvonly") {
			t.Fatalf("answer to the hold offer is not recvonly:\n%s", resp.Body())
		}

		refresh := cli.waitInvite(t, 5*time.Second)
		if dir := offerDirection(t, refresh.req); dir != sipmod.DirRecvOnly {
			t.Errorf("refresh direction = %q, want recvonly", dir)
		}
		cli.answerWith(t, refresh, sipmod.DirSendOnly, sessionExpires)

		httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
	})
}
