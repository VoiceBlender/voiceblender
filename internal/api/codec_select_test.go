package api

import (
	"reflect"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestBuildOfferedCodecs(t *testing.T) {
	if got := buildOfferedCodecs(nil); got != nil {
		t.Errorf("buildOfferedCodecs(nil) = %v, want nil", got)
	}
	if got := buildOfferedCodecs(&sipmod.SDPMedia{}); got != nil {
		t.Errorf("buildOfferedCodecs(empty) = %v, want nil", got)
	}

	remote := &sipmod.SDPMedia{
		Codecs: []codec.CodecType{codec.CodecOpus, codec.CodecPCMU, codec.CodecPCMA},
		CodecPTs: map[codec.CodecType]uint8{
			codec.CodecOpus: 111,
			codec.CodecPCMU: 0,
			codec.CodecPCMA: 8,
		},
		CodecRates: map[codec.CodecType]int{
			codec.CodecOpus: 48000,
			codec.CodecPCMU: 8000,
			codec.CodecPCMA: 8000,
		},
	}

	got := buildOfferedCodecs(remote)
	want := []events.OfferedCodec{
		{Name: "opus", PayloadType: 111, ClockRate: 48000, Priority: 1},
		{Name: "PCMU", PayloadType: 0, ClockRate: 8000, Priority: 2},
		{Name: "PCMA", PayloadType: 8, ClockRate: 8000, Priority: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOfferedCodecs:\n got = %#v\nwant = %#v", got, want)
	}
}

// Codec lookup falls back to defaults when CodecPTs/CodecRates are nil
// (e.g. older callers constructing SDPMedia by hand).
func TestBuildOfferedCodecs_FallbackDefaults(t *testing.T) {
	remote := &sipmod.SDPMedia{
		Codecs: []codec.CodecType{codec.CodecPCMU},
	}
	got := buildOfferedCodecs(remote)
	if len(got) != 1 {
		t.Fatalf("got %d codecs, want 1", len(got))
	}
	if got[0].PayloadType != 0 || got[0].ClockRate != 8000 {
		t.Errorf("fallback PCMU = %+v, want PT=0 ClockRate=8000", got[0])
	}
}
