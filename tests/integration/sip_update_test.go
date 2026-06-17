//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// sendInDialogUpdate builds an in-dialog UPDATE request from the rawSIPClient
// (acting as UAS) back to the peer that originated the dialog with INVITE
// `inv`. The response we sent to that INVITE (`okRes`) supplies the local
// (To) tag and the peer Contact governs the Request-URI. Returns the final
// response from the peer.
func (c *rawSIPClient) sendInDialogUpdate(t *testing.T, inv *sip.Request, okRes *sip.Response, sdp []byte, sessionExpires string) *sip.Response {
	t.Helper()

	peerContact := inv.Contact()
	if peerContact == nil {
		t.Fatal("INVITE missing Contact header")
	}

	callID := inv.CallID()
	from := inv.From() // becomes the remote party in the UPDATE
	to := okRes.To()   // contains the local (UAS) tag we generated
	if callID == nil || from == nil || to == nil {
		t.Fatal("dialog headers missing")
	}

	req := sip.NewRequest(sip.UPDATE, peerContact.Address)
	// Swap From/To for an in-dialog UAS-originated request.
	fromHdr := &sip.FromHeader{Address: to.Address, Params: to.Params.Clone()}
	toHdr := &sip.ToHeader{Address: from.Address, Params: from.Params.Clone()}
	req.AppendHeader(fromHdr)
	req.AppendHeader(toHdr)
	req.AppendHeader(sip.HeaderClone(callID))
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: c.host, Port: c.port}})
	if sessionExpires != "" {
		req.AppendHeader(sip.NewHeader("Session-Expires", sessionExpires))
		req.AppendHeader(sip.NewHeader("Supported", "timer"))
	}
	if len(sdp) > 0 {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(sdp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		t.Fatalf("UPDATE Do: %v", err)
	}
	return resp
}

// answerInviteCaptured behaves like answerInvite but returns the response
// object so the caller can read the auto-generated To-tag for follow-up
// in-dialog requests.
func (c *rawSIPClient) answerInviteCaptured(t *testing.T, e inviteEvent) *sip.Response {
	t.Helper()
	sdp := []byte(strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=raw 1 1 IN IP4 %s", c.host),
		"s=-",
		fmt.Sprintf("c=IN IP4 %s", c.host),
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
		"",
	}, "\r\n"))
	res := sip.NewResponseFromRequest(e.req, sip.StatusOK, "OK", sdp)
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: c.host, Port: c.port}})
	if err := e.tx.Respond(res); err != nil {
		t.Fatalf("respond 200: %v", err)
	}
	close(e.done)
	return res
}

// TestSIPUpdate_SessionTimerRefresh exercises RFC 3311 + RFC 4028 §10: an
// in-dialog UPDATE with no body and a Session-Expires header should be
// accepted by the engine (200 OK) and the Session-Expires must be echoed
// back, instead of the legacy 405 Method Not Allowed response.
func TestSIPUpdate_SessionTimerRefresh(t *testing.T) {
	inst := newTestInstance(t, "update-refresh")
	cli := newRawSIPClient(t, "update-ua")

	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)

	createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     "sip:alice@vb.test",
		"from":   "support",
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	e := cli.waitInvite(t, 5*time.Second)
	okRes := cli.answerInviteCaptured(t, e)

	// Give VB's ACK + state machine a moment to finish establishing the
	// UAC dialog so MatchRequestDialog can find it.
	time.Sleep(200 * time.Millisecond)

	resp := cli.sendInDialogUpdate(t, e.req, okRes, nil, "1800;refresher=uac")
	if resp.StatusCode != sip.StatusOK {
		t.Fatalf("UPDATE status = %d %s, want 200", resp.StatusCode, resp.Reason)
	}
	se := resp.GetHeader("Session-Expires")
	if se == nil {
		t.Fatal("200 OK missing Session-Expires echo")
	}
	if !strings.Contains(se.Value(), "1800") {
		t.Errorf("Session-Expires = %q, want interval 1800", se.Value())
	}
	allow := resp.GetHeader("Allow")
	if allow == nil {
		t.Fatal("200 OK missing Allow header")
	}
	if !strings.Contains(allow.Value(), "UPDATE") {
		t.Errorf("Allow header %q does not advertise UPDATE", allow.Value())
	}
}

