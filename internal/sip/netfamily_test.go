package sip

import "testing"

func TestUDPNetwork(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "udp"},
		{"::", "udp"},
		{"0.0.0.0", "udp4"},
		{"127.0.0.1", "udp4"},
		{"203.0.113.50", "udp4"},
		{"::1", "udp6"},
		{"2001:db8::1", "udp6"},
		{"::ffff:127.0.0.1", "udp4"}, // IPv4-mapped IPv6 is treated as v4
		{"example.com", "udp"},       // hostname falls back to dual-stack
	}
	for _, c := range cases {
		if got := UDPNetwork(c.in); got != c.want {
			t.Errorf("UDPNetwork(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAddressFamily(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"127.0.0.1", "IP4"},
		{"0.0.0.0", "IP4"},
		{"203.0.113.50", "IP4"},
		{"::1", "IP6"},
		{"::", "IP6"},
		{"2001:db8::1", "IP6"},
		{"::ffff:127.0.0.1", "IP4"},
		{"", ""},
		{"example.com", ""},
		{"not-an-ip", ""},
	}
	for _, c := range cases {
		if got := AddressFamily(c.in); got != c.want {
			t.Errorf("AddressFamily(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJoinHostPort(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"127.0.0.1", 5060, "127.0.0.1:5060"},
		{"::1", 5060, "[::1]:5060"},
		{"2001:db8::1", 5060, "[2001:db8::1]:5060"},
		{"example.com", 5060, "example.com:5060"},
	}
	for _, c := range cases {
		if got := JoinHostPort(c.host, c.port); got != c.want {
			t.Errorf("JoinHostPort(%q,%d) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}
