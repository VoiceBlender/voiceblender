// Package providercfg holds per-vendor endpoint and data-handling settings
// shared by the STT, TTS and agent integrations.
package providercfg

import "strings"

const (
	DeepgramDefaultBaseURL   = "https://api.deepgram.com"
	deepgramDefaultAgentURL  = "wss://agent.deepgram.com/v1/agent/converse"
	ElevenLabsDefaultBaseURL = "https://api.elevenlabs.io"
	VAPIDefaultBaseURL       = "https://api.vapi.ai"
)

// Deepgram configures every Deepgram endpoint VoiceBlender talks to.
type Deepgram struct {
	// BaseURL is the API origin, e.g. https://api.eu.deepgram.com. Empty
	// means the US default.
	BaseURL string
	// MIPOptOut excludes requests from Deepgram's Model Improvement Program.
	MIPOptOut bool
}

// HTTPBase returns the REST origin with no trailing slash.
func (d Deepgram) HTTPBase() string { return httpBase(d.BaseURL, DeepgramDefaultBaseURL) }

// WSBase returns the WebSocket origin with no trailing slash.
func (d Deepgram) WSBase() string { return wsBase(d.BaseURL, DeepgramDefaultBaseURL) }

// AgentURL returns the Voice Agent endpoint. The default US origin serves the
// agent from its own host; any other origin serves it under the API host.
func (d Deepgram) AgentURL() string {
	if d.HTTPBase() == DeepgramDefaultBaseURL {
		return deepgramDefaultAgentURL
	}
	return d.WSBase() + "/v1/agent/converse"
}

// ElevenLabs configures every ElevenLabs endpoint VoiceBlender talks to.
type ElevenLabs struct {
	// BaseURL is the API origin, e.g. https://api.eu.residency.elevenlabs.io.
	// Empty means the global default.
	BaseURL string
	// DisableLogging sends enable_logging=false (zero retention mode) on the
	// endpoints that accept it.
	DisableLogging bool
}

// HTTPBase returns the REST origin with no trailing slash.
func (e ElevenLabs) HTTPBase() string { return httpBase(e.BaseURL, ElevenLabsDefaultBaseURL) }

// WSBase returns the WebSocket origin with no trailing slash.
func (e ElevenLabs) WSBase() string { return wsBase(e.BaseURL, ElevenLabsDefaultBaseURL) }

// VAPI configures the VAPI REST origin. The media WebSocket is the listenUrl
// VAPI returns per call, so it does not derive from this origin.
type VAPI struct {
	// BaseURL is the API origin. Empty means the default.
	BaseURL string
}

// HTTPBase returns the REST origin with no trailing slash.
func (v VAPI) HTTPBase() string { return httpBase(v.BaseURL, VAPIDefaultBaseURL) }

// CallURL returns the endpoint that creates a call.
func (v VAPI) CallURL() string { return v.HTTPBase() + "/call" }

func normalize(base, def string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return def
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	return base
}

func httpBase(base, def string) string {
	base = normalize(base, def)
	switch {
	case strings.HasPrefix(base, "wss://"):
		return "https://" + strings.TrimPrefix(base, "wss://")
	case strings.HasPrefix(base, "ws://"):
		return "http://" + strings.TrimPrefix(base, "ws://")
	}
	return base
}

func wsBase(base, def string) string {
	base = httpBase(base, def)
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base
}
