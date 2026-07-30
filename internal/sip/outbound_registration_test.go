package sip

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestParseGrantedExpires_FromContactParam(t *testing.T) {
	res := sip.NewResponse(sip.StatusOK, "OK")
	res.AppendHeader(sip.NewHeader("Contact", "<sip:alice@10.0.0.5:5060>;expires=120"))
	if got := parseGrantedExpires(res, 3600); got != 120 {
		t.Errorf("Contact expires=120 → %d, want 120", got)
	}
}

func TestParseGrantedExpires_FromTopLevelHeader(t *testing.T) {
	res := sip.NewResponse(sip.StatusOK, "OK")
	res.AppendHeader(sip.NewHeader("Expires", "300"))
	if got := parseGrantedExpires(res, 3600); got != 300 {
		t.Errorf("Expires:300 → %d, want 300", got)
	}
}

func TestParseGrantedExpires_FallsBackToRequested(t *testing.T) {
	res := sip.NewResponse(sip.StatusOK, "OK")
	if got := parseGrantedExpires(res, 3600); got != 3600 {
		t.Errorf("no header → %d, want 3600 (requested)", got)
	}
}

func TestOutboundRegistrationConfig_DefaultsApplied(t *testing.T) {
	c := OutboundRegistrationConfig{}.withDefaults()
	if c.DefaultExpiresSeconds != 3600 {
		t.Errorf("DefaultExpiresSeconds = %d, want 3600", c.DefaultExpiresSeconds)
	}
	if c.MinExpiresSeconds != 60 {
		t.Errorf("MinExpiresSeconds = %d, want 60", c.MinExpiresSeconds)
	}
	if c.MaxExpiresSeconds != 7200 {
		t.Errorf("MaxExpiresSeconds = %d, want 7200", c.MaxExpiresSeconds)
	}
	if c.RefreshRatio != 0.5 {
		t.Errorf("RefreshRatio = %v, want 0.5", c.RefreshRatio)
	}
}

func TestOutboundRegistrationConfig_RefreshRatioGuards(t *testing.T) {
	// Out-of-range values clamp back to the default ratio.
	for _, in := range []float64{0, -0.1, 1.0, 1.5} {
		got := OutboundRegistrationConfig{RefreshRatio: in}.withDefaults().RefreshRatio
		if got != 0.5 {
			t.Errorf("RefreshRatio(%v) → %v, want 0.5", in, got)
		}
	}
}

func TestExtractSource(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"10.0.0.5:5060", "10.0.0.5", 5060},
		{"[::1]:5060", "::1", 5060},
		{"", "", 0},
		{"malformed", "", 0},
	}
	for _, c := range cases {
		h, p := extractSource(c.in)
		if h != c.wantHost || p != c.wantPort {
			t.Errorf("extractSource(%q) = (%q,%d), want (%q,%d)", c.in, h, p, c.wantHost, c.wantPort)
		}
	}
}

