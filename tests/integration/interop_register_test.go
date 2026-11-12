//go:build integration && interop

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

func registerPeer(t *testing.T, inst *testInstance, p peers.Peer, user, password string, expires int) peers.Registration {
	t.Helper()
	reg, err := p.Register(interopCtx(t, 5*time.Second), peers.RegisterOpts{
		Registrar: fmt.Sprintf("127.0.0.1:%d", inst.sipPort),
		User:      user,
		Password:  password,
		Expires:   expires,
	})
	if err != nil {
		t.Fatalf("peer register: %v", err)
	}
	t.Cleanup(func() { reg.Unregister() })
	return reg
}

func bindingFor(inst *testInstance, t *testing.T, user string) (api.RegistrationView, bool) {
	t.Helper()
	for _, b := range registrationsList(t, inst.baseURL()).Bindings {
		if strings.Contains(b.AOR, user+"@") {
			return b, true
		}
	}
	return api.RegistrationView{}, false
}

func waitBinding(t *testing.T, inst *testInstance, user string, present bool, timeout time.Duration) api.RegistrationView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, ok := bindingFor(inst, t, user); ok == present {
			return b
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("binding for %s present=%v not reached within %v", user, present, timeout)
	return api.RegistrationView{}
}

// The peer registers to VoiceBlender, VoiceBlender calls the AOR and reaches
// the peer through the registered contact, and the peer unregisters cleanly.
func TestInterop_Register_PeerRegistersToVB(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Register, "registering as a UAC")
		inst := newInteropVB(t, "vb", nil)
		reg := registerPeer(t, inst, p, "alice", "unused", 60)
		if err := reg.WaitRegistered(interopCtx(t, 10*time.Second)); err != nil {
			t.Fatal(err)
		}
		b := waitBinding(t, inst, "alice", true, 5*time.Second)
		t.Logf("binding: aor=%s contact=%s socket=%s", b.AOR, b.Contact, b.Socket)

		t.Run("vb-dials-aor", func(t *testing.T) {
			legID, call := connectVBToPeer(t, inst, p, interopDial{To: b.AOR, Codecs: []string{"PCMU"}}, peers.AnswerOpts{})
			assertMediaBothWays(t, inst, legID, call)
			hangupOnVB(t, inst, legID)
			waitPeerHangup(t, call, 5*time.Second)
		})
		t.Run("cancel-unanswered", func(t *testing.T) {
			legID, call := vbDialsPeer(t, inst, p, interopDial{To: b.AOR, Codecs: []string{"PCMU"}})
			hangupOnVB(t, inst, legID)
			if info := waitPeerHangup(t, call, 5*time.Second); info.Cause != "ORIGINATOR_CANCEL" {
				t.Errorf("peer cause %s, want ORIGINATOR_CANCEL: the CANCEL did not reach the contact", info.Cause)
			}
		})
		t.Run("unregister", func(t *testing.T) {
			if err := reg.Unregister(); err != nil {
				t.Fatal(err)
			}
			waitBinding(t, inst, "alice", false, 5*time.Second)
		})
	})
}

// VoiceBlender grants a shorter expiry than asked; the peer must refresh
// within it and the binding must survive.
func TestInterop_Register_Refresh(t *testing.T) {
	if testing.Short() {
		t.Skip("waits through a registration refresh")
	}
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Register, "registering as a UAC")
		// 60 s is the registrar's floor; the peer asks for more.
		inst := newInteropVB(t, "vb", func(c *config.Config) { c.SIPRegistrationMaxExpiresSeconds = 60 })
		reg := registerPeer(t, inst, p, "carol", "unused", 120)
		if err := reg.WaitRegistered(interopCtx(t, 10*time.Second)); err != nil {
			t.Fatal(err)
		}
		first := waitBinding(t, inst, "carol", true, 5*time.Second)
		if first.GrantedExpiresSeconds > 60 {
			t.Errorf("granted %d s, want at most 60", first.GrantedExpiresSeconds)
		}
		time.Sleep(70 * time.Second)
		b, ok := bindingFor(inst, t, "carol")
		if !ok {
			t.Fatal("binding expired: the peer did not refresh within the granted interval")
		}
		if b.LastRefresh == first.LastRefresh {
			t.Errorf("binding never refreshed (last_refresh %s)", b.LastRefresh)
		}
	})
}

