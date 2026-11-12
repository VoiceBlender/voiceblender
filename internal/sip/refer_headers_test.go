package sip

import "testing"

func TestReferToHeaders(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  map[string]string
	}{
		{"none", "<sip:bob@example.com>", nil},
		{"decoded", "<sip:bob@example.com?X-Interop-Tag=it-1&User-to-User=00FA%3Bencoding%3Dhex>",
			map[string]string{"X-Interop-Tag": "it-1", "User-to-User": "00FA;encoding=hex"}},
		{"bare uri", "sip:bob@example.com?X-A=1", map[string]string{"X-A": "1"}},
		{"replaces left to the attended flow", "<sip:bob@example.com?Replaces=abc%3Bto-tag%3D1&X-A=1>", map[string]string{"X-A": "1"}},
		{"denied dialog headers", "<sip:bob@example.com?From=x&Call-ID=y&Route=z&Contact=c&body=b&X-A=1>", map[string]string{"X-A": "1"}},
		{"duplicates keep the first", "<sip:bob@example.com?X-A=1&x-a=2>", map[string]string{"X-A": "1"}},
		{"malformed pairs skipped", "<sip:bob@example.com?noval&Bad%20Name=1&X-Ctl=a%0Db&X-A=1>", map[string]string{"X-A": "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, h := range ReferToHeaders(tc.value) {
				got[h.Name()] = h.Value()
			}
			if len(got) != len(tc.want) {
				t.Fatalf("headers = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q (all %v)", k, got[k], v, got)
				}
			}
		})
	}
}
