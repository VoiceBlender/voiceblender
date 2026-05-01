package sip

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bindv6OnlyDualStackSkip skips the test when the host has
// /proc/sys/net/ipv6/bindv6only=1, which forces v6 sockets to reject v4
// traffic. The dual-stack test below only makes sense when bindv6only=0.
func bindv6OnlyDualStackSkip(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/net/ipv6/bindv6only")
	if err != nil {
		// Not Linux or no procfs — skip; we only assert dual-stack on Linux.
		t.Skip("cannot read /proc/sys/net/ipv6/bindv6only:", err)
	}
	if strings.TrimSpace(string(data)) == "1" {
		t.Skip("bindv6only=1 — dual-stack not available on this host")
	}
}

func TestNewRTPSession_DualStackBind(t *testing.T) {
	bindv6OnlyDualStackSkip(t)

	sess, err := NewRTPSession()
	if err != nil {
		t.Fatalf("NewRTPSession: %v", err)
	}
	defer sess.Close()

	port := sess.LocalPort()
	if port == 0 {
		t.Fatal("expected non-zero local port")
	}

	// Send from IPv4 loopback.
	if err := sendUDPProbe(t, "127.0.0.1", port, []byte("v4probe")); err != nil {
		t.Fatalf("v4 probe: %v", err)
	}
	// Send from IPv6 loopback.
	if err := sendUDPProbe(t, "::1", port, []byte("v6probe")); err != nil {
		t.Fatalf("v6 probe: %v", err)
	}

	// Read and confirm both arrived.
	got := map[string]bool{}
	_ = sess.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	for len(got) < 2 {
		n, _, err := sess.conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		got[string(buf[:n])] = true
	}
	if !got["v4probe"] {
		t.Errorf("did not receive IPv4 probe (got %v)", got)
	}
	if !got["v6probe"] {
		t.Errorf("did not receive IPv6 probe (got %v)", got)
	}
}

func sendUDPProbe(t *testing.T, host string, port int, data []byte) error {
	t.Helper()
	conn, err := net.Dial("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(data)
	return err
}

func TestSetRemote_IPv6(t *testing.T) {
	sess, err := NewRTPSession()
	if err != nil {
		t.Fatalf("NewRTPSession: %v", err)
	}
	defer sess.Close()

	if err := sess.SetRemote("::1", 5004); err != nil {
		t.Fatalf("SetRemote v6: %v", err)
	}
	addr := sess.getRemote()
	if addr == nil || addr.IP.To16() == nil || addr.IP.To4() != nil {
		t.Errorf("expected IPv6 remote, got %v", addr)
	}
	if addr.Port != 5004 {
		t.Errorf("port = %d, want 5004", addr.Port)
	}
}

func TestSetRemote_IPv4(t *testing.T) {
	sess, err := NewRTPSession()
	if err != nil {
		t.Fatalf("NewRTPSession: %v", err)
	}
	defer sess.Close()

	if err := sess.SetRemote("127.0.0.1", 5004); err != nil {
		t.Fatalf("SetRemote v4: %v", err)
	}
	addr := sess.getRemote()
	if addr == nil || addr.IP.To4() == nil {
		t.Errorf("expected IPv4 remote, got %v", addr)
	}
	if addr.Port != 5004 {
		t.Errorf("port = %d, want 5004", addr.Port)
	}
}