func TestRegisterRejectionIsPermanent(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{401, true},
		{403, true},
		{407, true},
		// 404 is transient on purpose: registrars commonly 404 an AOR that
		// has only just been provisioned.
		{404, false},
		{408, false},
		{423, false},
		{480, false},
		{500, false},
		{503, false},
		{200, false},
		// 0 is what markFailed reports for a transport error (no response).
		{0, false},
	}
	for _, c := range cases {
		if got := registerRejectionIsPermanent(c.code); got != c.want {
			t.Errorf("registerRejectionIsPermanent(%d) = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestOutboundRegistration_NoteAuthRejection(t *testing.T) {
	t.Run("threshold", func(t *testing.T) {
		r := &OutboundRegistration{cfg: OutboundRegistrationConfig{AuthFailureLimit: 3}}
		want := []bool{false, false, true, true}
		for i, w := range want {
			if got := r.noteAuthRejection(); got != w {
				t.Errorf("call %d → %v, want %v", i+1, got, w)
			}
		}
	})

	t.Run("zero limit never terminates", func(t *testing.T) {
		r := &OutboundRegistration{cfg: OutboundRegistrationConfig{AuthFailureLimit: 0}}
		for i := 0; i < 10; i++ {
			if r.noteAuthRejection() {
				t.Fatalf("call %d returned true with AuthFailureLimit=0", i+1)
			}
		}
	})

	t.Run("success resets the run", func(t *testing.T) {
		r := &OutboundRegistration{cfg: OutboundRegistrationConfig{AuthFailureLimit: 3}}
		r.noteAuthRejection()
		r.noteAuthRejection()
		// What the success path in registerOnce does.
		r.mu.Lock()
		r.authFailures = 0
		r.mu.Unlock()
		for i := 0; i < 2; i++ {
			if r.noteAuthRejection() {
				t.Fatalf("call %d after reset returned true", i+1)
			}
		}
	})
}

func TestOutboundRegistration_MarkTerminated(t *testing.T) {
	m := NewTrunkManager()
	e := &Engine{trunks: m}
	bus := events.NewBus("test")
	var mu sync.Mutex
	var seen []events.Event
	bus.Subscribe(func(ev events.Event) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})

	r := &OutboundRegistration{
		id:     "t1",
		engine: e,
		bus:    bus,
		log:    slog.Default(),
		aor:    sip.Uri{Scheme: "sip", User: "alice", Host: "vb.test"},
		// Deliberately not the zero value, so the status assertion below
		// cannot pass by accident.
		status:       TrunkStatusFailed,
		peerHost:     "10.0.0.1",
		peerPort:     5060,
		registrarURI: sip.Uri{Scheme: "sip", Host: "10.0.0.1", Port: 5060},
	}
	m.Add(r)

	r.markTerminated(403, "Forbidden")

	r.mu.RLock()
	status, lastErr := r.status, r.lastError
	r.mu.RUnlock()
	if status != TrunkStatusTerminated {
		t.Errorf("status = %q, want %q", status, TrunkStatusTerminated)
	}
	if lastErr != "Forbidden" {
		t.Errorf("lastError = %q, want %q", lastErr, "Forbidden")
	}
	if m.Get("t1") == nil {
		t.Error("trunk removed from byID; it must stay inspectable")
	}
	if got := m.LookupByFromAOR(r.AOR()); got != nil {
		t.Errorf("LookupByFromAOR still resolves after markTerminated: %v", got)
	}
	if got := m.LookupByPeerSocket("10.0.0.1", 5060); got != nil {
		t.Errorf("LookupByPeerSocket still resolves after markTerminated: %v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("published %d events, want 2: %+v", len(seen), seen)
	}
	if seen[0].Type != events.SIPOutboundRegistrationFailed {
		t.Errorf("event[0] = %v, want %v", seen[0].Type, events.SIPOutboundRegistrationFailed)
	}
	failed, ok := seen[0].Data.(*events.SIPOutboundRegistrationFailedData)
	if !ok {
		t.Fatalf("event[0] data type = %T", seen[0].Data)
	}
	if failed.StatusCode != 403 {
		t.Errorf("failed.StatusCode = %d, want 403", failed.StatusCode)
	}
	if seen[1].Type != events.SIPOutboundRegistrationExpired {
		t.Errorf("event[1] = %v, want %v", seen[1].Type, events.SIPOutboundRegistrationExpired)
	}
	expired, ok := seen[1].Data.(*events.SIPOutboundRegistrationExpiredData)
	if !ok {
		t.Fatalf("event[1] data type = %T", seen[1].Data)
	}
	if expired.Reason != "credentials_rejected" {
		t.Errorf("expired.Reason = %q, want %q", expired.Reason, "credentials_rejected")
	}
}

// ---------------------------------------------------------------------------
// stubRegistrar — in-package UDP registrar for the end-to-end loop tests.
// Modelled on tests/integration/sip_trunks_test.go's rawSIPRegistrar, trimmed
// to the rejection scenarios this file needs and kept in internal/sip so the
// suite that actually gates merges covers the loop exit.
// ---------------------------------------------------------------------------

type stubRegistrarOpts struct {
	// challenge issues a 401 with WWW-Authenticate for any REGISTER that
	// arrives without an Authorization header, so the trunk's digest retry
	// runs before the rejection. With challenge false the rejection lands on
	// the very first, unauthenticated REGISTER.
	challenge    bool
	rejectCode   int
	rejectReason string
}

type stubRegistrar struct {
	host string
	port int

	opts stubRegistrarOpts

	mu    sync.Mutex
	count int
}

func (s *stubRegistrar) registerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func newStubRegistrar(t *testing.T, opts stubRegistrarOpts) *stubRegistrar {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("stub-registrar"),
		sipgo.WithUserAgentHostname("127.0.0.1"),
	)
	if err != nil {
		t.Fatalf("new UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	s := &stubRegistrar{host: "127.0.0.1", port: port, opts: opts}
	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		s.mu.Lock()
		s.count++
		s.mu.Unlock()
		if s.opts.challenge && req.GetHeader("Authorization") == nil {
			res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
			res.AppendHeader(sip.NewHeader("WWW-Authenticate",
				`Digest realm="vb-test", nonce="abcdef1234567890", algorithm=MD5`))
			_ = tx.Respond(res)
			return
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, s.opts.rejectCode, s.opts.rejectReason, nil))
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = srv.ListenAndServe(ctx, "udp", fmt.Sprintf("127.0.0.1:%d", port))
	}()
	t.Cleanup(func() {
		cancel()
		ua.Close()
	})
	// Give the listener a moment to bind; every assertion below polls, so a
	// slow bind only costs latency, not correctness.
	time.Sleep(100 * time.Millisecond)
	return s
}

func newRegistrationTestEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: pickFreePort(t, "udp"),
		SIPHost:  "test-vb",
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = engine.Serve(ctx) }()
	t.Cleanup(cancel)
	return engine
}

