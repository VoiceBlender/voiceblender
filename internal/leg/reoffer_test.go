package leg

import (
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func reofferSDP(port int, direction string) []byte {
	return []byte(strings.Join([]string{
		"v=0",
		"o=- 1 1 IN IP4 192.0.2.9",
		"s=-",
		"c=IN IP4 192.0.2.9",
		"t=0 0",
		"m=audio " + strconv.Itoa(port) + " RTP/AVP 0 101",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=" + direction,
		"",
	}, "\r\n"))
}

// newReofferLeg builds a leg with a real RTP socket on its primary stream so
// SetRemote has something to act on. The port is OS-assigned — these tests care
// about the remote address, not the local one.
func newReofferLeg(t *testing.T) *SIPLeg {
	t.Helper()
	sess, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatalf("NewRTPSession: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	l := newTestSIPLeg(codec.CodecPCMU)
	l.log = slog.New(slog.DiscardHandler)
	l.localIP = "192.0.2.1"
	l.supportedCodecs = []codec.CodecType{codec.CodecPCMU}
	l.prim.rtpSess = sess
	l.prim.rtpPT = codec.CodecPCMU.PayloadType()
	return l
}

// TestApplyRemoteOffer_RemoteAddressChangeApplied is the regression guard for
// the bug this path fixes: a peer that moves its media address on a re-INVITE
// (SBC re-anchor, post-transfer) used to leave the call one-way, because the
// re-INVITE handler only ever read the direction string and nothing called
// SetRemote again.
func TestApplyRemoteOffer_RemoteAddressChangeApplied(t *testing.T) {
	l := newReofferLeg(t)
	if err := l.prim.rtpSess.SetRemote("192.0.2.5", 40000); err != nil {
		t.Fatalf("initial SetRemote: %v", err)
	}
	before := l.prim.rtpSess.RemoteAddr()

	answer, direction := l.ApplyRemoteOffer(reofferSDP(41234, sipmod.DirSendRecv))

	if len(answer) == 0 {
		t.Fatal("ApplyRemoteOffer must return an SDP answer")
	}
	if direction != sipmod.DirSendRecv {
		t.Errorf("direction = %q, want sendrecv", direction)
	}
	after := l.prim.rtpSess.RemoteAddr()
	if after == before {
		t.Fatalf("remote address was not re-applied (still %v)", before)
	}
	if !strings.Contains(after.String(), "192.0.2.9:41234") {
		t.Errorf("remote = %v, want 192.0.2.9:41234", after)
	}
}

func TestApplyRemoteOffer_MirrorsDirection(t *testing.T) {
	cases := map[string]string{
		sipmod.DirSendOnly: sipmod.DirRecvOnly,
		sipmod.DirRecvOnly: sipmod.DirSendOnly,
		sipmod.DirInactive: sipmod.DirInactive,
		sipmod.DirSendRecv: sipmod.DirSendRecv,
	}
	for offered, want := range cases {
		l := newReofferLeg(t)
		answer, direction := l.ApplyRemoteOffer(reofferSDP(41000, offered))
		if direction != offered {
			t.Errorf("direction = %q, want %q", direction, offered)
		}
		if !strings.Contains(string(answer), "a="+want) {
			t.Errorf("offer %q answered without a=%q:\n%s", offered, want, answer)
		}
	}
}

func TestApplyRemoteOffer_NoBodyIsNoop(t *testing.T) {
	l := newReofferLeg(t)
	answer, direction := l.ApplyRemoteOffer(nil)
	if answer != nil || direction != "" {
		t.Errorf("empty body = (%q, %q), want (nil, \"\") — a session-timer refresh renegotiates nothing", answer, direction)
	}
}

func TestApplyRemoteOffer_MalformedBodyStillAnswers(t *testing.T) {
	l := newReofferLeg(t)
	// A body we cannot parse must not drop the call: RFC 3261 §14.2 still wants
	// SDP in the 200 OK, so we re-offer our current state.
	answer, direction := l.ApplyRemoteOffer([]byte("not sdp at all"))
	if len(answer) == 0 {
		t.Error("a malformed offer must still produce an answer body")
	}
	if direction != "" {
		t.Errorf("direction = %q, want empty for an unparseable offer", direction)
	}
}

func TestApplyAnswer_ReappliesRemoteAddress(t *testing.T) {
	l := newReofferLeg(t)
	if err := l.prim.rtpSess.SetRemote("192.0.2.5", 40000); err != nil {
		t.Fatalf("initial SetRemote: %v", err)
	}

	// The peer's answer to an offer we sent moves its media port.
	l.applyAnswer(reofferSDP(41999, sipmod.DirSendRecv))

	if got := l.prim.rtpSess.RemoteAddr().String(); !strings.Contains(got, "192.0.2.9:41999") {
		t.Errorf("remote = %s, want 192.0.2.9:41999", got)
	}
}

func TestMatchRemoteStream_PrefersMIDOverPosition(t *testing.T) {
	l := newReofferLeg(t)
	l.prim.mid = "xlat"

	remote, err := sipmod.ParseSDP([]byte(strings.Join([]string{
		"v=0",
		"o=- 1 1 IN IP4 192.0.2.9",
		"s=-",
		"c=IN IP4 192.0.2.9",
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=mid:orig",
		"m=audio 40002 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=mid:xlat",
		"",
	}, "\r\n")))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}

	// The stream sits at m-line index 0 but carries mid "xlat", which the peer
	// put second — mid must win, or a reordering B2BUA would cross the streams.
	got := l.matchRemoteStream(l.prim, remote)
	if got == nil || got.RemotePort != 40002 {
		t.Errorf("matched port = %v, want 40002 (the a=mid match)", got)
	}
}

// TestAdoptOutboundStreams_PeerRejectsExtraStream covers graceful degradation:
// the peer answers the second offered section with port 0, so that stream is
// torn down and tombstoned while the call continues on the primary.
func TestAdoptOutboundStreams_PeerRejectsExtraStream(t *testing.T) {
	primary, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatalf("NewRTPSession: %v", err)
	}
	t.Cleanup(func() { primary.Close() })
	extra, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatalf("NewRTPSession: %v", err)
	}
	t.Cleanup(func() { extra.Close() })

	l := newTestSIPLeg(codec.CodecPCMU)
	l.log = slog.New(slog.DiscardHandler)
	l.localIP = "192.0.2.1"
	l.supportedCodecs = []codec.CodecType{codec.CodecPCMU}

	answer, err := sipmod.ParseSDP([]byte(strings.Join([]string{
		"v=0",
		"o=- 1 1 IN IP4 192.0.2.9",
		"s=-",
		"c=IN IP4 192.0.2.9",
		"t=0 0",
		"m=audio 41000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
		"m=audio 0 RTP/AVP 0", // the peer refused this section
		"",
	}, "\r\n")))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}

	call := &sipmod.OutboundCall{
		RemoteSDP:    answer,
		RTPSess:      primary,
		ExtraRTPSess: []*sipmod.RTPSession{extra},
		OfferedStreams: []sipmod.AudioStream{
			{Port: primary.LocalPort(), MID: "0", Direction: sipmod.DirSendRecv},
			{Port: extra.LocalPort(), MID: "1", Direction: sipmod.DirSendOnly},
		},
	}

	if !l.adoptOutboundStreams(call) {
		t.Fatal("the primary stream must still come up")
	}
	if got := l.StreamCount(); got != 1 {
		t.Errorf("stream count = %d, want 1 — the rejected stream must be dropped", got)
	}
	// RFC 3264 §8: the slot survives so subsequent offers keep the same shape.
	if l.mlines.Len() != 2 {
		t.Fatalf("m-line count = %d, want 2", l.mlines.Len())
	}
	if got := l.mlines.Slot(1).State; got != sipmod.SlotTombstone {
		t.Errorf("rejected slot state = %v, want tombstone", got)
	}
}
