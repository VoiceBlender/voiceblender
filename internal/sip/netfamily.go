package sip

import (
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
)

// UDPNetwork returns the UDP network string ("udp", "udp4", or "udp6") to use
// for a given listen IP literal. An empty string or "::" yields "udp" so the
// OS can give us a dual-stack socket on Linux when bindv6only=0.
func UDPNetwork(listenIP string) string {
	switch listenIP {
	case "", "::":
		return "udp"
	case "0.0.0.0":
		return "udp4"
	}
	ip := net.ParseIP(listenIP)
	if ip == nil {
		return "udp"
	}
	if ip.To4() != nil {
		return "udp4"
	}
	return "udp6"
}

// AddressFamily returns the SDP address-type token ("IP4" or "IP6") for an IP
// literal, or "" when the input is empty / not a literal IP.
func AddressFamily(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.To4() != nil {
		return "IP4"
	}
	return "IP6"
}

// JoinHostPort wraps net.JoinHostPort with an int port — bracket-safe for IPv6
// literals and a no-op for IPv4 / hostnames.
func JoinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// warnIfBindV6OnlyConflict logs a warning when a "::" wildcard listen address
// is used on a Linux host with /proc/sys/net/ipv6/bindv6only=1. Under that
// sysctl an IPv6 socket will not accept IPv4 traffic, so what looks like a
// dual-stack bind silently drops half of the traffic.
func warnIfBindV6OnlyConflict(log *slog.Logger, listenIP, listenIPV6 string) {
	if listenIP != "::" && listenIPV6 != "::" {
		return
	}
	data, err := os.ReadFile("/proc/sys/net/ipv6/bindv6only")
	if err != nil {
		return
	}
	if strings.TrimSpace(string(data)) != "1" {
		return
	}
	log.Warn("/proc/sys/net/ipv6/bindv6only=1: a `::` listen socket will not accept IPv4 traffic. Bind to 0.0.0.0 (and a separate IPv6 address) for dual-stack on this host.")
}