func startTestTrunk(t *testing.T, engine *Engine, bus *events.Bus, reg *stubRegistrar, limit int) *OutboundRegistration {
	t.Helper()
	trunk := NewOutboundRegistration(engine, bus,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		OutboundRegistrationConfig{
			AuthFailureLimit:  limit,
			FailureBackoffMax: 50 * time.Millisecond,
		},
		OutboundRegistrationParams{
			ID:           "t1",
			RegistrarURI: sip.Uri{Scheme: "sip", Host: reg.host, Port: reg.port},
			AOR:          sip.Uri{Scheme: "sip", User: "alice", Host: "vb.test"},
			Username:     "alice",
			Password:     "secret",
		})
	// Add before Start, matching internal/api/sip_trunks.go — otherwise a
	// fast termination could Deindex before the trunk is even indexed.
	engine.Trunks().Add(trunk)
	trunk.Start(context.Background())
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = trunk.Stop(stopCtx)
	})
	return trunk
}

func waitForStatus(t *testing.T, r *OutboundRegistration, want TrunkStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.RLock()
		got := r.status
		r.mu.RUnlock()
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.RLock()
	got := r.status
	r.mu.RUnlock()
	t.Fatalf("status never reached %q (stuck at %q)", want, got)
}

func TestOutboundRegistration_TerminatesAfterRepeatedAuthRejection(t *testing.T) {
	reg := newStubRegistrar(t, stubRegistrarOpts{challenge: true, rejectCode: 403, rejectReason: "Forbidden"})
	engine := newRegistrationTestEngine(t)
	m := engine.Trunks()

	bus := events.NewBus("test")
	var mu sync.Mutex
	var seen []events.Event
	bus.Subscribe(func(ev events.Event) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})

	trunk := startTestTrunk(t, engine, bus, reg, 2)
	waitForStatus(t, trunk, TrunkStatusTerminated, 5*time.Second)

	// The storm has stopped: no further REGISTER leaves the box.
	before := reg.registerCount()
	time.Sleep(500 * time.Millisecond)
	if after := reg.registerCount(); after != before {
		t.Errorf("REGISTER count kept climbing after termination: %d → %d", before, after)
	}

	// Still inspectable, but no longer routable.
	if m.Get("t1") == nil {
		t.Error("terminated trunk vanished from byID")
	}
	if got := m.LookupByFromAOR(trunk.AOR()); got != nil {
		t.Errorf("terminated trunk still matches POST /v1/legs: %v", got)
	}

	mu.Lock()
	var reason string
	for _, ev := range seen {
		if ev.Type == events.SIPOutboundRegistrationExpired {
			if d, ok := ev.Data.(*events.SIPOutboundRegistrationExpiredData); ok {
				reason = d.Reason
			}
		}
	}
	mu.Unlock()
	if reason != "credentials_rejected" {
		t.Errorf("expired reason = %q, want %q", reason, "credentials_rejected")
	}

	// Stop must not put the rejected credentials back on the wire.
	beforeStop := reg.registerCount()
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = trunk.Stop(stopCtx)
	if after := reg.registerCount(); after != beforeStop {
		t.Errorf("Stop on a terminated trunk sent %d REGISTER(s); want 0", after-beforeStop)
	}
}

