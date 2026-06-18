package leg

import "testing"

func TestParseSTUNHostPort(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"stun:stun.l.google.com:19302", "stun.l.google.com:19302"},
		{"stun:stun.l.google.com", "stun.l.google.com:3478"},
		{"stun.l.google.com:19302", "stun.l.google.com:19302"},
		{"stun.l.google.com", "stun.l.google.com:3478"},
		{"  stun:host.example:1234  ", "host.example:1234"},
		{"stuns:secure.example", "secure.example:3478"},
		{"http://nope", ""},
		{"", ""},
		{"stun:[2001:db8::1]:19302", "[2001:db8::1]:19302"},
	}
	for _, c := range cases {
		got := parseSTUNHostPort(c.in)
		if got != c.want {
			t.Errorf("parseSTUNHostPort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
