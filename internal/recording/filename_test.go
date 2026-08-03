package recording

import (
	"strings"
	"testing"
)

func TestSanitizeBasename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "uuid call id", in: "c3ef9e71-6c9c-43a0-9a55-c51b3894a03c", want: "c3ef9e71-6c9c-43a0-9a55-c51b3894a03c.wav"},
		{name: "with wav suffix", in: "call-123.wav", want: "call-123.wav"},
		{name: "uppercase wav suffix", in: "call-123.WAV", want: "call-123.wav"},
		{name: "dotted stem preserved", in: "my.call.id", want: "my.call.id.wav"},
		{name: "dotted stem with wav", in: "call.v2.wav", want: "call.v2.wav"},
		{name: "numeric suffix not truncated", in: "call.123", want: "call.123.wav"},
		{name: "empty", in: "", wantErr: true},
		{name: "path traversal", in: "../secret.wav", wantErr: true},
		{name: "nested path", in: "dev/call.wav", wantErr: true},
		{name: "spaces trimmed", in: "  my-call  ", want: "my-call.wav"},
		{name: "invalid chars", in: "call id.wav", wantErr: true},
		{name: "leading dot", in: ".hidden", wantErr: true},
		{name: "trailing dot", in: "call.", wantErr: true},
		{name: "only wav", in: ".wav", wantErr: true},
		{name: "double dot segment", in: "a..b", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SanitizeBasename(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SanitizeBasename(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SanitizeBasename(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("SanitizeBasename(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveRecordingBasename_Default(t *testing.T) {
	t.Parallel()

	got, err := resolveRecordingBasename("")
	if err != nil {
		t.Fatalf("resolveRecordingBasename: %v", err)
	}
	if !strings.HasSuffix(got, ".wav") {
		t.Fatalf("basename = %q, want .wav suffix", got)
	}
	if !strings.Contains(got, "_") {
		t.Fatalf("basename = %q, want timestamp prefix", got)
	}
}
