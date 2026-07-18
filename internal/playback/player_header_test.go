package playback

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestParseWAVHeaderRejectsOutOfRangeSampleRate(t *testing.T) {
	tests := []struct {
		name       string
		sampleRate uint32
	}{
		{"zero", 0},
		{"below ptime resolution", 49},
		{"sub-telephony", 4000},
		{"absurdly high", 4000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wav := buildWAV(wavFormatPCM, 1, tt.sampleRate, 16, make([]byte, 320))

			_, err := parseWAVHeader(bytes.NewReader(wav))
			if err == nil {
				t.Fatalf("parseWAVHeader(sampleRate=%d) = nil error, want rejection", tt.sampleRate)
			}
			if !strings.Contains(err.Error(), "sample rate") {
				t.Fatalf("parseWAVHeader(sampleRate=%d) error = %q, want it to mention the sample rate", tt.sampleRate, err)
			}
		})
	}
}

func TestParseWAVHeaderAcceptsSupportedSampleRates(t *testing.T) {
	for _, rate := range []uint32{8000, 16000, 44100, 48000, 192000} {
		wav := buildWAV(wavFormatPCM, 1, rate, 16, make([]byte, 320))

		hdr, err := parseWAVHeader(bytes.NewReader(wav))
		if err != nil {
			t.Fatalf("parseWAVHeader(sampleRate=%d) = %v, want success", rate, err)
		}
		if hdr.SampleRate != rate {
			t.Fatalf("hdr.SampleRate = %d, want %d", hdr.SampleRate, rate)
		}
	}
}

// streamWAV divides the data size by a frame size derived from the sample rate.
// A rate below 50 Hz makes that frame size zero, so an unvalidated header
// panics the playback goroutine with an integer divide by zero.
func TestStreamWAVLowSampleRateReturnsErrorNotPanic(t *testing.T) {
	wav := buildWAV(wavFormatPCM, 1, 0, 16, make([]byte, 320))

	p := NewPlayer(slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := p.streamWAV(context.Background(), bytes.NewReader(wav), io.Discard, 48000)
	if err == nil {
		t.Fatal("streamWAV(sampleRate=0) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "sample rate") {
		t.Fatalf("streamWAV(sampleRate=0) error = %q, want it to mention the sample rate", err)
	}
}
