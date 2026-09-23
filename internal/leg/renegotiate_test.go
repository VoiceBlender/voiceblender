package leg

import (
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// offerSDP builds a re-offer advertising exactly the given rtpmap lines.
func offerSDP(port int, pts string, rtpmaps ...string) []byte {
	lines := []string{
		"v=0", "o=- 1 1 IN IP4 192.0.2.9", "s=-", "c=IN IP4 192.0.2.9", "t=0 0",
		"m=audio " + strconv.Itoa(port) + " RTP/AVP " + pts,
	}
	lines = append(lines, rtpmaps...)
	lines = append(lines, "a=sendrecv", "")
	return []byte(strings.Join(lines, "\r\n"))
}

// newRunningLeg is a leg with its primary stream's media pipeline actually
// started, which is what renegotiation acts on.
func newRunningLeg(t *testing.T, running codec.CodecType, supported ...codec.CodecType) *SIPLeg {
	t.Helper()
	sess, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatalf("NewRTPSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	l := newTestSIPLeg(running)
	l.log = slog.New(slog.DiscardHandler)
	l.localIP = "192.0.2.1"
	l.supportedCodecs = supported
	l.prim.rtpSess = sess
	l.prim.rtpPT = running.PayloadType()
	// Inactive so setupStreamMedia builds the codec state without starting the
	// read/write loops, which would need a real peer.
	l.prim.negotiatedDir = sipmod.DirInactive
	l.setupStreamMedia(l.prim)
	if l.prim.live() == nil {
		t.Fatal("setupStreamMedia did not publish a live codec")
	}
	return l
}

// TestReInviteSwitchesCodecAndRate is the headline case: a peer re-INVITEs to a
// codec at a different sample rate. Before, the leg accepted the new codec in
// its answer and went on decoding with the old one — so it told the peer "yes,
// G.722" while still running PCMU.
func TestReInviteSwitchesCodecAndRate(t *testing.T) {
	l := newRunningLeg(t, codec.CodecPCMU, codec.CodecPCMU, codec.CodecG722)

	before := l.prim.live()
	rateCh := make(chan int, 1)
	l.SetOnMediaRateChange(func(_ string, rate int) { rateCh <- rate })

	answer, _ := l.ApplyRemoteOffer(offerSDP(41000, "9", "a=rtpmap:9 G722/8000"))

	if !strings.Contains(string(answer), "G722") {
		t.Fatalf("answer should accept G.722:\n%s", answer)
	}
	after := l.prim.live()
	if after.codecType != codec.CodecG722 {
		t.Errorf("pipeline still running %v, want G722", after.codecType)
	}
	if after.decoder == before.decoder || after.encoder == before.encoder {
		t.Error("codec changed but the encoder/decoder were not rebuilt")
	}
	if got, want := after.pcmFrameBytes, 16000/50*2; got != want {
		t.Errorf("frame is %d bytes, want %d (20 ms at 16 kHz)", got, want)
	}
	if got, want := after.rtpPT, codec.CodecG722.PayloadType(); got != want {
		t.Errorf("receive PT = %d, want %d", got, want)
	}
	if got := l.prim.jbFrameBytes; got != after.pcmFrameBytes {
		t.Errorf("jitter-buffer frame is %d bytes, want %d", got, after.pcmFrameBytes)
	}
	if got := l.SampleRate(); got != 16000 {
		t.Errorf("SampleRate() = %d, want 16000", got)
	}

	select {
	case rate := <-rateCh:
		if rate != 16000 {
			t.Errorf("rate change reported %d, want 16000", rate)
		}
		t.Logf("PCMU -> G722: pipeline rebuilt, room notified of %d Hz", rate)
	case <-time.After(time.Second):
		t.Error("a rate change must be reported, or the room keeps resampling from the old rate")
	}
}

// TestReInviteSameCodecDoesNotRebuild: hold, unhold and SBC re-anchors all
// re-offer the same codec. Rebuilding there would drop the recurrent state of
// everything downstream for no reason, and firing a rate change would have the
// room tear down a working chain.
func TestReInviteSameCodecDoesNotRebuild(t *testing.T) {
	l := newRunningLeg(t, codec.CodecPCMU, codec.CodecPCMU, codec.CodecG722)

	before := l.prim.live()
	fired := make(chan int, 1)
	l.SetOnMediaRateChange(func(_ string, rate int) { fired <- rate })

	l.ApplyRemoteOffer(offerSDP(41000, "0", "a=rtpmap:0 PCMU/8000"))

	after := l.prim.live()
	if after != before {
		t.Errorf("an unchanged codec should leave the pipeline untouched (%p -> %p)", before, after)
	}
	select {
	case rate := <-fired:
		t.Errorf("no rate change should be reported, got %d", rate)
	default:
	}
	t.Log("re-offer of the running codec: pipeline left alone, no room rebuild")
}

// TestReInviteDropsStaleAudioOnRateChange: frames already queued are PCM at the
// old rate. Delivering them after the switch would play a burst at the wrong
// speed, which is audible as a chirp.
func TestReInviteDropsStaleAudioOnRateChange(t *testing.T) {
	l := newRunningLeg(t, codec.CodecPCMU, codec.CodecPCMU, codec.CodecG722)

	l.prim.inFrames <- make([]byte, 320)  // 20 ms at 8 kHz
	l.prim.outFrames <- make([]byte, 320) // ditto
	if len(l.prim.inFrames) != 1 || len(l.prim.outFrames) != 1 {
		t.Fatal("failed to queue the stale frames")
	}

	l.ApplyRemoteOffer(offerSDP(41000, "9", "a=rtpmap:9 G722/8000"))

	if n := len(l.prim.inFrames); n != 0 {
		t.Errorf("%d stale ingress frame(s) survived the rate change", n)
	}
	if n := len(l.prim.outFrames); n != 0 {
		t.Errorf("%d stale egress frame(s) survived the rate change", n)
	}
	t.Log("queued old-rate PCM dropped on the switch")
}

// TestReInviteKeepsTheFrameChannels pins what must NOT change. The room holds a
// reader over inFrames taken when the leg joined; replacing the channel would
// leave that reader listening to one nothing writes to, and the leg would go
// silent rather than change rate.
func TestReInviteKeepsTheFrameChannels(t *testing.T) {
	l := newRunningLeg(t, codec.CodecPCMU, codec.CodecPCMU, codec.CodecG722)
	in, out := l.prim.inFrames, l.prim.outFrames
	sess := l.prim.rtpSess

	l.ApplyRemoteOffer(offerSDP(41000, "9", "a=rtpmap:9 G722/8000"))

	if l.prim.inFrames != in || l.prim.outFrames != out {
		t.Error("the frame channels must survive: the room's reader is bound to them")
	}
	if l.prim.rtpSess != sess {
		t.Error("the RTP session must survive: its port is in the SDP the peer already has")
	}
	t.Log("channels and RTP socket survived; only the codec state was replaced")
}

// TestRenegotiateReportsNoChangeWithoutMedia guards the path where a re-offer
// arrives before media was ever set up — there is nothing running to swap.
func TestRenegotiateReportsNoChangeWithoutMedia(t *testing.T) {
	l := newTestSIPLeg(codec.CodecPCMU)
	l.log = slog.New(slog.DiscardHandler)
	if rate, changed := l.renegotiateStreamMedia(l.prim); changed || rate != 0 {
		t.Errorf("no media set up: got (%d, %v), want (0, false)", rate, changed)
	}
}

// TestReInviteChangesBitrateWithoutRateChange covers the other half of
// renegotiation: the codec stays, its bitrate does not. A peer that re-offers
// AMR-WB with a narrower mode-set is asking for a lower rate, which needs a new
// encoder — but nothing downstream has to move, so the room must not be told to
// rebuild a chain that is still correctly sized.
func TestReInviteChangesBitrateWithoutRateChange(t *testing.T) {
	l := newRunningLeg(t, codec.CodecAMRWB, codec.CodecAMRWB)
	l.prim.rtpPT = 96
	l.prim.amrwbMode = defaultAMRWBEncoderMode
	l.prim.amrwbOctetAligned = true
	l.setupStreamMedia(l.prim) // republish with the AMR parameters in place

	before := l.prim.live()
	if before.amrwbMode != defaultAMRWBEncoderMode {
		t.Fatalf("setup recorded mode %d, want %d", before.amrwbMode, defaultAMRWBEncoderMode)
	}
	fired := make(chan int, 1)
	l.SetOnMediaRateChange(func(_ string, rate int) { fired <- rate })

	// The peer narrows its mode-set, clamping our transmit mode down to 2.
	l.ApplyRemoteOffer(offerSDP(41000, "96",
		"a=rtpmap:96 AMR-WB/16000",
		"a=fmtp:96 octet-align=1;mode-set=0,1,2"))

	after := l.prim.live()
	if after.amrwbMode != 2 {
		t.Errorf("transmit mode = %d, want 2 (clamped to the peer's mode-set)", after.amrwbMode)
	}
	if after.encoder == before.encoder {
		t.Error("a bitrate change needs a new encoder; the old one still emits the old mode")
	}
	if after.sampleRate() != before.sampleRate() {
		t.Errorf("sample rate moved (%d -> %d); only the bitrate should have changed",
			before.sampleRate(), after.sampleRate())
	}
	select {
	case rate := <-fired:
		t.Errorf("a bitrate-only change must not report a rate change, got %d", rate)
	default:
	}
	t.Logf("AMR-WB mode %d -> %d: encoder rebuilt, rate unchanged, room left alone",
		before.amrwbMode, after.amrwbMode)
}
