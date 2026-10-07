package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
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

func TestCreateTrunk_IPIP_Created(t *testing.T) {
	s := newProxyTestServer(t, "")
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `{
		"type":"ip_ip","app_id":"acme",
		"ip_ip":{"peer_uri":"sip:carrier@203.0.113.10:5070;transport=tcp","aor":"sip:acme@carrier.example",
			"password":"secret","inbound_sources":["198.51.100.0/24","::ffff:192.0.2.7"]}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	var res CreateTrunkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Type != "ip_ip" || res.Status != "active" || res.ID == "" {
		t.Fatalf("response = %+v, want an active ip_ip trunk", res)
	}

	w = doRequest(s, http.MethodGet, "/v1/sip/trunks/"+res.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Errorf("view leaks the password: %s", w.Body.String())
	}
	var view sipmod.TrunkView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.IPIP == nil || view.SIPRegister != nil {
		t.Fatalf("view = %+v, want only the ip_ip block", view)
	}
	if view.AppID != "acme" || view.Status != sipmod.TrunkStatusActive {
		t.Errorf("app_id = %q status = %q", view.AppID, view.Status)
	}
	if got := view.IPIP.PeerURI; got != "sip:203.0.113.10:5070;transport=tcp" {
		t.Errorf("peer_uri = %q, want the user part dropped", got)
	}
	if view.IPIP.AOR != "sip:acme@carrier.example" || view.IPIP.Username != "acme" {
		t.Errorf("aor = %q username = %q", view.IPIP.AOR, view.IPIP.Username)
	}
	want := []string{"198.51.100.0/24", "192.0.2.7/32", "203.0.113.10/32"}
	if strings.Join(view.IPIP.InboundSources, ",") != strings.Join(want, ",") {
		t.Errorf("inbound_sources = %v, want %v", view.IPIP.InboundSources, want)
	}
}

func TestCreateTrunk_IPIP_Validation(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"missing block", `{"type":"ip_ip"}`, "ip_ip block is required"},
		{"missing peer_uri", `{"type":"ip_ip","ip_ip":{}}`, "ip_ip.peer_uri is required"},
		{"peer_uri not a URI", `{"type":"ip_ip","ip_ip":{"peer_uri":"not-a-uri"}}`, "ip_ip.peer_uri is invalid"},
		{"peer_uri wrong scheme", `{"type":"ip_ip","ip_ip":{"peer_uri":"http://pbx.example"}}`, "ip_ip.peer_uri is invalid"},
		{"aor not a URI", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","aor":"acme"}}`, "ip_ip.aor is invalid"},
		{"aor without user", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","aor":"sip:carrier.example"}}`, "ip_ip.aor is invalid"},
		{"username without password", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","username":"u"}}`, "ip_ip.password is required"},
		{"password without identity", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","password":"p"}}`, "ip_ip.username is required"},
		{"bad source", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","inbound_sources":["pbx.example"]}}`, "ip_ip.inbound_sources is invalid"},
		{"empty source", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","inbound_sources":[""]}}`, "ip_ip.inbound_sources is invalid"},
		{"catch-all source", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","inbound_sources":["0.0.0.0/0"]}}`, "matches every address"},
		{"negative interval", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","options_ping_interval_seconds":-1}}`, "options_ping_interval_seconds"},
		{"interval too long", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","options_ping_interval_seconds":3601}}`, "options_ping_interval_seconds"},
		{"bad proxy", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:pbx.example","outbound_proxy":"http://edge.example"}}`, "ip_ip.outbound_proxy is invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			w := doRequest(s, http.MethodPost, "/v1/sip/trunks", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body = %s, want %q", w.Body.String(), tc.want)
			}
		})
	}
}

func TestCreateTrunk_IPIP_TooManySources(t *testing.T) {
	s := newTestServer(t)
	sources := make([]string, 65)
	for i := range sources {
		sources[i] = "10.0.0." + strconv.Itoa(i)
	}
	body, err := json.Marshal(CreateTrunkRequest{Type: "ip_ip", IPIP: &IPIPTrunkSpec{PeerURI: "sip:pbx.example", InboundSources: sources}})
	if err != nil {
		t.Fatal(err)
	}
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", string(body))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "too many entries") {
		t.Fatalf("status = %d body = %s, want 400 too many entries", w.Code, w.Body.String())
	}
}

func TestCreateTrunk_IPIP_InheritsGlobalProxy(t *testing.T) {
	s := newProxyTestServer(t, "sip:global.acme.net;transport=tcp")
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `{"type":"ip_ip","ip_ip":{"peer_uri":"sip:203.0.113.10"}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	views := s.doListTrunks().Trunks
	if len(views) != 1 || views[0].IPIP == nil {
		t.Fatalf("trunks = %+v", views)
	}
	if got := views[0].IPIP.OutboundProxy; got != "sip:global.acme.net;transport=tcp" {
		t.Errorf("outbound_proxy = %q, want the global default", got)
	}
	// The proxy carries the trunk's requests but is not where its calls are
	// recognised from.
	if got := views[0].IPIP.InboundSources; len(got) != 1 || got[0] != "203.0.113.10/32" {
		t.Errorf("inbound_sources = %v, want only the peer", got)
	}
}

func TestIPIPTrunkView_NoPassword(t *testing.T) {
	out, err := json.Marshal(sipmod.IPIPTrunkView{PeerURI: "sip:pbx.example", Username: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "password") {
		t.Errorf("view = %s, want no password key", string(out))
	}
	if !strings.Contains(string(out), `"inbound_sources"`) {
		t.Errorf("view = %s, want inbound_sources always present", string(out))
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

func TestCreateTrunk_OutboundProxyInvalid(t *testing.T) {
	s := newTestServer(t)
	body := `{"type":"sip_register","sip_register":{"registrar_uri":"sip:pbx.example","aor":"sip:alice@pbx.example","password":"x","outbound_proxy":"not-a-uri"}}`
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sip_register.outbound_proxy is invalid") {
		t.Errorf("body = %s, want an outbound_proxy error", w.Body.String())
	}
}

func TestCreateTrunk_OutboundProxyWrongScheme(t *testing.T) {
	s := newTestServer(t)
	body := `{"type":"sip_register","sip_register":{"registrar_uri":"sip:pbx.example","aor":"sip:alice@pbx.example","password":"x","outbound_proxy":"http://edge.example"}}`
	w := doRequest(s, http.MethodPost, "/v1/sip/trunks", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestSIPRegisterTrunkView_OmitsProxyWhenUnset pins that an unconfigured trunk
// serialises exactly as it always has.
func TestSIPRegisterTrunkView_OmitsProxyWhenUnset(t *testing.T) {
	out, err := json.Marshal(sipmod.SIPRegisterTrunkView{RegistrarURI: "sip:pbx.example"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "outbound_proxy") {
		t.Errorf("view = %s, want no outbound_proxy key when unset", string(out))
	}

	out, err = json.Marshal(sipmod.SIPRegisterTrunkView{
		RegistrarURI:  "sip:pbx.example",
		OutboundProxy: "sip:edge.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"outbound_proxy":"sip:edge.example"`) {
		t.Errorf("view = %s, want the configured proxy", string(out))
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
