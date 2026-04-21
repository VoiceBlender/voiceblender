package api

import "testing"

func TestResolveSpeechDetection(t *testing.T) {
	tr := true
	fl := false

	cases := []struct {
		name           string
		override       *bool
		defaultEnabled bool
		want           bool
	}{
		{"nil override, default off", nil, false, false},
		{"nil override, default on", nil, true, true},
		{"override true beats default off", &tr, false, true},
		{"override false beats default on", &fl, true, false},
		{"override true matches default on", &tr, true, true},
		{"override false matches default off", &fl, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveSpeechDetection(c.override, c.defaultEnabled)
			if got != c.want {
				t.Fatalf("resolveSpeechDetection(%v, %v) = %v, want %v", c.override, c.defaultEnabled, got, c.want)
			}
		})
	}
}

func TestSpeechOverrideStore(t *testing.T) {
	s := newTestServer(t)

	if got := s.takeSpeechOverride("missing"); got != nil {
		t.Fatalf("expected nil for missing leg, got %v", got)
	}

	tr := true
	s.setSpeechOverride("leg-1", &tr)

	got := s.takeSpeechOverride("leg-1")
	if got == nil || *got != true {
		t.Fatalf("expected override=true, got %v", got)
	}

	if leftover := s.takeSpeechOverride("leg-1"); leftover != nil {
		t.Fatalf("expected override to be cleared after take, got %v", leftover)
	}
}
