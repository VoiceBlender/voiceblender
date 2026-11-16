package voxlock

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	_ "github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
)

func TestDegradedWithoutKernel(t *testing.T) {
	if Available() {
		t.Fatal("expected no kernel installed")
	}
	specs := []audiofilter.Spec{{Type: Name}}
	if err := audiofilter.Validate(specs); err != nil {
		t.Fatalf("%s must stay a known filter name: %v", Name, err)
	}
	if resolved := audiofilter.Resolve(specs); len(resolved) != 0 {
		t.Fatalf("unavailable filter should be dropped, got %+v", resolved)
	}
	if _, err := audiofilter.Build(nil, 16000, 16000, specs); err == nil {
		t.Error("building an unavailable filter should fail explicitly")
	}
	if err := audiofilter.Validate([]audiofilter.Spec{{Type: Name}, {Type: Name}}); err == nil {
		t.Errorf("%s twice must be rejected", Name)
	}
	if err := audiofilter.Validate([]audiofilter.Spec{{Type: "denoise"}, {Type: Name}}); err != nil {
		t.Errorf("denoise,%s: %v", Name, err)
	}
}

func TestInstallRejects(t *testing.T) {
	for _, c := range []Config{
		{FrameMs: 16},
		{Model: "/nonexistent/voxlock.bin", FrameMs: 16},
		{Model: "x", FrameMs: 0},
		{Model: "x", FrameMs: 12},
		{Model: "x", FrameMs: 88},
	} {
		if err := Install(c); err == nil {
			Shutdown()
			t.Errorf("Install(%+v) should fail", c)
		}
	}
	if Available() {
		t.Error("a failed Install must leave the filter unavailable")
	}
}
