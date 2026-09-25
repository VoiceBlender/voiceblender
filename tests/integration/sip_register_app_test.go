//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo/sip"
)

// decideAttempts posts path ("accept" or "challenge") with body for every
// sip.registration_attempt it has not yet decided, until the test ends.
func decideAttempts(t *testing.T, inst *testInstance, path string, body func() map[string]interface{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var mu sync.Mutex
	seen := map[string]bool{}
	go func() {
		for ctx.Err() == nil {
			for _, e := range inst.collector.matchAll(events.SIPRegistrationAttempt, nil) {
				id := e.Data.(*events.SIPRegistrationAttemptData).AttemptID
				mu.Lock()
				done := seen[id]
				seen[id] = true
				mu.Unlock()
				if done {
					continue
				}
				resp := httpPost(t, fmt.Sprintf("%s/v1/sip/registrations/attempts/%s/%s", inst.baseURL(), id, path), body())
				resp.Body.Close()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// TestSIPRegister_AcceptClaimsAppID pins the claim-on-decision model: the app
// accepting a REGISTER with app_id owns the binding, so its active event, the
// refresh attempt and calls from the registered socket are scoped to it — and
// a refresh accepted without app_id keeps the owner.
func TestSIPRegister_AcceptClaimsAppID(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "reg-app-accept", authConsultConfig)
	cli := newRawSIPClient(t, "reg-app-ua")

	var claimed sync.Once
	decideAttempts(t, inst, "accept", func() map[string]interface{} {
		body := map[string]interface{}{}
		claimed.Do(func() { body["app_id"] = "acme" })
		return body
	})

	if resp := cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600); resp.StatusCode != http.StatusOK {
		t.Fatalf("REGISTER status = %d, want 200", resp.StatusCode)
	}
	active := inst.collector.waitForMatch(t, events.SIPRegistrationActive, nil, 3*time.Second)
	if app := active.Data.GetAppID(); app != "acme" {
		t.Errorf("registration_active app_id = %q, want acme", app)
	}

	if resp := cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600); resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh REGISTER status = %d, want 200", resp.StatusCode)
	}
	attempts := inst.collector.matchAll(events.SIPRegistrationAttempt, nil)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if app := attempts[0].Data.GetAppID(); app != "" {
		t.Errorf("first attempt app_id = %q, want unclaimed", app)
	}
	if app := attempts[1].Data.GetAppID(); app != "acme" {
		t.Errorf("refresh attempt app_id = %q, want acme", app)
	}

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 || list.Bindings[0].AppID != "acme" {
		t.Fatalf("bindings = %+v, want one owned by acme", list.Bindings)
	}

	sendRawInvite(t, cli, inst.sipPort, "bob", sip.NewHeader("X-App-ID", "spoofed"))
	ev := inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegRingingData)
		return ok && strings.Contains(d.To, "bob")
	}, 5*time.Second)
	d := ev.Data.(*events.LegRingingData)
	if d.AppID != "acme" {
		t.Errorf("leg.ringing app_id = %q, want acme", d.AppID)
	}
	if d.TrunkID != "" {
		t.Errorf("leg.ringing trunk_id = %q, want none", d.TrunkID)
	}
	resp := httpDelete(t, inst.baseURL()+"/v1/legs/"+d.LegID)
	resp.Body.Close()
}

// TestSIPRegister_ChallengeClaimsAppID pins that app_id given on a challenge
// survives to the binding created by the credentialed re-REGISTER, which is
// not consulted again.
func TestSIPRegister_ChallengeClaimsAppID(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "reg-app-chal", authConsultConfig)
	cli := newRawSIPClient(t, "reg-app-chal-ua")

	const user, pass = "alice", "s3cret"
	decideAttempts(t, inst, "challenge", func() map[string]interface{} {
		return map[string]interface{}{"realm": "vb.test", "username": user, "password": pass, "app_id": "acme"}
	})

	callID := sip.GenerateTagN(16)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	resp, err := cli.client.Do(ctx, cli.buildAuthRegister(user, inst.sipPort, cli.contactURI(user), callID, 1, ""))
	if err != nil {
		t.Fatalf("initial REGISTER: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("initial REGISTER status = %d, want 401", resp.StatusCode)
	}
	auth := digestAuthHeader(t, resp.GetHeader("WWW-Authenticate").Value(), "REGISTER",
		fmt.Sprintf("sip:127.0.0.1:%d", inst.sipPort), user, pass)
	resp2, err := cli.client.Do(ctx, cli.buildAuthRegister(user, inst.sipPort, cli.contactURI(user), callID, 2, auth))
	if err != nil {
		t.Fatalf("authed REGISTER: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authed REGISTER status = %d, want 200", resp2.StatusCode)
	}

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 || list.Bindings[0].AppID != "acme" {
		t.Fatalf("bindings = %+v, want one owned by acme", list.Bindings)
	}
	active := inst.collector.waitForMatch(t, events.SIPRegistrationActive, nil, time.Second)
	if app := active.Data.GetAppID(); app != "acme" {
		t.Errorf("registration_active app_id = %q, want acme", app)
	}
}
