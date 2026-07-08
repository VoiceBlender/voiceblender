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
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// authConsultConfig gives the test ample time to react to the parked REGISTER
// and issue its decision before the consult auto-accepts.
func authConsultConfig(c *config.Config) {
	c.SIPInboundAuthConsultTimeoutMs = 4000
}

// buildAuthRegister builds a REGISTER carrying an explicit Call-ID and CSeq so
// the initial request and the credentialed retry correlate (the engine keys
// pending challenges by Call-ID).
func (c *rawSIPClient) buildAuthRegister(aorUser string, sipPort int, contact, callID string, cseq uint32, auth string) *sip.Request {
	req := c.buildRegister(aorUser, sipPort, "<"+contact+">", 600)
	cid := sip.CallIDHeader(callID)
	req.AppendHeader(&cid)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.REGISTER})
	if auth != "" {
		req.AppendHeader(sip.NewHeader("Authorization", auth))
	}
	return req
}

func digestAuthHeader(t *testing.T, challengeVal, method, uri, user, pass string) string {
	t.Helper()
	chal, err := digest.ParseChallenge(challengeVal)
	if err != nil {
		t.Fatalf("parse challenge %q: %v", challengeVal, err)
	}
	cred, err := digest.Digest(chal, digest.Options{
		Method:   method,
		URI:      uri,
		Username: user,
		Password: pass,
		Count:    1,
		Cnonce:   sip.GenerateTagN(8),
	})
	if err != nil {
		t.Fatalf("compute digest: %v", err)
	}
	return cred.String()
}

// TestSIPInboundAuth_RegisterChallengeSuccess exercises the full REGISTER
// challenge round-trip: REGISTER → 401 → credentialed re-REGISTER → 200 OK and
// a live binding.
func TestSIPInboundAuth_RegisterChallengeSuccess(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "reg-chal-ok", authConsultConfig)
	cli := newRawSIPClient(t, "reg-chal-ua")

	const realm = "vb.test"
	const user = "alice"
	const pass = "s3cret"

	// Concurrently react to the parked attempt by challenging it.
	go func() {
		if !inst.collector.hasEvent(events.SIPRegistrationAttempt, nil) {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) && !inst.collector.hasEvent(events.SIPRegistrationAttempt, nil) {
				time.Sleep(20 * time.Millisecond)
			}
		}
		for _, e := range inst.collector.matchAll(events.SIPRegistrationAttempt, nil) {
			id := e.Data.(*events.SIPRegistrationAttemptData).AttemptID
			resp := httpPost(t, fmt.Sprintf("%s/v1/sip/registrations/attempts/%s/challenge", inst.baseURL(), id),
				map[string]interface{}{"realm": realm, "username": user, "password": pass})
			resp.Body.Close()
		}
	}()

	callID := sip.GenerateTagN(16)
	regURI := fmt.Sprintf("sip:127.0.0.1:%d", inst.sipPort)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// 1) Initial REGISTER → expect 401 with a digest challenge.
	resp, err := cli.client.Do(ctx, cli.buildAuthRegister(user, inst.sipPort, cli.contactURI(user), callID, 1, ""))
	if err != nil {
		t.Fatalf("initial REGISTER: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("initial REGISTER status = %d, want 401", resp.StatusCode)
	}
	chalHdr := resp.GetHeader("WWW-Authenticate")
	if chalHdr == nil {
		t.Fatal("401 missing WWW-Authenticate header")
	}

	// 2) Credentialed re-REGISTER (same Call-ID) → expect 200 OK.
	auth := digestAuthHeader(t, chalHdr.Value(), "REGISTER", regURI, user, pass)
	resp2, err := cli.client.Do(ctx, cli.buildAuthRegister(user, inst.sipPort, cli.contactURI(user), callID, 2, auth))
	if err != nil {
		t.Fatalf("authed REGISTER: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authed REGISTER status = %d, want 200", resp2.StatusCode)
	}

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(list.Bindings))
	}
	if list.Bindings[0].AOR != "sip:alice@vb.test" {
		t.Errorf("AOR = %q", list.Bindings[0].AOR)
	}
}

