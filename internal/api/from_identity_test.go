package api

import "testing"

// TestSplitFromIdentity pins how a POST /v1/legs `from` value is split into the
// user and host parts of the outbound From URI.
//
// Every expected value below was produced by running sip.ParseUri from the
// pinned github.com/emiago/sipgo release against that exact input.
func TestSplitFromIdentity(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantUser string
		wantHost string
	}{
		// A full SIP URI is split. Before this existed the whole string
		// landed in the user-part and produced a From with two '@'.
		{"full uri", "sip:alice@pbx.example.com", "alice", "pbx.example.com"},
		{"bare user", "alice", "alice", ""},
		{"e164", "+15551234567", "+15551234567", ""},
		{"empty", "", "", ""},
		// Port and URI params are deliberately discarded — a From URI
		// should carry neither.
		{"sips with port", "sips:bob@tls.example.com:5061", "bob", "tls.example.com"},
		{"uri params", "sip:alice@pbx.example.com;transport=tcp", "alice", "pbx.example.com"},
		// sipgo parses tel: as host-only (user is empty), so this falls
		// through to the raw-value behaviour rather than becoming a
		// host-only From. This row is what makes the `u.User != ""`
		// conjunct of the accept condition load-bearing.
		{"tel uri", "tel:+15551234567", "tel:+15551234567", ""},
		// sipgo parses "sip:alice@" as user with an EMPTY host — the only
		// shape where the parse succeeds and the user is set but the host
		// is not. This row is what makes the `u.Host != ""` conjunct
		// load-bearing; without it the From would silently lose its host.
		{"user with no host", "sip:alice@", "sip:alice@", ""},
		// The host is lowercased to match CanonicalizeAOR, so the same
		// identity spelled differently produces the same wire form. The
		// user-part is NOT lowercased — SIP user-parts are case-sensitive.
		{"mixed case host", "sip:Alice@PBX.Example.COM", "Alice", "pbx.example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user, host := splitFromIdentity(tc.in)
			if user != tc.wantUser || host != tc.wantHost {
				t.Errorf("splitFromIdentity(%q) = (%q, %q), want (%q, %q)",
					tc.in, user, host, tc.wantUser, tc.wantHost)
			}
		})
	}
}
