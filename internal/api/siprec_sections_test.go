package api

import (
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/VoiceBlender/voiceblender/internal/siprec"
)

// Only a labelled section binds to a <stream> element, so an unlabelled one is
// no evidence about anything. It is dropped here rather than carried on the
// session for the life of the recording.
func TestMediaSections(t *testing.T) {
	cases := []struct {
		name string
		sdp  *sipmod.SDPMedia
		want []siprec.MediaSection
	}{
		{name: "no sdp", sdp: nil},
		{
			name: "labelled sections are kept with their cname",
			sdp: &sipmod.SDPMedia{Audio: []sipmod.RemoteAudioStream{
				{Label: "0", CNAME: "sip:alice@example.com"},
				{Label: "1", CNAME: "sip:bob@example.com"},
			}},
			want: []siprec.MediaSection{
				{Label: "0", CNAME: "sip:alice@example.com"},
				{Label: "1", CNAME: "sip:bob@example.com"},
			},
		},
		{
			name: "unlabelled sections are dropped",
			sdp: &sipmod.SDPMedia{Audio: []sipmod.RemoteAudioStream{
				{CNAME: "sip:alice@example.com"},
				{Label: "1", CNAME: "sip:bob@example.com"},
				{},
			}},
			want: []siprec.MediaSection{{Label: "1", CNAME: "sip:bob@example.com"}},
		},
		{
			name: "every section unlabelled",
			sdp: &sipmod.SDPMedia{Audio: []sipmod.RemoteAudioStream{
				{CNAME: "sip:alice@example.com"}, {},
			}},
			want: []siprec.MediaSection{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mediaSections(tc.sdp)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d sections, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("section %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
