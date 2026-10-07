package sip

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func proxyURIPtr(t *testing.T, raw string) *sip.Uri {
	t.Helper()
	u, err := ParseProxyURI(raw)
	if err != nil {
		t.Fatalf("ParseProxyURI(%q): %v", raw, err)
	}
	return &u
}

func TestIPIPTrunk_Identity(t *testing.T) {
	aor := mustParseURI(t, "sip:Acme@Carrier.Example:5060;transport=tcp")
	tr := NewIPIPTrunk(nil, nil, nil, IPIPTrunkParams{
		ID:            "t1",
		AppID:         "app",
		PeerURI:       *proxyURIPtr(t, "sip:203.0.113.10:5070;transport=tcp"),
		OutboundProxy: proxyURIPtr(t, "sip:edge.acme.net"),
		AOR:           &aor,
		Password:      "secret",
	})

	if tr.ID() != "t1" || tr.Type() != TrunkTypeIPIP || tr.AppID() != "app" {
		t.Errorf("id/type/app = %s/%s/%s", tr.ID(), tr.Type(), tr.AppID())
	}
	if got := tr.AOR(); got != "sip:Acme@carrier.example:5060" {
		t.Errorf("AOR = %q", got)
	}
	if got := tr.ContactUser(); got != "" {
		t.Errorf("ContactUser = %q, want empty", got)
	}
	// The peer, not the proxy in front of it.
	if host, port, transport := tr.PeerSocket(); host != "203.0.113.10" || port != 5070 || transport != "tcp" {
		t.Errorf("PeerSocket = %s %d %s, want the peer", host, port, transport)
	}
	if got := tr.OutboundRoute(sip.Uri{}).AuthUsername; got != "Acme" {
		t.Errorf("username = %q, want the AOR user", got)
	}

	bare := NewIPIPTrunk(nil, nil, nil, IPIPTrunkParams{ID: "t2", PeerURI: *proxyURIPtr(t, "sip:pbx.example")})
	if bare.AOR() != "" {
		t.Errorf("AOR = %q, want empty without one", bare.AOR())
	}
}

func TestIPIPTrunk_OutboundRoute(t *testing.T) {
	tests := []struct {
		name      string
		peer      string
		proxy     string
		recipient string
		wantRoute string
		wantProxy string
	}{
		{name: "recipient is the peer", peer: "sip:203.0.113.10", recipient: "sip:+15551234567@203.0.113.10"},
		{name: "recipient names the peer port", peer: "sip:203.0.113.10", recipient: "sip:bob@203.0.113.10:5060"},
		{name: "recipient elsewhere", peer: "sip:203.0.113.10", recipient: "sip:bob@pbx.example", wantRoute: "sip:203.0.113.10"},
		{name: "different port", peer: "sip:203.0.113.10:5070", recipient: "sip:bob@203.0.113.10", wantRoute: "sip:203.0.113.10:5070"},
		{name: "different transport", peer: "sip:203.0.113.10;transport=tcp", recipient: "sip:bob@203.0.113.10", wantRoute: "sip:203.0.113.10;transport=tcp"},
		{name: "proxy with recipient at peer", peer: "sip:203.0.113.10", proxy: "sip:edge.acme.net", recipient: "sip:bob@203.0.113.10", wantProxy: "sip:edge.acme.net"},
		{name: "proxy with recipient elsewhere", peer: "sip:203.0.113.10", proxy: "sip:edge.acme.net", recipient: "sip:bob@pbx.example", wantRoute: "sip:203.0.113.10", wantProxy: "sip:edge.acme.net"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := IPIPTrunkParams{ID: "t", PeerURI: *proxyURIPtr(t, tc.peer), Username: "u", Password: "p"}
			if tc.proxy != "" {
				p.OutboundProxy = proxyURIPtr(t, tc.proxy)
			}
			route := NewIPIPTrunk(nil, nil, nil, p).OutboundRoute(mustParseURI(t, tc.recipient))
			if got := proxyString(route.RouteURI); got != tc.wantRoute {
				t.Errorf("RouteURI = %q, want %q", got, tc.wantRoute)
			}
			if got := proxyString(route.ProxyURI); got != tc.wantProxy {
				t.Errorf("ProxyURI = %q, want %q", got, tc.wantProxy)
			}
			if route.AuthUsername != "u" || route.AuthPassword != "p" || route.FromHost != "" {
				t.Errorf("route = %+v, want credentials and no From host", route)
			}
		})
	}
}

