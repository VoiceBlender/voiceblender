package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestParseAllowedIPs(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantLen int
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"whitespace only", "   ", 0, false},
		{"v4 literal", "127.0.0.1", 1, false},
		{"v6 literal", "::1", 1, false},
		{"v4 CIDR", "10.0.0.0/8", 1, false},
		{"v6 CIDR", "2001:db8::/32", 1, false},
		{"mixed list with whitespace", "  127.0.0.1 , 10.0.0.0/8 ,2001:db8::/32 , ::1 ", 4, false},
		{"empty token skipped", "127.0.0.1, , 10.0.0.0/8", 2, false},
		{"malformed entry", "not-an-ip", 0, true},
		{"invalid v4 mask", "127.0.0.1/40", 0, true},
		{"invalid v6 mask", "::1/200", 0, true},
		{"trailing garbage in CIDR", "10.0.0.0/8x", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAllowedIPs(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (parsed=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), tt.wantLen, got)
			}
		})
	}
}

func TestParseAllowedIPs_MaskingNormalizesHostBits(t *testing.T) {
	got, err := ParseAllowedIPs("10.0.0.5/8")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].String() != "10.0.0.0/8" {
		t.Fatalf("got %v, want [10.0.0.0/8]", got)
	}
}

func TestParseAllowedIPs_BareIPGetsHostPrefix(t *testing.T) {
	got, err := ParseAllowedIPs("127.0.0.1, ::1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].Bits() != 32 {
		t.Fatalf("v4 host prefix bits = %d, want 32", got[0].Bits())
	}
	if got[1].Bits() != 128 {
		t.Fatalf("v6 host prefix bits = %d, want 128", got[1].Bits())
	}
}

// nullLogger returns a slog.Logger that discards all output.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// okHandler returns 200 OK and writes a sentinel body so we can verify the
// middleware passed through.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func runMW(t *testing.T, allowedSpec string, trustProxy bool, remoteAddr, xff string) *httptest.ResponseRecorder {
	t.Helper()
	allowed, err := ParseAllowedIPs(allowedSpec)
	if err != nil {
		t.Fatalf("parse %q: %v", allowedSpec, err)
	}
	mw := ipAllowlistMiddleware(allowed, trustProxy, nullLogger())
	h := mw(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestIPAllowlistMiddleware(t *testing.T) {
	tests := []struct {
		name        string
		allowedSpec string
		trustProxy  bool
		remoteAddr  string
		xff         string
		wantStatus  int
	}{
		{"empty list allows v4", "", false, "1.2.3.4:1234", "", http.StatusOK},
		{"empty list allows v6", "", false, "[2001:db8::1]:1234", "", http.StatusOK},
		{"single v4 literal allow", "127.0.0.1", false, "127.0.0.1:1234", "", http.StatusOK},
		{"single v4 literal deny", "127.0.0.1", false, "10.0.0.1:1234", "", http.StatusForbidden},
		{"single v6 literal allow", "::1", false, "[::1]:1234", "", http.StatusOK},
		{"single v6 literal deny", "::1", false, "[2001:db8::1]:1234", "", http.StatusForbidden},
		{"v4 CIDR match", "10.0.0.0/8", false, "10.5.5.5:1234", "", http.StatusOK},
		{"v4 CIDR miss", "10.0.0.0/8", false, "192.168.1.1:1234", "", http.StatusForbidden},
		{"v6 CIDR match", "2001:db8::/32", false, "[2001:db8::abcd]:1234", "", http.StatusOK},
		{"v6 CIDR miss", "2001:db8::/32", false, "[fe80::1]:1234", "", http.StatusForbidden},
		{"mixed list v4 hit", "10.0.0.0/8,2001:db8::/32", false, "10.1.2.3:1234", "", http.StatusOK},
		{"mixed list v6 hit", "10.0.0.0/8,2001:db8::/32", false, "[2001:db8::1]:1234", "", http.StatusOK},
		{"mixed list miss", "10.0.0.0/8,2001:db8::/32", false, "192.168.1.1:1234", "", http.StatusForbidden},
		{"trust-proxy off ignores XFF", "1.2.3.4", false, "127.0.0.1:1234", "1.2.3.4", http.StatusForbidden},
		{"trust-proxy on uses leftmost XFF", "1.2.3.4", true, "127.0.0.1:1234", "1.2.3.4, 9.9.9.9", http.StatusOK},
		{"trust-proxy on falls back when XFF empty", "127.0.0.1", true, "127.0.0.1:1234", "", http.StatusOK},
		{"trust-proxy on malformed XFF", "1.2.3.4", true, "127.0.0.1:1234", "not-an-ip", http.StatusForbidden},
		{"v4-mapped-in-v6 matches v4 entry", "127.0.0.1", false, "[::ffff:127.0.0.1]:1234", "", http.StatusOK},
		{"malformed RemoteAddr", "127.0.0.1", false, "garbage", "", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := runMW(t, tt.allowedSpec, tt.trustProxy, tt.remoteAddr, tt.xff)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, tt.wantStatus, strings.TrimSpace(rr.Body.String()))
			}
		})
	}
}

type identityHandler struct{ tag string }

func (identityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {}

func TestIPAllowlistMiddleware_EmptyListReturnsSameHandler(t *testing.T) {
	mw := ipAllowlistMiddleware(nil, false, nullLogger())
	h := identityHandler{tag: "inner"}
	if got, ok := mw(h).(identityHandler); !ok || got != h {
		t.Fatalf("expected no-op wrapper to return the inner handler unchanged; got %T %v", mw(h), mw(h))
	}
}

func TestClientIP_TrustProxyParsesV6XFF(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:1"
	req.Header.Set("X-Forwarded-For", "2001:db8::1")
	addr, ok := clientIP(req, true)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if addr != netip.MustParseAddr("2001:db8::1") {
		t.Fatalf("addr = %v, want 2001:db8::1", addr)
	}
}
