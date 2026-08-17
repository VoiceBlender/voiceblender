package sip

import (
	"errors"
	"fmt"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// ParseProxyURI parses an outbound-proxy setting into the URI used as a loose
// Route target. The returned URI is left without ";lr" so callers can echo it
// back over the API exactly as configured; looseRouteHeader adds the param at
// send time.
func ParseProxyURI(raw string) (sip.Uri, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sip.Uri{}, errors.New("outbound proxy is empty")
	}
	var u sip.Uri
	if err := sip.ParseUri(raw, &u); err != nil {
		return sip.Uri{}, err
	}
	switch strings.ToLower(u.Scheme) {
	case "sip", "sips":
	default:
		return sip.Uri{}, fmt.Errorf("outbound proxy must be a sip: or sips: URI, got %q", u.Scheme)
	}
	if u.Host == "" {
		return sip.Uri{}, errors.New("outbound proxy has no host")
	}
	// A Route hop addresses a proxy, not a user at that proxy.
	u.User = ""
	return u, nil
}

// TransportForURI returns the transport a SIP URI implies: its ";transport="
// param, else "tls" for sips:, else "".
func TransportForURI(u sip.Uri) string {
	if u.UriParams != nil {
		if t, ok := u.UriParams.Get("transport"); ok && t != "" {
			return strings.ToLower(t)
		}
	}
	if strings.EqualFold(u.Scheme, "sips") {
		return "tls"
	}
	return ""
}

// defaultPortForURI returns the port a SIP URI implies when it carries none.
func defaultPortForURI(u sip.Uri) int {
	if strings.EqualFold(u.Scheme, "sips") {
		return 5061
	}
	return 5060
}

// looseRouteHeader builds a "Route: <uri;lr>" header. The ";lr" is mandatory:
// sipgo rewrites the Request-URI to a Route that lacks it (strict routing),
// which would corrupt every in-dialog request.
func looseRouteHeader(u sip.Uri) sip.Header {
	if _, ok := u.UriParams.Get("lr"); !ok {
		// UriParams is a slice and Add appends into the shared backing array,
		// so clone before mutating a URI the caller still owns.
		u.UriParams = u.UriParams.Clone()
		u.UriParams.Add("lr", "")
	}
	return sip.NewHeader("Route", "<"+u.String()+">")
}

// proxyString renders a configured proxy for API snapshots; empty when unset.
func proxyString(u *sip.Uri) string {
	if u == nil {
		return ""
	}
	return u.String()
}
