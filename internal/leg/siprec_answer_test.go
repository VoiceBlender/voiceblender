package leg

import (
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// srcOfferSDP is the shape an SRC sends: one sendonly m=audio per recorded
// participant, each carrying the a=label the metadata document binds.
const srcOfferSDP = "v=0\r\n" +
	"o=- 1 0 IN IP4 192.0.2.9\r\n" +
	"s=-\r\n" +
	"c=IN IP4 192.0.2.9\r\n" +
	"t=0 0\r\n" +
	"m=audio 40000 RTP/AVP 0\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=sendonly\r\n" +
	"a=mid:1\r\n" +
	"a=label:1\r\n" +
	"m=audio 40002 RTP/AVP 0\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=sendonly\r\n" +
	"a=mid:2\r\n" +
	"a=label:2\r\n"

func newRecordingSessionLeg(t *testing.T, offerSDP string) *SIPLeg {
	t.Helper()
	l := newAnswerLeg(t, offerSDP)
	l.maxAnswerDirection = sipmod.DirRecvOnly
	l.streamsIndependent = true
	l.strictMLines = true
	return l
}

func TestRecordingSessionAnswer_EverySectionRecvOnlyAndLabelled(t *testing.T) {
	l := newRecordingSessionLeg(t, srcOfferSDP)

	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatalf("negotiateInboundAnswer = %v, want nil", err)
	}
	got := string(answer)

	if n := countMLines(answer); n != 2 {
		t.Fatalf("answer carries %d m-lines, want 2", n)
	}
	if c := strings.Count(got, "a=recvonly"); c != 2 {
		t.Errorf("answer carries %d a=recvonly, want 2:\n%s", c, got)
	}
	if strings.Contains(got, "a=sendrecv") || strings.Contains(got, "a=sendonly") {
		t.Errorf("a recording session must never offer to transmit:\n%s", got)
	}
	// SBCs correlate the metadata to the media on the echoed label.
	for _, label := range []string{"a=label:1", "a=label:2"} {
		if !strings.Contains(got, label) {
			t.Errorf("answer does not echo %s:\n%s", label, got)
		}
	}
	if strings.Contains(got, "m=audio 0 ") {
		t.Errorf("no section should have been rejected:\n%s", got)
	}

	// Neither stream may transmit, so neither gets a writeLoop.
	for _, s := range l.audioStreams() {
		if s.sends() {
			t.Errorf("stream %q sends(), want false for a recording session", s.id)
		}
		if !s.receives() {
			t.Errorf("stream %q does not receive, want true", s.id)
		}
	}
}

func TestRecordingSessionAnswer_SendRecvOfferIsNarrowed(t *testing.T) {
	offer := strings.ReplaceAll(srcOfferSDP, "a=sendonly", "a=sendrecv")
	l := newRecordingSessionLeg(t, offer)

	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatalf("negotiateInboundAnswer = %v, want nil", err)
	}
	if c := strings.Count(string(answer), "a=recvonly"); c != 2 {
		t.Errorf("a sendrecv offer must still be answered recvonly, got:\n%s", answer)
	}
}

func TestRecordingSessionLeg_StreamsHaveNoPrivilegedPrimary(t *testing.T) {
	l := newRecordingSessionLeg(t, srcOfferSDP)
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatalf("negotiateInboundAnswer = %v, want nil", err)
	}

	if got := l.SecondaryStreamIDs(); len(got) != 2 {
		t.Fatalf("SecondaryStreamIDs = %v, want both streams", got)
	}

	// Stream 0 must take a room and a role of its own; on an ordinary leg both
	// setters are no-ops for the primary.
	l.SetStreamRoom("0", "room-a")
	l.SetStreamRole("0", "caller")

	rooms := l.StreamRooms()
	if rooms["0"] != "room-a" {
		t.Fatalf("StreamRooms = %v, want stream 0 attached to room-a", rooms)
	}
	info, ok := l.Stream("0")
	if !ok {
		t.Fatal(`Stream("0") = (_, false), want true`)
	}
	if info.RoomID != "room-a" || info.Role != "caller" {
		t.Fatalf("stream 0 = (room %q, role %q), want (room-a, caller)", info.RoomID, info.Role)
	}
}

func TestOrdinaryLeg_PrimaryKeepsItsPrivileges(t *testing.T) {
	l := newAnswerLeg(t, twoAudioOfferSDP)
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatalf("negotiateInboundAnswer = %v, want nil", err)
	}

	if got := l.SecondaryStreamIDs(); len(got) != 1 {
		t.Fatalf("SecondaryStreamIDs = %v, want only the non-primary stream", got)
	}
	l.SetStreamRoom("0", "room-a")
	if got := l.StreamRooms(); got["0"] != "" {
		t.Fatalf("StreamRooms = %v, want the primary excluded on an ordinary leg", got)
	}
}

// A rejected re-offer section must tear the old stream down: a participant
// leaving a recording session arrives as a port-0 re-offer, and leaving the
// stream running would leak its RTP port and its readLoop.
func TestBuildAnswerStreams_RejectedSectionClosesItsStream(t *testing.T) {
	l := newRecordingSessionLeg(t, srcOfferSDP)
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatalf("negotiateInboundAnswer = %v, want nil", err)
	}
	if got := l.StreamCount(); got != 2 {
		t.Fatalf("StreamCount = %d, want 2", got)
	}

	sess := func(id string) *sipmod.RTPSession {
		for _, s := range l.audioStreams() {
			if s.id == id {
				return s.rtpSess
			}
		}
		return nil
	}
	closed := sess("1")
	if closed == nil {
		t.Fatal("stream 1 has no RTP session")
	}
	if err := closed.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("stream 1's socket is already closed before the re-offer: %v", err)
	}

	// The second participant leaves: the re-offer disables its section.
	reoffer := strings.Replace(srcOfferSDP, "m=audio 40002 RTP/AVP 0", "m=audio 0 RTP/AVP 0", 1)
	answer, _ := l.ApplyRemoteOffer([]byte(reoffer))
	if len(answer) == 0 {
		t.Fatal("ApplyRemoteOffer returned no answer")
	}

	if got := l.StreamCount(); got != 1 {
		t.Fatalf("StreamCount = %d after the section was disabled, want 1", got)
	}
	if _, ok := l.Stream("1"); ok {
		t.Error(`Stream("1") still resolves after its section was rejected`)
	}
	if err := closed.SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		t.Error("the rejected section's RTP socket is still open; its port was never released")
	}
	// The surviving section must still be answered in place, port 0.
	if n := countMLines(answer); n != 2 {
		t.Errorf("answer carries %d m-lines, want 2 (RFC 3264 §6 positional match)", n)
	}
}