// TestSIPInboundAuth_RegisterChallengeMaxExpires verifies a challenge's
// max_expires caps the granted binding TTL, floored at the registrar's 60 s
// minimum: the UA requests 600 s and the challenge caps at 30 s, so the
// credentialed re-REGISTER binds for the 60 s floor.
func TestSIPInboundAuth_RegisterChallengeMaxExpires(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "reg-chal-cap", authConsultConfig)
	cli := newRawSIPClient(t, "reg-chal-cap-ua")

	const realm = "vb.test"
	const user = "alice"
	const pass = "s3cret"

	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && !inst.collector.hasEvent(events.SIPRegistrationAttempt, nil) {
			time.Sleep(20 * time.Millisecond)
		}
		for _, e := range inst.collector.matchAll(events.SIPRegistrationAttempt, nil) {
			id := e.Data.(*events.SIPRegistrationAttemptData).AttemptID
			resp := httpPost(t, fmt.Sprintf("%s/v1/sip/registrations/attempts/%s/challenge", inst.baseURL(), id),
				map[string]interface{}{"realm": realm, "username": user, "password": pass, "max_expires": 30})
			resp.Body.Close()
		}
	}()

	callID := sip.GenerateTagN(16)
	regURI := fmt.Sprintf("sip:127.0.0.1:%d", inst.sipPort)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// Initial REGISTER (requests 600 s) → 401.
	resp, err := cli.client.Do(ctx, cli.buildAuthRegister(user, inst.sipPort, cli.contactURI(user), callID, 1, ""))
	if err != nil {
		t.Fatalf("initial REGISTER: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("initial REGISTER status = %d, want 401", resp.StatusCode)
	}

	// Credentialed re-REGISTER → 200 OK, but capped at 30 s.
	auth := digestAuthHeader(t, resp.GetHeader("WWW-Authenticate").Value(), "REGISTER", regURI, user, pass)
	resp2, err := cli.client.Do(ctx, cli.buildAuthRegister(user, inst.sipPort, cli.contactURI(user), callID, 2, auth))
	if err != nil {
		t.Fatalf("authed REGISTER: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authed REGISTER status = %d, want 200", resp2.StatusCode)
	}

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(list.Bindings))
	}
	if got := list.Bindings[0].GrantedExpiresSeconds; got != 60 {
		t.Errorf("GrantedExpiresSeconds = %d, want 60 (max_expires 30 floored to the 60s minimum)", got)
	}
}

// TestSIPInboundAuth_RegisterChallengeWrongPassword verifies a bad credential
// is answered with 403 and never binds.
func TestSIPInboundAuth_RegisterChallengeWrongPassword(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "reg-chal-bad", authConsultConfig)
	cli := newRawSIPClient(t, "reg-chal-bad-ua")

	const realm = "vb.test"
	const user = "alice"

	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && !inst.collector.hasEvent(events.SIPRegistrationAttempt, nil) {
			time.Sleep(20 * time.Millisecond)
		}
		for _, e := range inst.collector.matchAll(events.SIPRegistrationAttempt, nil) {
			id := e.Data.(*events.SIPRegistrationAttemptData).AttemptID
			resp := httpPost(t, fmt.Sprintf("%s/v1/sip/registrations/attempts/%s/challenge", inst.baseURL(), id),
				map[string]interface{}{"realm": realm, "username": user, "password": "correct-horse"})
			resp.Body.Close()
		}
	}()

	callID := sip.GenerateTagN(16)
	regURI := fmt.Sprintf("sip:127.0.0.1:%d", inst.sipPort)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	resp, err := cli.client.Do(ctx, cli.buildAuthRegister(user, inst.sipPort, cli.contactURI(user), callID, 1, ""))
	if err != nil {
		t.Fatalf("initial REGISTER: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("initial REGISTER status = %d, want 401", resp.StatusCode)
	}

	// Sign with the wrong password.
	auth := digestAuthHeader(t, resp.GetHeader("WWW-Authenticate").Value(), "REGISTER", regURI, user, "wrong-password")
	resp2, err := cli.client.Do(ctx, cli.buildAuthRegister(user, inst.sipPort, cli.contactURI(user), callID, 2, auth))
	if err != nil {
		t.Fatalf("authed REGISTER: %v", err)
	}
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("authed REGISTER status = %d, want 403", resp2.StatusCode)
	}
	if list := registrationsList(t, inst.baseURL()); len(list.Bindings) != 0 {
		t.Errorf("bindings = %d, want 0 (no binding on failed auth)", len(list.Bindings))
	}
}

// TestSIPInboundAuth_RegisterTimeoutAcceptsWhenConfigured confirms that with
// SIP_INBOUND_REGISTER_DEFAULT=accept (the harness default) the REGISTER is
// surfaced for a decision (a sip.registration_attempt event fires) and, when no
// client challenges/rejects within the consult window, the fallback binds it.
func TestSIPInboundAuth_RegisterTimeoutAcceptsWhenConfigured(t *testing.T) {
	inst := newTestInstance(t, "reg-timeout")
	cli := newRawSIPClient(t, "reg-timeout-ua")

	resp := cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("REGISTER status = %d, want 200 (accept fallback)", resp.StatusCode)
	}
	if !inst.collector.hasEvent(events.SIPRegistrationAttempt, nil) {
		t.Error("sip.registration_attempt event was not published")
	}
	if list := registrationsList(t, inst.baseURL()); len(list.Bindings) != 1 {
		t.Errorf("bindings = %d, want 1", len(list.Bindings))
	}
}