func TestOutboundRegistration_UnauthenticatedRejectionKeepsRetrying(t *testing.T) {
	// A 403 to an un-credentialed REGISTER says nothing about the password —
	// VoiceBlender's own registrar answers exactly this when no client
	// decides an inbound REGISTER inside the consult window. It must stay on
	// the retry path however often it repeats.
	reg := newStubRegistrar(t, stubRegistrarOpts{challenge: false, rejectCode: 403, rejectReason: "Forbidden"})
	engine := newRegistrationTestEngine(t)
	trunk := startTestTrunk(t, engine, events.NewBus("test"), reg, 2)

	waitForStatus(t, trunk, TrunkStatusFailed, 5*time.Second)
	before := reg.registerCount()
	time.Sleep(500 * time.Millisecond)
	if after := reg.registerCount(); after <= before {
		t.Errorf("REGISTER count froze at %d; a pre-digest 403 must keep retrying", after)
	}

	trunk.mu.RLock()
	status := trunk.status
	trunk.mu.RUnlock()
	if status == TrunkStatusTerminated {
		t.Error("pre-digest 403 terminated the trunk; a consult-timeout reject would kill a working trunk")
	}
	if got := engine.Trunks().LookupByFromAOR(trunk.AOR()); got == nil {
		t.Error("trunk was deindexed despite a transient rejection")
	}
}

func TestOutboundRegistration_ChallengelessUnauthorizedTerminates(t *testing.T) {
	// A 401 carrying no WWW-Authenticate offers nothing to authenticate
	// against — no credential we could compute would change the answer, so
	// it is a refusal rather than a challenge.
	reg := newStubRegistrar(t, stubRegistrarOpts{challenge: false, rejectCode: 401, rejectReason: "Unauthorized"})
	engine := newRegistrationTestEngine(t)
	trunk := startTestTrunk(t, engine, events.NewBus("test"), reg, 2)

	waitForStatus(t, trunk, TrunkStatusTerminated, 5*time.Second)
	before := reg.registerCount()
	time.Sleep(400 * time.Millisecond)
	if after := reg.registerCount(); after != before {
		t.Errorf("REGISTER count kept climbing after termination: %d → %d", before, after)
	}
	if got := engine.Trunks().LookupByFromAOR(trunk.AOR()); got != nil {
		t.Errorf("terminated trunk still matches POST /v1/legs: %v", got)
	}
}

func TestOutboundRegistration_NonAuthRejectionKeepsRetrying(t *testing.T) {
	// 404 after a full digest exchange is transient: registrars 404 an AOR
	// that has only just been provisioned.
	reg := newStubRegistrar(t, stubRegistrarOpts{challenge: true, rejectCode: 404, rejectReason: "Not Found"})
	engine := newRegistrationTestEngine(t)
	trunk := startTestTrunk(t, engine, events.NewBus("test"), reg, 2)

	waitForStatus(t, trunk, TrunkStatusFailed, 5*time.Second)
	before := reg.registerCount()
	time.Sleep(500 * time.Millisecond)
	if after := reg.registerCount(); after <= before {
		t.Errorf("REGISTER count froze at %d; a 404 must keep retrying", after)
	}

	trunk.mu.RLock()
	status := trunk.status
	trunk.mu.RUnlock()
	if status == TrunkStatusTerminated {
		t.Error("404 terminated the trunk; only 401/403/407 are credential rejections")
	}
}
