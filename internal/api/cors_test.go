package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSPreflightAllowsEveryRouteMethod(t *testing.T) {
	h := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("preflight must not reach the handler")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/v1/legs/abc/role", nil)
	req.Header.Set("Origin", "https://example.org")
	req.Header.Set("Access-Control-Request-Method", http.MethodPatch)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	allowed := map[string]bool{}
	for _, m := range strings.Split(rec.Header().Get("Access-Control-Allow-Methods"), ",") {
		allowed[strings.TrimSpace(m)] = true
	}
	for _, rm := range RoutesMetadata() {
		// Extended CONNECT (WebTransport) is not issued through fetch, so CORS never applies.
		if rm.Method == http.MethodConnect {
			continue
		}
		if !allowed[rm.Method] {
			t.Errorf("%s %s: method missing from Access-Control-Allow-Methods %q",
				rm.Method, rm.Path, rec.Header().Get("Access-Control-Allow-Methods"))
		}
	}
}
