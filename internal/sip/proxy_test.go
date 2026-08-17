package sip

import (
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestParseProxyURI(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantErr   bool
		wantHost  string
		wantPort  int
		wantUser  string
		wantParam string
	}{
		{name: "plain", raw: "sip:proxy.example.com", wantHost: "proxy.example.com"},
		{name: "port", raw: "sip:proxy.example.com:5080", wantHost: "proxy.example.com", wantPort: 5080},
		{name: "transport param preserved", raw: "sip:p.example:5080;transport=tcp", wantHost: "p.example", wantPort: 5080, wantParam: "tcp"},
		{name: "sips", raw: "sips:p.example:5061", wantHost: "p.example", wantPort: 5061},
		{name: "user stripped", raw: "sip:bob@p.example", wantHost: "p.example", wantUser: ""},
		{name: "whitespace trimmed", raw: "  sip:p.example  ", wantHost: "p.example"},
		{name: "empty", raw: "", wantErr: true},
		{name: "blank", raw: "   ", wantErr: true},
		{name: "no scheme", raw: "proxy.example.com", wantErr: true},
		{name: "wrong scheme", raw: "http://proxy.example.com", wantErr: true},
		{name: "no host", raw: "sip:", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := ParseProxyURI(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseProxyURI(%q) = %v, want error", tt.raw, u)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseProxyURI(%q): %v", tt.raw, err)
			}
			if u.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", u.Host, tt.wantHost)
			}
			if u.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", u.Port, tt.wantPort)
			}
			if u.User != tt.wantUser {
				t.Errorf("user = %q, want %q", u.User, tt.wantUser)
			}
			if tt.wantParam != "" {
				got, _ := u.UriParams.Get("transport")
				if got != tt.wantParam {
					t.Errorf("transport param = %q, want %q", got, tt.wantParam)
				}
			}
			// The parser must not inject ";lr" — API snapshots echo this value
			// back exactly as the operator configured it.
			if _, ok := u.UriParams.Get("lr"); ok {
				t.Error("ParseProxyURI injected an lr param; that belongs to looseRouteHeader")
			}
		})
	}
}

func TestTransportForURI(t *testing.T) {
	mustParse := func(raw string) sip.Uri {
		t.Helper()
		u, err := ParseProxyURI(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return u
	}
	tests := []struct {
		raw  string
		want string
	}{
		{"sip:p.example", ""},
		{"sip:p.example;transport=tcp", "tcp"},
		{"sip:p.example;transport=TLS", "tls"},
		{"sips:p.example", "tls"},
		// An explicit param beats the scheme.
		{"sips:p.example;transport=tcp", "tcp"},
	}
	for _, tt := range tests {
		if got := TransportForURI(mustParse(tt.raw)); got != tt.want {
			t.Errorf("TransportForURI(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestLooseRouteHeader_AddsLR(t *testing.T) {
	u, _ := ParseProxyURI("sip:p.example:5060")
	h := looseRouteHeader(u)
	if h.Name() != "Route" {
		t.Errorf("header name = %q, want Route", h.Name())
	}
	if want := "<sip:p.example:5060;lr>"; h.Value() != want {
		t.Errorf("header = %q, want %q", h.Value(), want)
	}
}

func TestLooseRouteHeader_DoesNotDoubleLR(t *testing.T) {
	u, _ := ParseProxyURI("sip:p.example;lr")
	h := looseRouteHeader(u)
	if n := strings.Count(h.Value(), "lr"); n != 1 {
		t.Errorf("header %q has %d lr params, want 1", h.Value(), n)
	}
}

// TestLooseRouteHeader_DoesNotMutateInput guards the aliasing trap: sip.Uri
// copies by value but UriParams is a slice, and HeaderParams.Add has a pointer
// receiver that appends into the shared backing array. Without a clone, adding
// ";lr" would write into a URI the caller still owns.
func TestLooseRouteHeader_DoesNotMutateInput(t *testing.T) {
	u := sip.Uri{Scheme: "sip", Host: "p.example", UriParams: sip.NewParams()}
	first := looseRouteHeader(u)
	if _, ok := u.UriParams.Get("lr"); ok {
		t.Fatal("looseRouteHeader mutated the caller's UriParams")
	}
	second := looseRouteHeader(u)
	if first.Value() != second.Value() {
		t.Errorf("repeated calls diverged: %q then %q", first.Value(), second.Value())
	}
}

// TestLooseRouteHeader_DrivesDestination pins the behaviour the whole design
// rests on: sipgo resolves an untyped Route header as the next hop, and the
// ";lr" keeps it loose routing so the Request-URI survives.
func TestLooseRouteHeader_DrivesDestination(t *testing.T) {
	recipient := sip.Uri{Scheme: "sip", User: "bob", Host: "callee.example", Port: 5060}
	req := sip.NewRequest(sip.INVITE, recipient)
	proxy, _ := ParseProxyURI("sip:127.0.0.1:5080")
	req.AppendHeader(looseRouteHeader(proxy))

	route := req.Route()
	if route == nil {
		t.Fatal("req.Route() = nil; sipgo did not parse the untyped Route header")
	}
	if !route.Address.UriParams.Has("lr") {
		t.Error("Route lacks lr; sipgo would strict-route and rewrite the Request-URI")
	}
	if got, want := req.Destination(), "127.0.0.1:5080"; got != want {
		t.Errorf("Destination() = %q, want %q", got, want)
	}
	if got, want := req.Recipient.String(), recipient.String(); got != want {
		t.Errorf("Request-URI = %q, want it unchanged at %q", got, want)
	}
}

// TestRouteTransportDerivation records why the proxy branch calls SetTransport:
// sipgo already honours a Route's ";transport=" param, but nothing maps the
// "sips:" scheme onto TLS.
func TestRouteTransportDerivation(t *testing.T) {
	newReqWithRoute := func(raw string) *sip.Request {
		req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "bob", Host: "callee.example"})
		u, err := ParseProxyURI(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		req.AppendHeader(looseRouteHeader(u))
		return req
	}

	if got := newReqWithRoute("sip:p.example;transport=tcp").Transport(); got != "TCP" {
		t.Errorf("transport param Route: Transport() = %q, want TCP", got)
	}
	if got := newReqWithRoute("sips:p.example").Transport(); got == "TLS" {
		t.Error("sips: Route resolved to TLS on its own; the SetTransport call in Invite would be redundant")
	}
}

func TestDefaultPortForURI(t *testing.T) {
	sipURI, _ := ParseProxyURI("sip:p.example")
	sipsURI, _ := ParseProxyURI("sips:p.example")
	if got := defaultPortForURI(sipURI); got != 5060 {
		t.Errorf("sip default port = %d, want 5060", got)
	}
	if got := defaultPortForURI(sipsURI); got != 5061 {
		t.Errorf("sips default port = %d, want 5061", got)
	}
}
