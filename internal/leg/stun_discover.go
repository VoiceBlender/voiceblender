package leg

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	stun "github.com/pion/stun/v3"
)

const stunDiscoverTimeout = 2 * time.Second

// DiscoverPublicIPs queries each STUN server in turn and returns the first
// IPv4 and IPv6 reflexive addresses learned. The result is suitable for
// PCMediaConfig.ExternalIPs (and pion's SetNAT1To1IPs). Returns nil when
// no STUN URL is reachable.
//
// Each lookup is bounded by stunDiscoverTimeout. Failures are logged at
// warn level and the next server is tried; if every server fails the
// caller should fall back to gathering host candidates as-is.
func DiscoverPublicIPs(stunURLs []string, log *slog.Logger) []string {
	if log == nil {
		log = slog.Default()
	}
	var v4, v6 string
	for _, raw := range stunURLs {
		host := parseSTUNHostPort(raw)
		if host == "" {
			continue
		}
		if v4 == "" {
			if ip, err := stunQuery("udp4", host); err != nil {
				log.Warn("webrtc external IP discovery: STUN v4 failed", "stun", host, "error", err)
			} else {
				v4 = ip
				log.Info("webrtc external IP discovered", "family", "v4", "ip", v4, "stun", host)
			}
		}
		if v6 == "" {
			if ip, err := stunQuery("udp6", host); err == nil {
				v6 = ip
				log.Info("webrtc external IP discovered", "family", "v6", "ip", v6, "stun", host)
			}
		}
		if v4 != "" && v6 != "" {
			break
		}
	}
	var out []string
	if v4 != "" {
		out = append(out, v4)
	}
	if v6 != "" {
		out = append(out, v6)
	}
	return out
}

// parseSTUNHostPort accepts "stun:host:port", "stun:host", "host:port", or
// a bare "host" and returns "host:port" with the default STUN port 3478
// applied when absent. Returns "" for unparseable input or non-stun schemes.
func parseSTUNHostPort(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "stun" && u.Scheme != "stuns") {
			return ""
		}
		raw = u.Opaque
		if raw == "" {
			raw = u.Host
		}
	} else if strings.HasPrefix(raw, "stun:") || strings.HasPrefix(raw, "stuns:") {
		raw = raw[strings.Index(raw, ":")+1:]
	}
	if _, _, err := net.SplitHostPort(raw); err == nil {
		return raw
	}
	return net.JoinHostPort(raw, "3478")
}

func stunQuery(network, addr string) (string, error) {
	dialer := net.Dialer{Timeout: stunDiscoverTimeout}
	conn, err := dialer.Dial(network, addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(stunDiscoverTimeout))

	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	if _, err := conn.Write(msg.Raw); err != nil {
		return "", fmt.Errorf("stun write: %w", err)
	}

	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("stun read: %w", err)
	}
	resp := &stun.Message{Raw: buf[:n]}
	if err := resp.Decode(); err != nil {
		return "", fmt.Errorf("stun decode: %w", err)
	}
	var xor stun.XORMappedAddress
	if err := xor.GetFrom(resp); err != nil {
		return "", fmt.Errorf("stun xor-mapped-address: %w", err)
	}
	if xor.IP == nil {
		return "", fmt.Errorf("stun: empty mapped address")
	}
	return xor.IP.String(), nil
}
