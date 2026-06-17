package api

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseAllowedIPs parses a comma-separated list of IPs and CIDR ranges into
// netip.Prefix values. Bare addresses become host-prefix entries (/32 for v4,
// /128 for v6). Whitespace around tokens is trimmed; empty tokens are skipped.
// Returns an error on the first malformed entry. An empty (or whitespace-only)
// input returns (nil, nil), which the middleware treats as "no allowlist".
func ParseAllowedIPs(s string) ([]netip.Prefix, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "/") {
			p, err := netip.ParsePrefix(tok)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", tok, err)
			}
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(tok)
		if err != nil {
			return nil, fmt.Errorf("invalid IP %q: %w", tok, err)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// ipAllowlistMiddleware returns a chi-compatible middleware that rejects
// requests whose client IP is not contained in any of the configured
// prefixes. When allowed is empty the middleware is a no-op. When trustProxy
// is true the leftmost X-Forwarded-For entry is used as the client IP (with
// r.RemoteAddr as fallback when the header is absent); a present-but-
// malformed header yields a 403 rather than silently falling back.
func ipAllowlistMiddleware(allowed []netip.Prefix, trustProxy bool, log *slog.Logger) func(http.Handler) http.Handler {
	if len(allowed) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addr, ok := clientIP(r, trustProxy)
			if !ok {
				log.Warn("ip allowlist: cannot determine client IP",
					"remote_addr", r.RemoteAddr,
					"path", r.URL.Path,
					"method", r.Method,
				)
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			addr = addr.Unmap()
			for _, p := range allowed {
				if p.Contains(addr) {
					next.ServeHTTP(w, r)
					return
				}
			}
			log.Warn("ip allowlist: rejected request",
				"ip", addr.String(),
				"path", r.URL.Path,
				"method", r.Method,
			)
			writeError(w, http.StatusForbidden, "forbidden")
		})
	}
}

func clientIP(r *http.Request, trustProxy bool) (netip.Addr, bool) {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			xff = strings.TrimSpace(xff)
			addr, err := netip.ParseAddr(xff)
			if err != nil {
				return netip.Addr{}, false
			}
			return addr, true
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}
