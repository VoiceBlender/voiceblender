package leg

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/pion/webrtc/v4"
)

func TestConfigureOpus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codec codec.CodecType
		fmtp  string
		want  codec.OpusParams
	}{
		{"no fmtp", codec.CodecOpus, "", codec.OpusParams{Bitrate: codec.DefaultOpusBitrate}},
		{"peer caps bitrate and bandwidth", codec.CodecOpus, "useinbandfec=1; maxaveragebitrate=12000; maxplaybackrate=16000",
			codec.OpusParams{Bitrate: 12000, MaxPlaybackRate: 16000}},
		{"other codec untouched", codec.CodecPCMU, "maxaveragebitrate=12000", codec.OpusParams{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestSIPLeg(tc.codec)
			remote := &sipmod.SDPMedia{CodecFmtp: map[codec.CodecType]string{codec.CodecOpus: tc.fmtp}}
			l.configureOpus(l.prim, remote)
			if l.prim.opus != tc.want {
				t.Errorf("opus = %+v, want %+v", l.prim.opus, tc.want)
			}
		})
	}
}

func liveOpusBitrate(t *testing.T, c *liveCodec) int {
	t.Helper()
	enc, ok := c.encoder.(*codec.OpusEncoder)
	if !ok {
		t.Fatalf("encoder is %T, want *codec.OpusEncoder", c.encoder)
	}
	return enc.Bitrate()
}

// TestReInviteOpusCapRebuildsEncoder: a re-offer that only changes the peer's
// Opus limits must reach the encoder, and an unchanged one must leave it be.
func TestReInviteOpusCapRebuildsEncoder(t *testing.T) {
	l := newRunningLeg(t, codec.CodecOpus, codec.CodecOpus)
	before := l.prim.live()
	if got := liveOpusBitrate(t, before); got != codec.DefaultOpusBitrate {
		t.Fatalf("initial bitrate = %d, want %d", got, codec.DefaultOpusBitrate)
	}
	fired := make(chan int, 1)
	l.SetOnMediaRateChange(func(_ string, rate int) { fired <- rate })

	capped := offerSDP(41000, "111", "a=rtpmap:111 opus/48000/2", "a=fmtp:111 maxaveragebitrate=12000")
	l.ApplyRemoteOffer(capped)
	after := l.prim.live()
	if after.encoder == before.encoder {
		t.Fatal("a new bitrate cap needs a new encoder")
	}
	if got := liveOpusBitrate(t, after); got != 12000 {
		t.Errorf("bitrate after re-offer = %d, want 12000", got)
	}
	select {
	case rate := <-fired:
		t.Errorf("a bitrate-only change must not report a rate change, got %d", rate)
	default:
	}

	l.ApplyRemoteOffer(capped)
	if l.prim.live().encoder != after.encoder {
		t.Error("an unchanged re-offer rebuilt the encoder")
	}
}

func TestPCMedia_FollowsRemoteOpusFmtp(t *testing.T) {
	m, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecOpus, OpusBitrate: 32000, Log: testLogger()})
	if err != nil {
		t.Fatalf("NewPCMedia: %v", err)
	}
	defer m.Close()
	if got, want := *m.opus.Load(), (codec.OpusParams{Bitrate: 32000}); got != want {
		t.Fatalf("initial opus = %+v, want %+v", got, want)
	}

	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(offer.SDP, "a=fmtp:111 ") {
		t.Fatalf("client offer has no Opus fmtp:\n%s", offer.SDP)
	}
	offer.SDP = strings.Replace(offer.SDP, "a=fmtp:111 ", "a=fmtp:111 maxaveragebitrate=12000;maxplaybackrate=16000;", 1)

	if err := m.SetRemoteDescription(offer); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	if got, want := *m.opus.Load(), (codec.OpusParams{Bitrate: 12000, MaxPlaybackRate: 16000}); got != want {
		t.Errorf("opus after offer = %+v, want %+v", got, want)
	}
}