// TestSIPAllow_InviteCarriesAllowHeader verifies the engine advertises every
// supported method (including UPDATE) on outbound INVITEs via the Allow
// header (RFC 3261 §20.5).
func TestSIPAllow_InviteCarriesAllowHeader(t *testing.T) {
	inst := newTestInstance(t, "allow-invite")
	cli := newRawSIPClient(t, "allow-ua")

	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)

	createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     "sip:alice@vb.test",
		"from":   "support",
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	e := cli.waitInvite(t, 5*time.Second)
	defer close(e.done)
	allow := e.req.GetHeader("Allow")
	if allow == nil {
		t.Fatal("outbound INVITE missing Allow header")
	}
	for _, m := range []string{"INVITE", "BYE", "CANCEL", "UPDATE", "REFER"} {
		if !strings.Contains(allow.Value(), m) {
			t.Errorf("Allow %q missing %s", allow.Value(), m)
		}
	}
}

// TestSIPAllow_InviteOkCarriesAllowHeader verifies the engine attaches the
// Allow header on the 200 OK to an inbound INVITE (RFC 3261 §20.5), so the
// peer learns up front that UPDATE, REFER, etc. are accepted.
func TestSIPAllow_InviteOkCarriesAllowHeader(t *testing.T) {
	inst := newTestInstance(t, "allow-200ok")
	cli := newRawSIPClient(t, "allow-200ok-ua")

	offerSDP := []byte(strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=raw 1 1 IN IP4 %s", cli.host),
		"s=-",
		fmt.Sprintf("c=IN IP4 %s", cli.host),
		"t=0 0",
		"m=audio 40010 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
		"",
	}, "\r\n"))

	target := sip.Uri{Scheme: "sip", User: "alice", Host: "127.0.0.1", Port: inst.sipPort}
	req := sip.NewRequest(sip.INVITE, target)
	fromURI := sip.Uri{Scheme: "sip", User: "tester", Host: cli.host, Port: cli.port}
	fromHdr := &sip.FromHeader{Address: fromURI, Params: sip.NewParams()}
	fromHdr.Params.Add("tag", sip.GenerateTagN(8))
	req.AppendHeader(fromHdr)
	req.AppendHeader(&sip.ToHeader{Address: target, Params: sip.NewParams()})
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: cli.host, Port: cli.port}})
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(offerSDP)

	respCh := make(chan *sip.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := cli.client.Do(ctx, req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	// Wait for VB to register the inbound leg, then answer it via the REST API.
	inbound := waitForInboundLeg(t, inst.baseURL(), 5*time.Second)
	ansResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", inst.baseURL(), inbound.ID), nil)
	if ansResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: status %d", ansResp.StatusCode)
	}
	ansResp.Body.Close()

	select {
	case err := <-errCh:
		t.Fatalf("INVITE Do: %v", err)
	case resp := <-respCh:
		if resp.StatusCode != sip.StatusOK {
			t.Fatalf("INVITE status = %d %s, want 200", resp.StatusCode, resp.Reason)
		}
		allow := resp.GetHeader("Allow")
		if allow == nil {
			t.Fatal("200 OK to INVITE missing Allow header")
		}
		for _, m := range []string{"INVITE", "BYE", "CANCEL", "UPDATE", "REFER"} {
			if !strings.Contains(allow.Value(), m) {
				t.Errorf("Allow %q missing %s", allow.Value(), m)
			}
		}
	case <-time.After(7 * time.Second):
		t.Fatal("timed out waiting for 200 OK")
	}
}

// TestSIPUpdate_OutOfDialogRejected ensures the engine rejects an UPDATE
// that doesn't match any active dialog with 481 Call/Transaction Does Not
// Exist (RFC 3261 §12.2.2 / RFC 3311 §5.2) rather than 405 Method Not
// Allowed.
func TestSIPUpdate_OutOfDialogRejected(t *testing.T) {
	inst := newTestInstance(t, "update-no-dialog")
	cli := newRawSIPClient(t, "update-no-dialog-ua")

	target := sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: inst.sipPort}
	req := sip.NewRequest(sip.UPDATE, target)
	fromURI := sip.Uri{Scheme: "sip", User: "x", Host: cli.host, Port: cli.port}
	fromHdr := &sip.FromHeader{Address: fromURI, Params: sip.NewParams()}
	fromHdr.Params.Add("tag", sip.GenerateTagN(8))
	req.AppendHeader(fromHdr)
	toParams := sip.NewParams()
	toParams.Add("tag", "stale-tag")
	req.AppendHeader(&sip.ToHeader{
		Address: sip.Uri{Scheme: "sip", User: "y", Host: "vb.test"},
		Params:  toParams,
	})
	req.AppendHeader(sip.NewHeader("Call-ID", "nonexistent-dialog@update-test"))
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: cli.host, Port: cli.port}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := cli.client.Do(ctx, req)
	if err != nil {
		t.Fatalf("UPDATE Do: %v", err)
	}
	if resp.StatusCode != 481 {
		t.Fatalf("UPDATE status = %d %s, want 481", resp.StatusCode, resp.Reason)
	}
}
