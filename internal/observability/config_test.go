package observability

import (
	"reflect"
	"testing"
)

func TestParseHeaders(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"empty", "", nil},
		{"single", "authorization=Bearer xyz", map[string]string{"authorization": "Bearer xyz"}},
		{"multiple", "a=1,b=2", map[string]string{"a": "1", "b": "2"}},
		{"spaces trimmed", " a = 1 , b = 2 ", map[string]string{"a": "1", "b": "2"}},
		{"malformed skipped", "a=1,garbage,b=2", map[string]string{"a": "1", "b": "2"}},
		{"empty key skipped", "=1,b=2", map[string]string{"b": "2"}},
		{"empty value skipped", "a=,b=2", map[string]string{"b": "2"}},
		{"all malformed", "garbage", nil},
		{"value with equals", "a=b=c", map[string]string{"a": "b=c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseHeaders(tc.raw); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseHeaders(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestConfigRatioClamp(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{"zero", 0, 0},
		{"half", 0.5, 0.5},
		{"one", 1, 1},
		{"negative clamps to zero", -0.5, 0},
		{"above one clamps to one", 7, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Config{SamplerRatio: tc.in}).Ratio(); got != tc.want {
				t.Errorf("Ratio() with %v = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestConfigPropagatorsList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty yields nil", "", nil},
		{"default pair", "tracecontext,baggage", []string{"tracecontext", "baggage"}},
		{"single", "tracecontext", []string{"tracecontext"}},
		{"trims and lowercases", " TraceContext , BAGGAGE ", []string{"tracecontext", "baggage"}},
		{"drops empty entries", "tracecontext,,baggage,", []string{"tracecontext", "baggage"}},
		{"only commas yields nil", ",,,", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Config{Propagators: tc.in}).PropagatorsList(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PropagatorsList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
