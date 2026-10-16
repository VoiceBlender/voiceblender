package providercfg

import "testing"

func TestDeepgramURLs(t *testing.T) {
	tests := []struct {
		name             string
		base             string
		wantHTTP, wantWS string
		wantAgent        string
	}{
		{"default", "", "https://api.deepgram.com", "wss://api.deepgram.com", "wss://agent.deepgram.com/v1/agent/converse"},
		{"explicit default", "https://api.deepgram.com/", "https://api.deepgram.com", "wss://api.deepgram.com", "wss://agent.deepgram.com/v1/agent/converse"},
		{"eu", "https://api.eu.deepgram.com", "https://api.eu.deepgram.com", "wss://api.eu.deepgram.com", "wss://api.eu.deepgram.com/v1/agent/converse"},
		{"bare host", "api.eu.deepgram.com", "https://api.eu.deepgram.com", "wss://api.eu.deepgram.com", "wss://api.eu.deepgram.com/v1/agent/converse"},
		{"wss given", "wss://api.eu.deepgram.com", "https://api.eu.deepgram.com", "wss://api.eu.deepgram.com", "wss://api.eu.deepgram.com/v1/agent/converse"},
		{"plain http proxy", "http://dg-proxy:8080", "http://dg-proxy:8080", "ws://dg-proxy:8080", "ws://dg-proxy:8080/v1/agent/converse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Deepgram{BaseURL: tt.base}
			if got := d.HTTPBase(); got != tt.wantHTTP {
				t.Errorf("HTTPBase = %q, want %q", got, tt.wantHTTP)
			}
			if got := d.WSBase(); got != tt.wantWS {
				t.Errorf("WSBase = %q, want %q", got, tt.wantWS)
			}
			if got := d.AgentURL(); got != tt.wantAgent {
				t.Errorf("AgentURL = %q, want %q", got, tt.wantAgent)
			}
		})
	}
}

func TestElevenLabsURLs(t *testing.T) {
	e := ElevenLabs{}
	if got := e.HTTPBase(); got != "https://api.elevenlabs.io" {
		t.Errorf("default HTTPBase = %q", got)
	}
	e.BaseURL = "https://api.eu.residency.elevenlabs.io/"
	if got := e.HTTPBase(); got != "https://api.eu.residency.elevenlabs.io" {
		t.Errorf("eu HTTPBase = %q", got)
	}
	if got := e.WSBase(); got != "wss://api.eu.residency.elevenlabs.io" {
		t.Errorf("eu WSBase = %q", got)
	}
}
