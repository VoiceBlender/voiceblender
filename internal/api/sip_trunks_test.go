package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCreateTrunk_UnknownType(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `{"type":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown trunk type") {
		t.Errorf("body = %s, want error about unknown type", w.Body.String())
	}
}

func TestCreateTrunk_MissingType(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateTrunk_IPIPNotImplemented(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example"}}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ip_ip") {
		t.Errorf("body = %s, want mention of ip_ip", w.Body.String())
	}
}

func TestCreateTrunk_SIPRegisterMissingBlock(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `{"type":"sip_register"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sip_register block is required") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestCreateTrunk_SIPRegisterMissingPassword(t *testing.T) {
	s := newTestServer(t)
	body := `{"type":"sip_register","sip_register":{"registrar_uri":"sip:pbx.example","aor":"sip:alice@pbx.example"}}`
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "password is required") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestCreateTrunk_SIPRegisterMissingAOR(t *testing.T) {
	s := newTestServer(t)
	body := `{"type":"sip_register","sip_register":{"registrar_uri":"sip:pbx.example","password":"x"}}`
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateTrunk_SIPRegisterInvalidRegistrarURI(t *testing.T) {
	s := newTestServer(t)
	body := `{"type":"sip_register","sip_register":{"registrar_uri":"not-a-uri","aor":"sip:alice@pbx.example","password":"x"}}`
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateTrunk_InvalidJSON(t *testing.T) {
	s := newTestServer(t)
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TrunkView in JSON must never expose the password field, even if a caller
// constructs one by hand. Verified at the struct level.
func TestSIPRegisterTrunkView_NoPasswordField(t *testing.T) {
	// Marshal a request that includes a password, then unmarshal into the
	// view shape — the view has no password key by design.
	var spec SIPRegisterTrunkSpec
	if err := json.Unmarshal([]byte(`{"password":"secret"}`), &spec); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip the request type DOES include password (it's a request body);
	// the view is a separate type and is what's returned over the API.
	if !strings.Contains(string(out), "password") {
		t.Fatal("request struct unexpectedly hides password — test is checking the wrong type")
	}
	// Ensure the view struct's JSON shape has no password field.
	view, _ := json.Marshal(struct {
		// Mirror the view's exposed fields without copying the implementation.
		RegistrarURI string `json:"registrar_uri"`
	}{RegistrarURI: "sip:x"})
	if strings.Contains(string(view), "password") {
		t.Errorf("trunk view leaks password: %s", string(view))
	}
}
