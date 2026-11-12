package sip

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestParseSessionExpires(t *testing.T) {
	tests := []struct {
		input     string
		interval  uint32
		refresher string
	}{
		{"1800", 1800, ""},
		{"1800;refresher=uac", 1800, "uac"},
		{"1800;refresher=uas", 1800, "uas"},
		{"  900 ; refresher=uac ", 900, "uac"},
		{"", 0, ""},
		{"0", 0, ""},
		{"abc", 0, ""},
		{"1800;foo=bar;refresher=uac", 1800, "uac"},
	}

	for _, tt := range tests {
		interval, refresher := ParseSessionExpires(tt.input)
		if interval != tt.interval || refresher != tt.refresher {
			t.Errorf("ParseSessionExpires(%q) = (%d, %q), want (%d, %q)",
				tt.input, interval, refresher, tt.interval, tt.refresher)
		}
	}
}

func TestParseMinSE(t *testing.T) {
	tests := []struct {
		input string
		want  uint32
	}{
		{"90", 90},
		{"120;refresher=uas", 120},
		{"", 0},
		{"abc", 0},
	}

	for _, tt := range tests {
		got := ParseMinSE(tt.input)
		if got != tt.want {
			t.Errorf("ParseMinSE(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFormatSessionExpires(t *testing.T) {
	got := FormatSessionExpires(1800, "uac")
	want := "1800;refresher=uac"
	if got != want {
		t.Errorf("FormatSessionExpires(1800, uac) = %q, want %q", got, want)
	}
}

func TestRequestedSessionTimer(t *testing.T) {
	tests := []struct {
		name      string
		header    string // "-" for no header
		supported string
		want      *SessionTimerParams
	}{
		{"no header", "-", "timer", nil},
		{"refresher named", "1800;refresher=uas", "timer", &SessionTimerParams{Interval: 1800, Refresher: "uas"}},
		{"refresher named, no Supported", "1800;refresher=uac", "", &SessionTimerParams{Interval: 1800, Refresher: "uac"}},
		{"none named, sender supports timer", "1800", "replaces, timer", &SessionTimerParams{Interval: 1800, Refresher: "uac"}},
		{"none named, sender does not", "1800", "replaces", &SessionTimerParams{Interval: 1800, Refresher: "uas"}},
		{"none named, no Supported", "1800", "", &SessionTimerParams{Interval: 1800, Refresher: "uas"}},
		{"zero interval", "0", "timer", nil},
		{"not a number", "abc", "timer", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := sip.NewRequest(sip.UPDATE, sip.Uri{Scheme: "sip", Host: "192.0.2.1"})
			if tt.header != "-" {
				req.AppendHeader(sip.NewHeader("Session-Expires", tt.header))
			}
			if tt.supported != "" {
				req.AppendHeader(sip.NewHeader("Supported", tt.supported))
			}

			got := requestedSessionTimer(req)
			if got == nil || tt.want == nil {
				if got != tt.want {
					t.Fatalf("requestedSessionTimer() = %v, want %v", got, tt.want)
				}
				return
			}
			if *got != *tt.want {
				t.Errorf("requestedSessionTimer() = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestRequestedSessionTimer_HeldToMinimum(t *testing.T) {
	tests := []struct {
		name   string
		header string
		minSE  string // "" for no Min-SE header
		want   uint32
	}{
		{"below the minimum", "2;refresher=uas", "", 90},
		{"at the minimum", "90;refresher=uas", "", 90},
		{"Min-SE above our minimum", "100;refresher=uas", "120", 120},
		{"Min-SE below our minimum", "30;refresher=uas", "60", 90},
		{"above both", "1800;refresher=uas", "120", 1800},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := sip.NewRequest(sip.UPDATE, sip.Uri{Scheme: "sip", Host: "192.0.2.1"})
			req.AppendHeader(sip.NewHeader("Session-Expires", tt.header))
			if tt.minSE != "" {
				req.AppendHeader(sip.NewHeader("Min-SE", tt.minSE))
			}
			got := requestedSessionTimer(req)
			if got == nil || got.Interval != tt.want || got.Refresher != "uas" {
				t.Fatalf("requestedSessionTimer() = %+v, want interval %d and refresher uas", got, tt.want)
			}
		})
	}
}
