package sip

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// fakeRegistrar is a minimal UAS that records the REGISTERs it receives and
// answers them with a caller-supplied policy.
type fakeRegistrar struct {
	port int

	mu   sync.Mutex
	seen []*sip.Request
}

// startFakeRegistrar binds a UDP UAS on 127.0.0.1 and hands each REGISTER to
// respond, which returns the response to send or nil to stay silent (which is
// what a peer replying to an unreachable Via sent-by looks like from here).
func startFakeRegistrar(t *testing.T, respond func(n int, req *sip.Request) *sip.Response) *fakeRegistrar {
	t.Helper()
	reg := &fakeRegistrar{port: pickFreePort(t, "udp")}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("registrar-test"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		reg.mu.Lock()
		reg.seen = append(reg.seen, req)
		n := len(reg.seen)
		reg.mu.Unlock()
		if res := respond(n, req); res != nil {
			_ = tx.Respond(res)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.ListenAndServe(ctx, "udp", fmt.Sprintf("127.0.0.1:%d", reg.port)) }()
	t.Cleanup(func() {
		cancel()
		ua.Close()
	})
	return reg
}

func (f *fakeRegistrar) received() []*sip.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*sip.Request(nil), f.seen...)
}

func newNATTestEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: pickFreePort(t, "udp"),
		SIPHost:  "test-vb",
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

func newTrunkTo(engine *Engine, port int) *OutboundRegistration {
	registrar := sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: port}
	return NewOutboundRegistration(engine, nil, nil, OutboundRegistrationConfig{}, OutboundRegistrationParams{
		ID:           "t-nat",
		RegistrarURI: registrar,
		AOR:          sip.Uri{Scheme: "sip", User: "alice", Host: "127.0.0.1"},
		Username:     "alice",
		Password:     "secret",
	})
}

// TestOutboundRegistration_ViaOffersRport pins RFC 3581 on the REGISTER path.
// sipgo dials each destination from a fresh ephemeral socket while the Via
// sent-by is pinned to publicHost:bindPort, so a registrar that answers to
// sent-by instead of symmetrically can never reach us — behind NAT the pinned
// host is a private address. ;rport is what tells it to answer to the source.
func TestOutboundRegistration_ViaOffersRport(t *testing.T) {
	reg := startFakeRegistrar(t, func(_ int, req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	})
	engine := newNATTestEngine(t)
	r := newTrunkTo(engine, reg.port)

	var res *sip.Response
	var lastErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		res, lastErr = r.sendRegister(ctx, 60, "", "")
		cancel()
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("REGISTER never answered: %v", lastErr)
	}
	if res.StatusCode != sip.StatusOK {
		t.Fatalf("REGISTER status = %d, want 200", res.StatusCode)
	}

	seen := reg.received()
	if len(seen) == 0 {
		t.Fatal("registrar saw no REGISTER")
	}
	via := seen[len(seen)-1].Via()
	if via == nil {
		t.Fatal("REGISTER has no Via header")
	}
	if !via.Params.Has("rport") {
		t.Errorf("Via %q lacks ;rport; a registrar answering to sent-by would black-hole the response", via.Value())
	}
	if via.Host != engine.publicHost || via.Port != engine.bindPort {
		t.Errorf("Via sent-by = %s:%d, want the pinned %s:%d", via.Host, via.Port, engine.publicHost, engine.bindPort)
	}
}

// TestRegisterOnce_DigestRetryTransportFailure covers the path where the
// registrar challenges and then goes silent: registerOnce must report the
// failure, not dereference the nil response the timeout returns.
func TestRegisterOnce_DigestRetryTransportFailure(t *testing.T) {
	reg := startFakeRegistrar(t, func(_ int, req *sip.Request) *sip.Response {
		if req.GetHeader("Authorization") != nil {
			return nil // challenge answered, then silence
		}
		res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
		res.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="test", nonce="abc123"`))
		return res
	})
	engine := newNATTestEngine(t)
	r := newTrunkTo(engine, reg.port)

	// Serve binds asynchronously; wait for the challenge to come back before
	// the assertions, so a slow bind cannot masquerade as the silent retry.
	var challenged bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		res, err := r.sendRegister(ctx, 60, "", "")
		cancel()
		if err == nil && res.StatusCode == sip.StatusUnauthorized {
			challenged = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !challenged {
		t.Fatal("fake registrar never issued a challenge")
	}

	before := len(reg.received())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err := r.registerOnce(ctx, 60)
	cancel()
	if err == nil {
		t.Fatal("registerOnce returned nil; want the digest-retry transport error")
	}

	view := r.Snapshot()
	if view.Status != TrunkStatusFailed {
		t.Errorf("status = %q, want %q", view.Status, TrunkStatusFailed)
	}
	if !strings.HasPrefix(view.LastError, "digest retry: ") {
		t.Errorf("last_error = %q, want the digest retry failure", view.LastError)
	}
	if got := len(reg.received()) - before; got < 2 {
		t.Errorf("registrar saw %d REGISTERs from registerOnce, want the challenge plus the authenticated retry", got)
	}
}
