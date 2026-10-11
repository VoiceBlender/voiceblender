package sip

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestParseAllowedSources(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "empty", in: ""},
		{name: "whitespace", in: "  "},
		{name: "only separators", in: " , ,"},
		{name: "v4 literal", in: "203.0.113.7", want: []string{"203.0.113.7/32"}},
		{name: "v6 literal", in: "2001:db8::1", want: []string{"2001:db8::1/128"}},
		{name: "v4 cidr", in: "10.0.0.0/8", want: []string{"10.0.0.0/8"}},
		{name: "v6 cidr", in: "2001:db8::/32", want: []string{"2001:db8::/32"}},
		{name: "host bits masked", in: "10.1.2.3/8", want: []string{"10.0.0.0/8"}},
		{name: "mixed with whitespace", in: " 10.0.0.0/8 , ,::1 ", want: []string{"10.0.0.0/8", "::1/128"}},
		{name: "v4-mapped literal unmapped", in: "::ffff:192.0.2.1", want: []string{"192.0.2.1/32"}},
		{name: "v4-mapped cidr unmapped", in: "::ffff:10.0.0.0/104", want: []string{"10.0.0.0/8"}},
		{name: "v4 zero prefix accepts all", in: "0.0.0.0/0"},
		{name: "v6 zero prefix accepts all", in: "::/0"},
		{name: "zero prefix among others accepts all", in: "10.0.0.0/8,0.0.0.0/0,::1"},
		{name: "not an ip", in: "not-an-ip", wantErr: true},
		{name: "bad v4 mask", in: "127.0.0.1/40", wantErr: true},
		{name: "bad v6 mask", in: "::1/200", wantErr: true},
		{name: "bad entry after zero prefix", in: "0.0.0.0/0,bogus", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAllowedSources(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAllowedSources(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAllowedSources(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseAllowedSources(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i, p := range got {
				if p.String() != tc.want[i] {
					t.Errorf("prefix %d = %s, want %s", i, p, tc.want[i])
				}
			}
		})
	}
}

func TestSourceAllowlist_Allows(t *testing.T) {
	list := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("203.0.113.7/32"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	cases := []struct {
		name    string
		allowed []netip.Prefix
		source  string
		want    bool
	}{
		{name: "empty list accepts anything", source: "198.51.100.1:5060", want: true},
		{name: "empty list accepts unparseable", source: ":5060", want: true},
		{name: "v4 inside cidr", allowed: list, source: "10.1.2.3:5060", want: true},
		{name: "v4 exact host", allowed: list, source: "203.0.113.7:41000", want: true},
		{name: "v4 neighbour of host", allowed: list, source: "203.0.113.8:5060", want: false},
		{name: "v4 outside", allowed: list, source: "198.51.100.1:5060", want: false},
		{name: "v6 inside cidr", allowed: list, source: "[2001:db8::5]:5060", want: true},
		{name: "v6 outside", allowed: list, source: "[2001:db9::5]:5060", want: false},
		{name: "v4-mapped v6 matches v4 entry", allowed: list, source: "[::ffff:10.1.2.3]:5060", want: true},
		{name: "v4-mapped v6 outside", allowed: list, source: "[::ffff:198.51.100.1]:5060", want: false},
		{name: "zoned v6", allowed: []netip.Prefix{netip.MustParsePrefix("fe80::/10")}, source: "[fe80::1%eth0]:5060", want: true},
		{name: "empty source", allowed: list, source: "", want: false},
		{name: "port only", allowed: list, source: ":5060", want: false},
		{name: "hostname", allowed: list, source: "example.com:5060", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &sourceAllowlist{prefixes: tc.allowed}
			if got := a.allows(tc.source); got != tc.want {
				t.Errorf("allows(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

var allowlistTransports = []string{"udp", "tcp", "tls"}

type allowlistEngine struct {
	engine  *Engine
	port    int // UDP and TCP
	tlsPort int
	invites *atomic.Int32
}

func (a *allowlistEngine) portFor(transport string) int {
	if transport == "tls" {
		return a.tlsPort
	}
	return a.port
}

// pickFreeSIPPort returns a port free on both UDP and TCP, which share one.
func pickFreeSIPPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		port := pickFreePort(t, "udp")
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		l.Close()
		return port
	}
	t.Fatal("no port free on both udp and tcp")
	return 0
}

func startAllowlistEngine(t *testing.T, allowed string, mutate ...func(*EngineConfig)) *allowlistEngine {
	t.Helper()
	sources, err := ParseAllowedSources(allowed)
	if err != nil {
		t.Fatalf("ParseAllowedSources(%q): %v", allowed, err)
	}
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	a := &allowlistEngine{
		port:    pickFreeSIPPort(t),
		tlsPort: pickFreePort(t, "tcp"),
		invites: &atomic.Int32{},
	}
	cfg := EngineConfig{
		BindIP:         "127.0.0.1",
		BindPort:       a.port,
		TCPEnabled:     true,
		TLSBindPort:    a.tlsPort,
		TLSCertPath:    certPath,
		TLSKeyPath:     keyPath,
		AllowedSources: sources,
		SIPHost:        "test-vb",
		Codecs:         []codec.CodecType{codec.CodecPCMU},
		Log:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a.engine, err = NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	a.engine.OnInvite(func(call *InboundCall) {
		a.invites.Add(1)
		call.Dialog.Respond(sip.StatusBusyHere, "Busy Here", nil)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.engine.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after ctx cancel")
		}
	})
	return a
}

// dialRaw opens a plain socket to the engine, retrying while the listener is
// still coming up.
func (a *allowlistEngine) dialRaw(t *testing.T, transport string) net.Conn {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", a.portFor(transport))
	deadline := time.Now().Add(3 * time.Second)
	for {
		var (
			conn net.Conn
			err  error
		)
		switch transport {
		case "udp":
			conn, err = net.Dial("udp", addr)
		case "tcp":
			conn, err = net.DialTimeout("tcp", addr, time.Second)
		case "tls":
			conn, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
		}
		if err == nil {
			t.Cleanup(func() { conn.Close() })
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s %s: %v", transport, addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func rawRequest(method, transport string, port int) []byte {
	local := "127.0.0.1"
	lines := []string{
		fmt.Sprintf("%s sip:probe@127.0.0.1:%d SIP/2.0", method, port),
		fmt.Sprintf("Via: SIP/2.0/%s %s:5099;rport;branch=z9hG4bK%s", viaTransport(transport), local, sip.GenerateTagN(16)),
		fmt.Sprintf("From: <sip:scanner@%s>;tag=%s", local, sip.GenerateTagN(8)),
		fmt.Sprintf("To: <sip:probe@127.0.0.1:%d>", port),
		"Call-ID: " + sip.GenerateTagN(16),
		"CSeq: 1 " + method,
		fmt.Sprintf("Contact: <sip:scanner@%s:5099>", local),
		"Max-Forwards: 70",
		"Content-Length: 0",
		"", "",
	}
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\r\n"
		}
		out += l
	}
	return []byte(out)
}

func viaTransport(transport string) string {
	switch transport {
	case "tcp":
		return "TCP"
	case "tls":
		return "TLS"
	}
	return "UDP"
}

// A request from an unlisted source must draw no bytes at all on any
// transport, and must leave a stream connection open.
func TestSourceAllowlist_UnlistedDroppedSilently(t *testing.T) {
	a := startAllowlistEngine(t, "10.0.0.0/8")

	for _, transport := range allowlistTransports {
		t.Run(transport, func(t *testing.T) {
			conn := a.dialRaw(t, transport)
			for _, method := range []string{"OPTIONS", "INVITE", "REGISTER", "BYE", "MESSAGE"} {
				before := a.engine.RequestsRejected()
				// UDP has no handshake, so resend until the listener counts one.
				deadline := time.Now().Add(3 * time.Second)
				for sent := false; a.engine.RequestsRejected() == before; {
					if time.Now().After(deadline) {
						t.Fatalf("%s never reached the allowlist", method)
					}
					if transport == "udp" || !sent {
						conn.Write(rawRequest(method, transport, a.portFor(transport)))
						sent = true
					}
					time.Sleep(20 * time.Millisecond)
				}
			}

			// Long enough for sipgo's automatic 100 Trying (200 ms).
			buf := make([]byte, 2048)
			quiet := time.Now().Add(500 * time.Millisecond)
			for {
				conn.SetReadDeadline(quiet)
				n, err := conn.Read(buf)
				if n > 0 {
					t.Fatalf("got a reply to a dropped request:\n%s", buf[:n])
				}
				var nerr net.Error
				if errors.As(err, &nerr) && nerr.Timeout() {
					break
				}
				if transport != "udp" {
					t.Fatalf("connection closed after a dropped request: %v", err)
				}
				// A UDP socket can still hold an ICMP error from before the
				// listener was up.
			}
		})
	}

	if n := a.invites.Load(); n != 0 {
		t.Errorf("INVITE handler ran %d times for an unlisted source", n)
	}
}

func allowlistClient(t *testing.T, transport string) *sipgo.Client {
	t.Helper()
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("allowlist-test"),
		sipgo.WithUserAgenTLSConfig(&tls.Config{InsecureSkipVerify: true}),
	)
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	t.Cleanup(func() { ua.Close() })
	cli, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cli
}

// doRequest retries until the listener answers; Serve binds asynchronously.
func (a *allowlistEngine) doRequest(t *testing.T, cli *sipgo.Client, transport string, method sip.RequestMethod) *sip.Response {
	t.Helper()
	target := sip.Uri{Scheme: "sip", User: "probe", Host: "127.0.0.1", Port: a.portFor(transport), UriParams: sip.NewParams()}
	target.UriParams.Add("transport", transport)

	var lastErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req := sip.NewRequest(method, target)
		req.SetTransport(viaTransport(transport))
		reqCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		res, err := cli.Do(reqCtx, req)
		cancel()
		if err == nil {
			if via := res.Via(); via == nil || via.Transport != viaTransport(transport) {
				t.Fatalf("%s answered over %v, want %s", method, via, viaTransport(transport))
			}
			return res
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s over %s never answered: %v", method, transport, lastErr)
	return nil
}

// A listed source, the default empty list and a /0 entry all behave as if
// there were no filter, on every transport.
func TestSourceAllowlist_ListedAccepted(t *testing.T) {
	for _, allowed := range []string{"", "0.0.0.0/0", "127.0.0.0/8", "10.0.0.0/8,127.0.0.1"} {
		t.Run("allowed="+allowed, func(t *testing.T) {
			a := startAllowlistEngine(t, allowed)
			for _, transport := range allowlistTransports {
				t.Run(transport, func(t *testing.T) {
					cli := allowlistClient(t, transport)
					if res := a.doRequest(t, cli, transport, sip.OPTIONS); res.StatusCode != sip.StatusOK {
						t.Errorf("OPTIONS = %d, want 200", res.StatusCode)
					}
					res := a.doRequest(t, cli, transport, sip.MESSAGE)
					if res.StatusCode != sip.StatusMethodNotAllowed {
						t.Errorf("MESSAGE = %d, want 405", res.StatusCode)
					}
					if allow := res.GetHeader("Allow"); allow == nil || allow.Value() == "" {
						t.Error("405 carries no Allow header")
					}
				})
			}
			if n := a.engine.RequestsRejected(); n != 0 {
				t.Errorf("RequestsRejected() = %d, want 0", n)
			}
		})
	}
}

// One engine, two real source addresses: the list decides per request, so the
// IPv4 loopback is dropped while the IPv6 one is served.
func TestSourceAllowlist_FiltersPerSource(t *testing.T) {
	if l, err := net.ListenPacket("udp6", "[::1]:0"); err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	} else {
		l.Close()
	}
	a := startAllowlistEngine(t, "::1", func(c *EngineConfig) {
		c.ListenIP = "127.0.0.1"
		c.BindIPV6 = "::1"
		c.ListenIPV6 = "::1"
	})

	v6, err := net.Dial("udp", fmt.Sprintf("[::1]:%d", a.port))
	if err != nil {
		t.Fatalf("dial ::1: %v", err)
	}
	defer v6.Close()
	buf := make([]byte, 2048)
	reply := ""
	for deadline := time.Now().Add(3 * time.Second); reply == ""; {
		if time.Now().After(deadline) {
			t.Fatal("OPTIONS from ::1 never answered")
		}
		v6.Write(rawRequest("OPTIONS", "udp", a.port))
		v6.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if n, _ := v6.Read(buf); n > 0 {
			reply = string(buf[:n])
		}
	}
	if !strings.HasPrefix(reply, "SIP/2.0 200 ") {
		t.Fatalf("OPTIONS from ::1 answered:\n%s", reply)
	}
	if n := a.engine.RequestsRejected(); n != 0 {
		t.Fatalf("RequestsRejected() = %d after a listed request, want 0", n)
	}

	conn := a.dialRaw(t, "udp")
	for deadline := time.Now().Add(3 * time.Second); a.engine.RequestsRejected() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("OPTIONS from 127.0.0.1 never reached the allowlist")
		}
		conn.Write(rawRequest("OPTIONS", "udp", a.port))
		time.Sleep(20 * time.Millisecond)
	}
	for quiet := time.Now().Add(500 * time.Millisecond); ; {
		conn.SetReadDeadline(quiet)
		n, err := conn.Read(buf)
		if n > 0 {
			t.Fatalf("got a reply to a request from 127.0.0.1:\n%s", buf[:n])
		}
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			break
		}
	}
}

func TestSourceAllowlist_FilterUDP(t *testing.T) {
	listed := &net.UDPAddr{IP: net.ParseIP("10.1.2.3"), Port: 5060}
	unlisted := &net.UDPAddr{IP: net.ParseIP("198.51.100.1"), Port: 5060}
	mapped := &net.UDPAddr{IP: net.ParseIP("::ffff:10.1.2.3"), Port: 5060}
	request := []byte("OPTIONS sip:a@b SIP/2.0\r\n\r\n")
	response := []byte("SIP/2.0 200 OK\r\n\r\n")

	cases := []struct {
		name      string
		transport string
		from      net.Addr
		data      []byte
		wantPass  bool
	}{
		{"request from listed", "udp", listed, request, true},
		{"request from listed v4-mapped", "udp", mapped, request, true},
		{"request from unlisted", "udp", unlisted, request, false},
		{"garbage from unlisted", "udp", unlisted, []byte("\x00\x01hello"), false},
		{"keepalive from unlisted", "udp", unlisted, []byte("\r\n\r\n"), false},
		{"response from unlisted", "udp", unlisted, response, true},
		{"response after CRLF from unlisted", "udp", unlisted, append([]byte("\r\n"), response...), true},
		{"request without source", "udp", nil, request, false},
		{"transport name case", "UDP", unlisted, request, false},
		{"tcp chunk from unlisted left to handlers", "tcp", &net.TCPAddr{IP: unlisted.IP, Port: 5060}, request, true},
		{"tls chunk from unlisted left to handlers", "tls", &net.TCPAddr{IP: unlisted.IP, Port: 5060}, request, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &sourceAllowlist{prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
			out, err := a.filterUDP(sip.TransportReadProps{Transport: tc.transport, RemoteAddr: tc.from}, tc.data)
			if err != nil {
				t.Fatalf("filterUDP: %v", err)
			}
			if passed := len(out) > 0; passed != tc.wantPass {
				t.Fatalf("passed = %v, want %v", passed, tc.wantPass)
			}
			wantRejected := uint64(1)
			if tc.wantPass {
				wantRejected = 0
			}
			if got := a.rejected.Load(); got != wantRejected {
				t.Errorf("rejected = %d, want %d", got, wantRejected)
			}
		})
	}
}

// sipgo answers a request it cannot build a transaction from with a stateless
// 400 before any handler runs. Over UDP that would confirm the listener to an
// unlisted source.
func TestSourceAllowlist_MalformedUDPDroppedSilently(t *testing.T) {
	a := startAllowlistEngine(t, "10.0.0.0/8")
	conn := a.dialRaw(t, "udp")

	malformed := func() []byte {
		var lines []string
		for _, l := range strings.Split(string(rawRequest("OPTIONS", "udp", a.port)), "\r\n") {
			if !strings.HasPrefix(l, "CSeq: ") {
				lines = append(lines, l)
			}
		}
		return []byte(strings.Join(lines, "\r\n"))
	}
	for deadline := time.Now().Add(3 * time.Second); a.engine.RequestsRejected() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("request never reached the allowlist")
		}
		conn.Write(malformed())
		time.Sleep(20 * time.Millisecond)
	}
	buf := make([]byte, 2048)
	for quiet := time.Now().Add(500 * time.Millisecond); ; {
		conn.SetReadDeadline(quiet)
		n, err := conn.Read(buf)
		if n > 0 {
			t.Fatalf("got a reply to a malformed request from an unlisted source:\n%s", buf[:n])
		}
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			break
		}
	}
}