// The registrar authenticated the identity and expects to stay in the path, so
// its Route is kept even when the Request-URI already targets it.
func TestOutboundRegistration_OutboundRoute(t *testing.T) {
	r := newTrunkTo(nil, 5060)
	route := r.OutboundRoute(mustParseURI(t, "sip:bob@127.0.0.1:5060"))
	if got := proxyString(route.RouteURI); got != "sip:127.0.0.1:5060" {
		t.Errorf("RouteURI = %q, want the registrar", got)
	}
	if route.ProxyURI != nil {
		t.Errorf("ProxyURI = %v, want nil", route.ProxyURI)
	}
	if route.FromHost != "127.0.0.1" || route.AuthUsername != "alice" || route.AuthPassword != "secret" {
		t.Errorf("route = %+v", route)
	}
}

func TestIPIPTrunk_MatchInboundSource(t *testing.T) {
	sources, err := ParseSourcePrefixes([]string{"10.1.0.0/16", "10.1.2.0/24", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}
	tr := NewIPIPTrunk(nil, nil, nil, IPIPTrunkParams{
		ID:             "t",
		PeerURI:        *proxyURIPtr(t, "sip:203.0.113.10:5070"),
		OutboundProxy:  proxyURIPtr(t, "sip:192.0.2.99"),
		InboundSources: sources,
	})

	tests := []struct {
		src       string
		wantBits  int
		wantExact bool
		wantOK    bool
	}{
		{"10.1.9.9:5060", 16, false, true},
		{"10.1.2.9:5060", 24, false, true},
		{"[2001:db8::7]:5060", 32, false, true},
		{"203.0.113.10:5070", 32, true, true},
		{"203.0.113.10:40000", 32, false, true},
		{"192.0.2.99:5060", 0, false, false}, // the proxy is not a source
		{"10.2.0.1:5060", 0, false, false},
	}
	for _, tc := range tests {
		bits, exact, ok := tr.MatchInboundSource(netip.MustParseAddrPort(tc.src))
		if bits != tc.wantBits || exact != tc.wantExact || ok != tc.wantOK {
			t.Errorf("MatchInboundSource(%s) = %d %v %v, want %d %v %v", tc.src, bits, exact, ok, tc.wantBits, tc.wantExact, tc.wantOK)
		}
	}

	hostname := NewIPIPTrunk(nil, nil, nil, IPIPTrunkParams{ID: "h", PeerURI: *proxyURIPtr(t, "sip:carrier.example")})
	if _, _, ok := hostname.MatchInboundSource(netip.MustParseAddrPort("10.1.9.9:5060")); ok {
		t.Error("a hostname peer with no sources matched an address")
	}
}

func TestIPIPTrunk_Snapshot(t *testing.T) {
	sources, err := ParseSourcePrefixes([]string{"198.51.100.0/24", "203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}
	aor := mustParseURI(t, "sip:acme@carrier.example")
	tr := NewIPIPTrunk(nil, nil, nil, IPIPTrunkParams{
		ID:             "t",
		AppID:          "app",
		PeerURI:        *proxyURIPtr(t, "sip:203.0.113.10"),
		OutboundProxy:  proxyURIPtr(t, "sip:edge.acme.net"),
		AOR:            &aor,
		Username:       "acct",
		Password:       "hunter2",
		InboundSources: sources,
		PingInterval:   30 * time.Second,
	})
	view := tr.Snapshot()
	if view.Type != TrunkTypeIPIP || view.Status != TrunkStatusActive || view.AppID != "app" || view.SIPRegister != nil {
		t.Fatalf("view = %+v", view)
	}
	ip := view.IPIP
	if ip.PeerURI != "sip:203.0.113.10" || ip.OutboundProxy != "sip:edge.acme.net" || ip.AOR != "sip:acme@carrier.example" || ip.Username != "acct" {
		t.Errorf("ip_ip view = %+v", ip)
	}
	// The peer literal was listed explicitly, so it is not repeated.
	if got := strings.Join(ip.InboundSources, ","); got != "198.51.100.0/24,203.0.113.10/32" {
		t.Errorf("inbound_sources = %q", got)
	}
	if ip.OptionsPingIntervalSeconds != 30 || ip.LastPingAt != "" || ip.LastPingStatusCode != 0 {
		t.Errorf("ping fields = %d %q %d", ip.OptionsPingIntervalSeconds, ip.LastPingAt, ip.LastPingStatusCode)
	}
	out, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hunter2") || strings.Contains(string(out), "password") {
		t.Errorf("snapshot leaks the password: %s", out)
	}

	bare := NewIPIPTrunk(nil, nil, nil, IPIPTrunkParams{ID: "h", PeerURI: *proxyURIPtr(t, "sip:carrier.example")})
	out, err = json.Marshal(bare.Snapshot().IPIP)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"inbound_sources":[]`) {
		t.Errorf("view = %s, want an empty inbound_sources array", out)
	}
}

func TestIPIPTrunk_TLSTrust(t *testing.T) {
	e := newTrustTestEngine(t)

	tr := NewIPIPTrunk(e, nil, nil, IPIPTrunkParams{
		ID: "t", PeerURI: *proxyURIPtr(t, "sips:sbc.example.net:5061"), TLSInsecureSkipVerify: true,
	})
	if !e.peerTrust.trusted("sbc.example.net") {
		t.Fatal("trunk did not exempt its peer host")
	}
	if err := tr.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e.peerTrust.trusted("sbc.example.net") {
		t.Error("exemption survived the trunk")
	}

	// The handshake is with the proxy, so that is the host exempted.
	proxied := NewIPIPTrunk(e, nil, nil, IPIPTrunkParams{
		ID: "p", PeerURI: *proxyURIPtr(t, "sip:sbc.example.net"),
		OutboundProxy: proxyURIPtr(t, "sips:edge.example.net"), TLSInsecureSkipVerify: true,
	})
	if !e.peerTrust.trusted("edge.example.net") || e.peerTrust.trusted("sbc.example.net") {
		t.Error("want the proxy exempted and the peer not")
	}
	_ = proxied.Stop(context.Background())

	NewIPIPTrunk(e, nil, nil, IPIPTrunkParams{ID: "off", PeerURI: *proxyURIPtr(t, "sips:other.example.net")})
	if e.peerTrust.trusted("other.example.net") {
		t.Error("peer exempted without tls_insecure_skip_verify")
	}
	NewIPIPTrunk(e, nil, nil, IPIPTrunkParams{
		ID: "udp", PeerURI: *proxyURIPtr(t, "sip:plain.example.net"), TLSInsecureSkipVerify: true,
	})
	if e.peerTrust.trusted("plain.example.net") {
		t.Error("a non-TLS peer was exempted")
	}
}

func TestOptionsReplyIsUp(t *testing.T) {
	up := []int{200, 202, 301, 401, 403, 404, 405, 407, 480, 486, 501, 603}
	down := []int{408, 500, 502, 503, 504, 580}
	for _, code := range up {
		if !optionsReplyIsUp(code) {
			t.Errorf("optionsReplyIsUp(%d) = false, want true", code)
		}
	}
	for _, code := range down {
		if optionsReplyIsUp(code) {
			t.Errorf("optionsReplyIsUp(%d) = true, want false", code)
		}
	}
}

// fakeOptionsPeer is a minimal UAS that records the OPTIONS it receives and
// answers with a switchable status; 0 stays silent.
type fakeOptionsPeer struct {
	port   int
	status atomic.Int32

	mu   sync.Mutex
	seen []*sip.Request
}

func startFakeOptionsPeer(t *testing.T, status int) *fakeOptionsPeer {
	t.Helper()
	peer := &fakeOptionsPeer{port: pickFreePort(t, "udp")}
	peer.status.Store(int32(status))

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("options-peer-test"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		peer.mu.Lock()
		peer.seen = append(peer.seen, req)
		peer.mu.Unlock()
		if code := int(peer.status.Load()); code != 0 {
			_ = tx.Respond(sip.NewResponseFromRequest(req, code, "Test Reply", nil))
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.ListenAndServe(ctx, "udp", fmt.Sprintf("127.0.0.1:%d", peer.port)) }()
	t.Cleanup(func() {
		cancel()
		ua.Close()
	})
	// Let the listener bind before the first ping.
	time.Sleep(50 * time.Millisecond)
	return peer
}

func (p *fakeOptionsPeer) received() []*sip.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*sip.Request(nil), p.seen...)
}

func (p *fakeOptionsPeer) uri() sip.Uri {
	return sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: p.port}
}

func newPingTrunk(t *testing.T, p IPIPTrunkParams) (*IPIPTrunk, *capturingBus) {
	t.Helper()
	bus := events.NewBus("test")
	cap := newCapturingBus()
	bus.Subscribe(cap.handle)
	p.ID = "t-ping"
	p.AppID = "app"
	if p.PingInterval == 0 {
		p.PingInterval = 40 * time.Millisecond
	}
	tr := NewIPIPTrunk(newNATTestEngine(t), bus, nil, p)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tr.Stop(ctx)
	})
	return tr, cap
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestIPIPTrunk_PingUpDownUp(t *testing.T) {
	peer := startFakeOptionsPeer(t, 200)
	tr, cap := newPingTrunk(t, IPIPTrunkParams{PeerURI: peer.uri()})
	tr.Start(context.Background())
	tr.Start(context.Background()) // a second Start must not double the loop

	waitFor(t, "first sip.trunk_up", func() bool { return len(cap.byType(events.SIPTrunkUp)) == 1 })
	up := cap.byType(events.SIPTrunkUp)[0].Data.(*events.SIPTrunkStatusData)
	if up.TrunkID != "t-ping" || up.TrunkType != "ip_ip" || up.AppID != "app" || up.StatusCode != 200 {
		t.Errorf("up event = %+v", up)
	}
	if up.PeerURI != tr.peerURI.String() {
		t.Errorf("peer_uri = %q, want %q", up.PeerURI, tr.peerURI.String())
	}
	if view := tr.Snapshot(); view.Status != TrunkStatusActive || view.IPIP.LastPingStatusCode != 200 || view.IPIP.LastPingAt == "" {
		t.Errorf("view after up = %+v / %+v", view, view.IPIP)
	}

	peer.status.Store(503)
	waitFor(t, "sip.trunk_down", func() bool { return len(cap.byType(events.SIPTrunkDown)) == 1 })
	down := cap.byType(events.SIPTrunkDown)[0].Data.(*events.SIPTrunkStatusData)
	if down.StatusCode != 503 || down.Reason != "Test Reply" {
		t.Errorf("down event = %+v", down)
	}
	if view := tr.Snapshot(); view.Status != TrunkStatusFailed || view.LastError != "503 Test Reply" {
		t.Errorf("view after down: status %q last_error %q", view.Status, view.LastError)
	}

	// A refusal still shows a serving peer.
	peer.status.Store(404)
	waitFor(t, "second sip.trunk_up", func() bool { return len(cap.byType(events.SIPTrunkUp)) == 2 })
	if view := tr.Snapshot(); view.Status != TrunkStatusActive || view.LastError != "" || view.IPIP.LastPingStatusCode != 404 {
		t.Errorf("view after recovery: status %q last_error %q code %d", view.Status, view.LastError, view.IPIP.LastPingStatusCode)
	}

	// Several more pings land while the peer stays up: no repeat events.
	seen := len(peer.received())
	waitFor(t, "further pings", func() bool { return len(peer.received()) >= seen+3 })
	if ups, downs := len(cap.byType(events.SIPTrunkUp)), len(cap.byType(events.SIPTrunkDown)); ups != 2 || downs != 1 {
		t.Errorf("events = %d up / %d down, want 2 / 1", ups, downs)
	}

	for _, req := range peer.received() {
		if req.GetHeader("Route") != nil {
			t.Fatalf("OPTIONS carries a Route:\n%s", req.String())
		}
	}
}

func TestIPIPTrunk_PingTimeoutMarksDown(t *testing.T) {
	peer := startFakeOptionsPeer(t, 0)
	tr, cap := newPingTrunk(t, IPIPTrunkParams{PeerURI: peer.uri()})
	tr.pingTimeout = 150 * time.Millisecond
	tr.Start(context.Background())

	waitFor(t, "sip.trunk_down", func() bool { return len(cap.byType(events.SIPTrunkDown)) == 1 })
	down := cap.byType(events.SIPTrunkDown)[0].Data.(*events.SIPTrunkStatusData)
	if down.StatusCode != 0 || down.Reason != "timeout" {
		t.Errorf("down event = %+v, want a timeout with no status", down)
	}
	if view := tr.Snapshot(); view.Status != TrunkStatusFailed || view.LastError != "timeout" || view.IPIP.LastPingStatusCode != 0 {
		t.Errorf("view: status %q last_error %q code %d", view.Status, view.LastError, view.IPIP.LastPingStatusCode)
	}
	if len(cap.byType(events.SIPTrunkUp)) != 0 {
		t.Error("sip.trunk_up published for a silent peer")
	}
}

func TestIPIPTrunk_StopDuringPingEmitsNothing(t *testing.T) {
	peer := startFakeOptionsPeer(t, 0)
	tr, cap := newPingTrunk(t, IPIPTrunkParams{PeerURI: peer.uri()})
	tr.Start(context.Background())
	waitFor(t, "the ping to reach the peer", func() bool { return len(peer.received()) > 0 })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stopped := time.Now()
	if err := tr.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(stopped); took > time.Second {
		t.Errorf("Stop took %v, want it not to wait out the ping", took)
	}
	if n := len(cap.byType(events.SIPTrunkDown)) + len(cap.byType(events.SIPTrunkUp)); n != 0 {
		t.Errorf("%d events published for a ping cut short by Stop", n)
	}
	if view := tr.Snapshot(); view.Status != TrunkStatusActive {
		t.Errorf("status = %q, want active", view.Status)
	}
}

func TestIPIPTrunk_PingDisabled(t *testing.T) {
	peer := startFakeOptionsPeer(t, 200)
	tr := NewIPIPTrunk(newNATTestEngine(t), nil, nil, IPIPTrunkParams{ID: "t", PeerURI: peer.uri()})
	tr.Start(context.Background())
	time.Sleep(200 * time.Millisecond)
	if n := len(peer.received()); n != 0 {
		t.Errorf("peer received %d OPTIONS with pings disabled", n)
	}
	if view := tr.Snapshot(); view.Status != TrunkStatusActive || view.IPIP.LastPingAt != "" {
		t.Errorf("view = %+v / %+v", view, view.IPIP)
	}
	if err := tr.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// The peer's name does not resolve: the ping can only succeed by being sent at
// the proxy, with the Request-URI left as the peer.
func TestIPIPTrunk_OptionsViaProxy(t *testing.T) {
	proxy := startFakeOptionsPeer(t, 200)
	proxyURI := proxy.uri()
	aor := mustParseURI(t, "sip:acme@carrier.example")
	tr, cap := newPingTrunk(t, IPIPTrunkParams{
		PeerURI:       sip.Uri{Scheme: "sip", Host: "peer.invalid"},
		OutboundProxy: &proxyURI,
		AOR:           &aor,
	})
	tr.Start(context.Background())

	waitFor(t, "sip.trunk_up", func() bool { return len(cap.byType(events.SIPTrunkUp)) == 1 })
	req := proxy.received()[0]
	if req.Recipient.Host != "peer.invalid" {
		t.Errorf("Request-URI host = %q, want the peer", req.Recipient.Host)
	}
	if req.GetHeader("Route") != nil {
		t.Errorf("OPTIONS carries a Route:\n%s", req.String())
	}
	if from := req.From(); from == nil || from.Address.User != "acme" || from.Address.Host != "carrier.example" {
		t.Errorf("From = %v, want the trunk AOR", from)
	}
}
