package agent

import (
	"log/slog"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/providercfg"
)

func TestDeepgramSettings_MIPOptOut(t *testing.T) {
	cases := []struct {
		name string
		cfg  providercfg.Deepgram
		opts Options
		want bool
	}{
		{name: "default settings, off", opts: Options{}},
		{name: "default settings, on", cfg: providercfg.Deepgram{MIPOptOut: true}, want: true},
		{
			name: "caller settings, on",
			cfg:  providercfg.Deepgram{MIPOptOut: true},
			opts: Options{Settings: map[string]interface{}{"agent": map[string]interface{}{}}},
			want: true,
		},
		{
			// The server-wide opt-out must not be undone by a request.
			name: "caller tries to opt back in",
			cfg:  providercfg.Deepgram{MIPOptOut: true},
			opts: Options{Settings: map[string]interface{}{"mip_opt_out": false}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewDeepgram(tc.cfg, slog.Default()).buildSettings(tc.opts)
			v, ok := got["mip_opt_out"].(bool)
			if tc.want && (!ok || !v) {
				t.Errorf("mip_opt_out = %v, want true", got["mip_opt_out"])
			}
			if !tc.want && ok {
				t.Errorf("mip_opt_out set to %v, want absent", v)
			}
		})
	}
}

func TestDeepgramAgentURL(t *testing.T) {
	if got := (providercfg.Deepgram{}).AgentURL(); got != "wss://agent.deepgram.com/v1/agent/converse" {
		t.Errorf("default AgentURL = %q", got)
	}
	if got := (providercfg.Deepgram{BaseURL: "https://api.eu.deepgram.com"}).AgentURL(); got != "wss://api.eu.deepgram.com/v1/agent/converse" {
		t.Errorf("eu AgentURL = %q", got)
	}
}
