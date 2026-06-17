//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/gobwas/ws"
)

// allowlistGet issues a simple GET against the test instance and returns the
// status code. Body is drained and discarded so the test cleanup doesn't
// leak connections.
func allowlistGet(t *testing.T, baseURL, path string, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestIPAllowlist_DefaultAllowsLoopback(t *testing.T) {
	inst := newTestInstance(t, "allowlist-default")
	if got := allowlistGet(t, inst.baseURL(), "/v1/legs", nil); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
}

func TestIPAllowlist_LoopbackAllowed(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "allowlist-allow", func(c *config.Config) {
		c.AllowedIPs = "127.0.0.1"
	})
	if got := allowlistGet(t, inst.baseURL(), "/v1/legs", nil); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
}

func TestIPAllowlist_LoopbackRejected(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "allowlist-deny", func(c *config.Config) {
		c.AllowedIPs = "10.0.0.0/8"
	})
	if got := allowlistGet(t, inst.baseURL(), "/v1/legs", nil); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}
}

func TestIPAllowlist_MixedListWithRanges(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "allowlist-mixed", func(c *config.Config) {
		c.AllowedIPs = "10.0.0.0/8, 127.0.0.1, 2001:db8::/32"
	})
	if got := allowlistGet(t, inst.baseURL(), "/v1/legs", nil); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (loopback should match the literal entry)", got)
	}
}

func TestIPAllowlist_VSIWebSocketRejected(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "allowlist-vsi", func(c *config.Config) {
		c.AllowedIPs = "10.0.0.0/8"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := fmt.Sprintf("ws://%s/v1/vsi", inst.httpAddr)
	conn, _, _, err := ws.Dial(ctx, url)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("expected VSI dial to fail with allowlist denying loopback")
	}
}

func TestIPAllowlist_TrustProxyHeadersUsesXFF(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "allowlist-xff", func(c *config.Config) {
		c.AllowedIPs = "203.0.113.5"
		c.TrustProxyHeaders = true
	})
	got := allowlistGet(t, inst.baseURL(), "/v1/legs", map[string]string{
		"X-Forwarded-For": "203.0.113.5, 10.0.0.1",
	})
	if got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (leftmost XFF should be allowed)", got)
	}
}

func TestIPAllowlist_XFFIgnoredWhenTrustProxyOff(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "allowlist-no-xff", func(c *config.Config) {
		c.AllowedIPs = "203.0.113.5"
	})
	got := allowlistGet(t, inst.baseURL(), "/v1/legs", map[string]string{
		"X-Forwarded-For": "203.0.113.5",
	})
	if got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (XFF must be ignored when TRUST_PROXY_HEADERS is off)", got)
	}
}

// Sanity check: the 403 body still carries the standard error envelope.
func TestIPAllowlist_RejectedBody(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "allowlist-body", func(c *config.Config) {
		c.AllowedIPs = "10.0.0.0/8"
	})
	resp, err := http.Get(inst.baseURL() + "/v1/legs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, `"error":"forbidden"`) {
		t.Fatalf("body = %q, expected error=forbidden", body)
	}
}