// TestSIPInboundAuth_RegisterTimeoutRejectsByDefault verifies the shipped
// fail-closed default: with SIP_INBOUND_REGISTER_DEFAULT=reject, an inbound
// REGISTER that no controller decides is denied with 403 and never binds.
func TestSIPInboundAuth_RegisterTimeoutRejectsByDefault(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "reg-timeout-reject", func(c *config.Config) {
		c.SIPInboundRegisterDefault = "reject"
	})
	cli := newRawSIPClient(t, "reg-timeout-reject-ua")

	resp := cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("REGISTER status = %d, want 403 (reject fallback)", resp.StatusCode)
	}
	if !inst.collector.hasEvent(events.SIPRegistrationAttempt, nil) {
		t.Error("sip.registration_attempt event was not published")
	}
	if list := registrationsList(t, inst.baseURL()); len(list.Bindings) != 0 {
		t.Errorf("bindings = %d, want 0 (no binding on reject)", len(list.Bindings))
	}
}

// TestSIPInboundAuth_InviteChallengeSuccess exercises the full INVITE challenge
// round-trip: INVITE → challenge_leg → 401 → credentialed re-INVITE surfaced as
// authenticated → answer → 200 OK.
func TestSIPInboundAuth_InviteChallengeSuccess(t *testing.T) {
	inst := newTestInstance(t, "inv-chal-ok")
	cli := newRawSIPClient(t, "inv-chal-ua")

	const realm = "vb.test"
	const user = "bob"
	const pass = "hunter2"

	offerSDP := []byte(strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=raw 1 1 IN IP4 %s", cli.host),
		"s=-",
		fmt.Sprintf("c=IN IP4 %s", cli.host),
		"t=0 0",
		"m=audio 40020 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
		"",
	}, "\r\n"))

	target := sip.Uri{Scheme: "sip", User: user, Host: "127.0.0.1", Port: inst.sipPort}
	inviteURI := fmt.Sprintf("sip:%s@127.0.0.1:%d", user, inst.sipPort)
	callID := sip.GenerateTagN(16)
	fromTag := sip.GenerateTagN(8)

	buildInvite := func(cseq uint32, auth string) *sip.Request {
		req := sip.NewRequest(sip.INVITE, target)
		fromURI := sip.Uri{Scheme: "sip", User: "caller", Host: cli.host, Port: cli.port}
		fromHdr := &sip.FromHeader{Address: fromURI, Params: sip.NewParams()}
		fromHdr.Params.Add("tag", fromTag)
		req.AppendHeader(fromHdr)
		req.AppendHeader(&sip.ToHeader{Address: target, Params: sip.NewParams()})
		req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: cli.host, Port: cli.port}})
		cid := sip.CallIDHeader(callID)
		req.AppendHeader(&cid)
		req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.INVITE})
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		if auth != "" {
			req.AppendHeader(sip.NewHeader("Authorization", auth))
		}
		req.SetBody(offerSDP)
		return req
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// 1) Initial INVITE in a goroutine; the leg surfaces as ringing.
	resp1Ch := make(chan *sip.Response, 1)
	err1Ch := make(chan error, 1)
	go func() {
		resp, err := cli.client.Do(ctx, buildInvite(1, ""))
		if err != nil {
			err1Ch <- err
			return
		}
		resp1Ch <- resp
	}()

	ringing := waitForInboundLeg(t, inst.baseURL(), 5*time.Second)
	chalResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/challenge", inst.baseURL(), ringing.ID),
		map[string]interface{}{"realm": realm, "username": user, "password": pass})
	if chalResp.StatusCode != http.StatusAccepted {
		t.Fatalf("challenge_leg status = %d, want 202", chalResp.StatusCode)
	}
	chalResp.Body.Close()

	var challenge string
	select {
	case err := <-err1Ch:
		t.Fatalf("initial INVITE: %v", err)
	case resp := <-resp1Ch:
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("initial INVITE status = %d, want 401", resp.StatusCode)
		}
		h := resp.GetHeader("WWW-Authenticate")
		if h == nil {
			t.Fatal("401 missing WWW-Authenticate")
		}
		challenge = h.Value()
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for 401 challenge")
	}

	// 2) Credentialed re-INVITE → surfaced as a new ringing leg with
	// authenticated=true, which we then answer.
	auth := digestAuthHeader(t, challenge, "INVITE", inviteURI, user, pass)
	resp2Ch := make(chan *sip.Response, 1)
	err2Ch := make(chan error, 1)
	go func() {
		resp, err := cli.client.Do(ctx, buildInvite(2, auth))
		if err != nil {
			err2Ch <- err
			return
		}
		resp2Ch <- resp
	}()

	ev := inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
		return e.Data.(*events.LegRingingData).Authenticated
	}, 5*time.Second)
	authedLegID := ev.Data.(*events.LegRingingData).LegID
	if u := ev.Data.(*events.LegRingingData).AuthUsername; u != user {
		t.Errorf("auth_username = %q, want %q", u, user)
	}

	ansResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", inst.baseURL(), authedLegID), nil)
	if ansResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer status = %d, want 202", ansResp.StatusCode)
	}
	ansResp.Body.Close()

	select {
	case err := <-err2Ch:
		t.Fatalf("authed INVITE: %v", err)
	case resp := <-resp2Ch:
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("authed INVITE status = %d, want 200", resp.StatusCode)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for 200 OK on authed INVITE")
	}
}