// challengeAllAttempts answers every parked REGISTER attempt with a digest
// challenge until the test ends.
func challengeAllAttempts(t *testing.T, inst *testInstance, realm, user, pass string) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		done := map[string]bool{}
		for ctx.Err() == nil {
			for _, e := range inst.collector.matchAll(events.SIPRegistrationAttempt, nil) {
				id := e.Data.(*events.SIPRegistrationAttemptData).AttemptID
				if done[id] {
					continue
				}
				done[id] = true
				resp := httpPost(t, fmt.Sprintf("%s/v1/sip/registrations/attempts/%s/challenge", inst.baseURL(), id),
					map[string]interface{}{"realm": realm, "username": user, "password": pass})
				resp.Body.Close()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

func TestInterop_Register_DigestChallenge(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Register, "registering as a UAC")
		for _, tc := range []struct {
			name     string
			password string
			ok       bool
		}{
			{"right-password", "s3cret", true},
			{"wrong-password", "wrong", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				inst := newInteropVB(t, "vb", func(c *config.Config) {
					c.SIPInboundAuthConsultTimeoutMs = 4000
					c.SIPInboundRegisterDefault = "reject"
				})
				challengeAllAttempts(t, inst, "vb.test", "dave", "s3cret")
				reg := registerPeer(t, inst, p, "dave", tc.password, 60)
				err := reg.WaitRegistered(interopCtx(t, 8*time.Second))
				_, bound := bindingFor(inst, t, "dave")
				if tc.ok && (err != nil || !bound) {
					t.Fatalf("registration with the right password failed: %v (bound=%v)", err, bound)
				}
				if !tc.ok && (err == nil || bound) {
					t.Fatalf("registration with a wrong password succeeded (bound=%v)", bound)
				}
			})
		}
	})
}

// VoiceBlender challenges the peer's INVITE; the peer retries with
// credentials and the call proceeds on the authenticated leg.
func TestInterop_Auth_VBChallengesInvite(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		call, lv := peerDialsVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}, AuthUser: "bob", AuthPassword: "hunter2"})
		resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/challenge", inst.baseURL(), lv.ID),
			map[string]interface{}{"realm": "vb.test", "username": "bob", "password": "hunter2"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("challenge: status %d", resp.StatusCode)
		}
		ev := inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
			return e.Data.(*events.LegRingingData).Authenticated
		}, 10*time.Second)
		d := ev.Data.(*events.LegRingingData)
		if d.AuthUsername != "bob" {
			t.Errorf("auth_username = %q, want bob", d.AuthUsername)
		}
		answerOnVB(t, inst, d.LegID, nil)
		waitForLegState(t, inst.baseURL(), d.LegID, "connected", 5*time.Second)
		if err := call.WaitAnswered(interopCtx(t, 5*time.Second)); err != nil {
			t.Fatal(err)
		}
		assertMediaBothWays(t, inst, d.LegID, call)
	})
}

// The peer challenges VoiceBlender's INVITE; the leg's credentials answer it.
func TestInterop_Auth_PeerChallengesInvite(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Registrar, "challenging INVITEs")
		inst := newInteropVB(t, "vb", nil)
		ri := p.Registrar()
		var user, pass string
		for u, pw := range ri.Users {
			user, pass = u, pw
			break
		}
		legID, call := connectVBToPeer(t, inst, p, interopDial{
			Auth:   true,
			Codecs: []string{"PCMU"},
			Extra:  map[string]interface{}{"auth": map[string]string{"username": user, "password": pass}},
		}, peers.AnswerOpts{})
		assertMediaBothWays(t, inst, legID, call)

		bad, _ := vbDialsPeerNoWait(t, inst, p, interopDial{
			Auth:   true,
			Codecs: []string{"PCMU"},
			Extra:  map[string]interface{}{"auth": map[string]string{"username": user, "password": "wrong"}},
		})
		if got := waitDisconnect(t, inst, bad, 10*time.Second).CDR.Reason; got != "unauthorized" && got != "forbidden" {
			t.Errorf("wrong password: reason %q, want unauthorized or forbidden", got)
		}
	})
}
