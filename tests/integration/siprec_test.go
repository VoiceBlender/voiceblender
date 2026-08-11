//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/VoiceBlender/voiceblender/internal/siprec"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
)

// siprecInstance is a server acting as a session recording server.
func siprecInstance(t *testing.T, name string, mutate func(*config.Config)) *testInstance {
	return newTestInstanceWithOpts(t, name, func(c *config.Config) {
		c.SIPTCPEnabled = true
		c.SIPRECEnabled = true
		c.SIPRECAutoAnswer = true
		c.SIPRECMaxStreams = 8
		c.SIPRECMetadataMaxBytes = 65536
		c.SIPRECRoomMode = config.SIPRECRoomModeNone
		if mutate != nil {
			mutate(c)
		}
	})
}

// twoPartyMetadata builds the RFC 7865 document an SRC sends for a two-party
// call: one stream per participant, labelled to match the SDP.
//
// It is deliberately compact so the whole INVITE stays small enough to debug
// against a UDP peer; the tests themselves dial over TCP, as a real SBC does.
func twoPartyMetadata(t *testing.T) []byte {
	t.Helper()
	rec := &siprec.Recording{
		DataMode:               siprec.DataModeComplete,
		SessionRecordingAssocs: []siprec.SessionRecordingAssoc{{SessionID: "s1"}},
		Participants: []siprec.Participant{
			{ParticipantID: "pa", NameIDs: []siprec.NameID{{AOR: "sip:alice@example.com"}}},
			{ParticipantID: "pb", NameIDs: []siprec.NameID{{AOR: "sip:bob@example.com"}}},
		},
		Streams: []siprec.Stream{
			{StreamID: "ta", Label: "1"},
			{StreamID: "tb", Label: "2"},
		},
		ParticipantStreams: []siprec.ParticipantStreamAssoc{
			{ParticipantID: "pa", Send: []string{"ta"}},
			{ParticipantID: "pb", Send: []string{"tb"}},
		},
	}
	md, err := rec.Marshal()
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return md
}

// dialSIPREC originates a recording session from `from` to `to`, the way an SBC
// would: two sendonly m=audio sections plus the rs-metadata document, with
// Require: siprec and a +sip.src Contact feature tag.
func dialSIPREC(t *testing.T, from, to *testInstance, metadata []byte) (*sipmod.OutboundCall, error) {
	t.Helper()
	// A SIPREC INVITE carries SDP plus the whole metadata document, past the
	// 1300 bytes RFC 3261 §18.1.1 allows on UDP, so it goes over TCP — as it
	// does from a real SBC.
	params := sip.NewParams()
	params.Add("transport", "tcp")
	recipient := sip.Uri{User: "siprec", Host: "127.0.0.1", Port: to.sipPort, UriParams: params}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return from.engine.Invite(ctx, recipient, sipmod.InviteOptions{
		Streams: []sipmod.OfferStream{
			{Direction: sipmod.DirSendOnly, Label: "1", Content: "main"},
			{Direction: sipmod.DirSendOnly, Label: "2", Content: "main"},
		},
		Headers: []sip.Header{
			sip.NewHeader("Require", sipmod.OptionTagSIPREC),
			sip.NewHeader("Contact", fmt.Sprintf("<sip:src@127.0.0.1:%d>;+sip.src", from.sipPort)),
		},
		BodyParts: []sipmod.BodyPart{{
			ContentType: sipmod.ContentTypeRSMetadata,
			Disposition: "recording-session;handling=required",
			Data:        metadata,
		}},
	})
}

func TestSIPREC_InboundSessionAnsweredRecvOnly(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	// The session was answered without any REST /answer call.
	if call.RemoteSDP == nil {
		t.Fatal("no answer SDP")
	}
	if len(call.RemoteSDP.Audio) != 2 {
		t.Fatalf("answer carries %d audio sections, want 2", len(call.RemoteSDP.Audio))
	}
	for i, a := range call.RemoteSDP.Audio {
		if a.RemotePort == 0 {
			t.Errorf("section %d was rejected; the SRS must accept both streams", i)
		}
		if a.Direction != sipmod.DirRecvOnly {
			t.Errorf("section %d direction = %q, want recvonly", i, a.Direction)
		}
		if want := fmt.Sprintf("%d", i+1); a.Label != want {
			t.Errorf("section %d label = %q, want %q echoed back", i, a.Label, want)
		}
	}

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	legID, _ := evt.Data.(interface{ GetLegID() string })
	if legID == nil || legID.GetLegID() == "" {
		t.Fatal("siprec.session_started carries no leg ID")
	}

	view := getSIPRECSession(t, srs, legID.GetLegID())
	if view.SessionID != "s1" {
		t.Errorf("session_id = %q, want s1", view.SessionID)
	}
	if len(view.Participants) != 2 {
		t.Fatalf("participants = %d, want 2", len(view.Participants))
	}
	if len(view.Streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(view.Streams))
	}
	// The whole point: each m= section resolves to the party it carries.
	byLabel := map[string]siprecStreamView{}
	for _, s := range view.Streams {
		byLabel[s.Label] = s
	}
	if got := byLabel["1"].ParticipantAOR; got != "sip:alice@example.com" {
		t.Errorf("label 1 participant = %q, want Alice", got)
	}
	if got := byLabel["2"].ParticipantAOR; got != "sip:bob@example.com" {
		t.Errorf("label 2 participant = %q, want Bob", got)
	}
	if byLabel["1"].LegStreamID == byLabel["2"].LegStreamID {
		t.Error("both labels resolved to the same leg stream")
	}

	// The leg is typed as a recording session, not a call.
	leg := getLegView(t, srs, legID.GetLegID())
	if leg.Type != "siprec_in" {
		t.Errorf("leg type = %q, want siprec_in", leg.Type)
	}
}

func TestSIPREC_RejectedWhenDisabled(t *testing.T) {
	src := newTestInstance(t, "src")
	// SIPREC off is the shipped default.
	srs := newTestInstance(t, "srs")

	_, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err == nil {
		t.Fatal("SIPREC INVITE succeeded against a server with SIPREC disabled")
	}
	if srs.collector.hasEvent(events.SIPRECSessionStarted, nil) {
		t.Error("a session was started despite SIPREC being disabled")
	}
	if srs.collector.hasEvent(events.LegRinging, nil) {
		t.Error("a rejected recording session must not surface as a ringing leg")
	}
}

// With SIPREC disabled, only Require obliges a response. An INVITE that merely
// carries the +sip.src feature tag must keep connecting as an ordinary call, or
// enabling nothing would still change behaviour for existing deployments.
func TestSIPREC_DisabledIgnoresHintOnlyInvite(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := newTestInstance(t, "srs") // SIPREC off — the shipped default

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	recipient := sip.Uri{User: "alice", Host: "127.0.0.1", Port: srs.sipPort}
	done := make(chan error, 1)
	go func() {
		_, err := src.engine.Invite(ctx, recipient, sipmod.InviteOptions{
			Headers: []sip.Header{
				sip.NewHeader("Contact", fmt.Sprintf("<sip:src@127.0.0.1:%d>;+sip.src", src.sipPort)),
			},
		})
		done <- err
	}()

	inbound := waitForInboundLeg(t, srs.baseURL(), 5*time.Second)
	if inbound.Type != "sip_inbound" {
		t.Fatalf("leg type = %q, want sip_inbound: a +sip.src hint alone must not claim the call", inbound.Type)
	}
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", srs.baseURL(), inbound.ID), nil)
	resp.Body.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ordinary call carrying +sip.src failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("INVITE did not complete")
	}
	if srs.collector.hasEvent(events.SIPRECSessionStarted, nil) {
		t.Error("a recording session was started with SIPREC disabled")
	}
}

func TestSIPREC_MissingMetadataRejected(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Claims siprec via Require but carries a plain SDP body.
	recipient := sip.Uri{User: "siprec", Host: "127.0.0.1", Port: srs.sipPort}
	_, err := src.engine.Invite(ctx, recipient, sipmod.InviteOptions{
		Headers: []sip.Header{sip.NewHeader("Require", sipmod.OptionTagSIPREC)},
	})
	if err == nil {
		t.Fatal("a SIPREC INVITE with no rs-metadata was accepted")
	}
	if srs.collector.hasEvent(events.SIPRECSessionStarted, nil) {
		t.Error("a session was started without metadata")
	}
}

func TestSIPREC_StreamsAttachedToSessionRoom(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", func(c *config.Config) {
		c.SIPRECRoomMode = config.SIPRECRoomModePerSession
	})

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	legID := evt.Data.(interface{ GetLegID() string }).GetLegID()

	// Both streams — including m-line 0, which on an ordinary leg would be the
	// privileged primary — must be mixed into the session room.
	var view siprecSessionView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		view = getSIPRECSession(t, srs, legID)
		if view.RoomID != "" && view.Streams[0].RoomID != "" && view.Streams[1].RoomID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	wantRoom := "siprec-" + legID
	if view.RoomID != wantRoom {
		t.Fatalf("session room = %q, want %q", view.RoomID, wantRoom)
	}
	for _, s := range view.Streams {
		if s.RoomID != wantRoom {
			t.Errorf("stream %q room = %q, want %q", s.LegStreamID, s.RoomID, wantRoom)
		}
		if s.Role == "" {
			t.Errorf("stream %q has no routing role; it should default to the participant", s.LegStreamID)
		}
	}
	if view.Streams[0].Role == view.Streams[1].Role {
		t.Error("both streams took the same role; each should carry its own participant")
	}
}

// A plain call against a SIPREC-enabled server must negotiate exactly as before.
func TestSIPREC_OrdinaryCallUnaffected(t *testing.T) {
	caller := newTestInstance(t, "caller")
	srs := siprecInstance(t, "srs", nil)

	outID, inID := establishCall(t, caller, srs)
	if outID == "" || inID == "" {
		t.Fatal("plain call did not establish")
	}

	leg := getLegView(t, srs, inID)
	if leg.Type != "sip_inbound" {
		t.Errorf("leg type = %q, want sip_inbound", leg.Type)
	}
	if srs.collector.hasEvent(events.SIPRECSessionStarted, nil) {
		t.Error("a plain call was treated as a recording session")
	}
}

// threePartyPartialJoin is the incremental document an SRC sends when a third
// party joins the recorded call: datamode=partial, only the new elements.
func threePartyPartialJoin(t *testing.T) []byte {
	t.Helper()
	rec := &siprec.Recording{
		DataMode:     siprec.DataModePartial,
		Participants: []siprec.Participant{{ParticipantID: "pc", NameIDs: []siprec.NameID{{AOR: "sip:carol@example.com"}}}},
		Streams:      []siprec.Stream{{StreamID: "tc", Label: "3"}},
		ParticipantStreams: []siprec.ParticipantStreamAssoc{
			{ParticipantID: "pc", Send: []string{"tc"}},
		},
	}
	md, err := rec.Marshal()
	if err != nil {
		t.Fatalf("marshal partial join: %v", err)
	}
	return md
}

// partialLeave is what an SRC sends when a party drops out: the association is
// closed with a disassociate-time rather than the element being omitted.
func partialLeave(t *testing.T, participantID string) []byte {
	t.Helper()
	rec := &siprec.Recording{
		DataMode: siprec.DataModePartial,
		ParticipantSessions: []siprec.ParticipantSessionAssoc{{
			ParticipantID:    participantID,
			DisassociateTime: "2026-08-07T09:18:22Z",
		}},
	}
	md, err := rec.Marshal()
	if err != nil {
		t.Fatalf("marshal partial leave: %v", err)
	}
	return md
}

// reInviteWithMetadata sends an in-dialog re-INVITE carrying an updated
// metadata document, optionally with a fresh SDP offer.
func reInviteWithMetadata(t *testing.T, inst *testInstance, call *sipmod.OutboundCall, sdp, metadata []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := inst.engine.SendReInviteBody(ctx, call.Dialog, sdp, []sipmod.BodyPart{{
		ContentType: sipmod.ContentTypeRSMetadata,
		Disposition: "recording-session;handling=required",
		Data:        metadata,
	}}, nil)
	if err != nil {
		t.Fatalf("re-INVITE with metadata failed: %v", err)
	}
}

func TestSIPREC_MetadataOnlyReInviteUpdatesSession(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	legID := evt.Data.(interface{ GetLegID() string }).GetLegID()

	before := getSIPRECSession(t, srs, legID)
	if len(before.Streams) != 2 {
		t.Fatalf("streams = %d before the update, want 2", len(before.Streams))
	}

	// A metadata-only re-INVITE: no SDP part at all. It must still be answered
	// and must still apply, leaving the negotiated media untouched.
	renamed := &siprec.Recording{
		DataMode:     siprec.DataModePartial,
		Participants: []siprec.Participant{{ParticipantID: "pa", NameIDs: []siprec.NameID{{AOR: "sip:alice@example.com", Name: &siprec.Name{Value: "Alice Renamed"}}}}},
	}
	md, err := renamed.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reInviteWithMetadata(t, src, call, nil, md)

	srs.collector.waitForMatch(t, events.SIPRECMetadataUpdated, nil, 5*time.Second)

	after := getSIPRECSession(t, srs, legID)
	if len(after.Streams) != 2 {
		t.Errorf("streams = %d after a metadata-only update, want 2 unchanged", len(after.Streams))
	}
	var alice string
	for _, p := range after.Participants {
		if p.ParticipantID == "pa" {
			alice = p.Name
		}
	}
	if alice != "Alice Renamed" {
		t.Errorf("participant pa name = %q, want %q — the partial update did not apply", alice, "Alice Renamed")
	}
	// A partial document must not drop the participants it does not mention.
	if len(after.Participants) != 2 {
		t.Errorf("participants = %d, want 2: a partial update never deletes on absence", len(after.Participants))
	}
}

func TestSIPREC_ReInviteRemovesParticipant(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	legID := evt.Data.(interface{ GetLegID() string }).GetLegID()

	reInviteWithMetadata(t, src, call, nil, partialLeave(t, "pb"))

	left := srs.collector.waitForMatch(t, events.SIPRECParticipantLeft, nil, 5*time.Second)
	raw, err := json.Marshal(left.Data)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var payload struct {
		ParticipantID string `json:"participant_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if payload.ParticipantID != "pb" {
		t.Errorf("participant_left participant_id = %q, want pb", payload.ParticipantID)
	}

	after := getSIPRECSession(t, srs, legID)
	if len(after.Participants) != 1 || after.Participants[0].ParticipantID != "pa" {
		t.Errorf("participants after the leave = %+v, want only pa", after.Participants)
	}
	// The leaver's stream stops carrying anyone, so its binding is dropped.
	for _, st := range after.Streams {
		if st.Label == "2" && st.ParticipantID != "" {
			t.Errorf("stream label 2 still bound to %q after its participant left", st.ParticipantID)
		}
	}
}

// srcReofferSDP renders the SRC's offer with an extra sendonly section for a
// party that just joined. The third section's port is arbitrary: this test
// exercises signalling and the metadata binding, not media flow.
func srcReofferSDP(call *sipmod.OutboundCall) []byte {
	ports := []int{call.RTPSess.LocalPort()}
	for _, s := range call.ExtraRTPSess {
		ports = append(ports, s.LocalPort())
	}
	ports = append(ports, ports[len(ports)-1]+100)

	sdp := "v=0\r\no=- 2 2 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n"
	for i, port := range ports {
		sdp += fmt.Sprintf("m=audio %d RTP/AVP 0\r\n"+
			"a=rtpmap:0 PCMU/8000\r\n"+
			"a=sendonly\r\n"+
			"a=mid:%d\r\n"+
			"a=label:%d\r\n", port, i+1, i+1)
	}
	return []byte(sdp)
}

func TestSIPREC_ReInviteAddsParticipant(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", func(c *config.Config) {
		c.SIPRECRoomMode = config.SIPRECRoomModePerSession
	})

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	legID := evt.Data.(interface{ GetLegID() string }).GetLegID()

	// Carol joins: a third sendonly m= section plus a partial metadata document
	// naming her and binding her to label 3.
	reInviteWithMetadata(t, src, call, srcReofferSDP(call), threePartyPartialJoin(t))

	joined := srs.collector.waitForMatch(t, events.SIPRECParticipantJoined, nil, 5*time.Second)
	raw, err := json.Marshal(joined.Data)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var payload struct {
		ParticipantID  string `json:"participant_id"`
		ParticipantAOR string `json:"participant_aor"`
		Label          string `json:"label"`
		LegStreamID    string `json:"leg_stream_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if payload.ParticipantID != "pc" {
		t.Errorf("participant_joined participant_id = %q, want pc", payload.ParticipantID)
	}
	if payload.ParticipantAOR != "sip:carol@example.com" {
		t.Errorf("participant_joined aor = %q, want Carol", payload.ParticipantAOR)
	}
	if payload.Label != "3" {
		t.Errorf("participant_joined label = %q, want 3 — the new m= section must be bound", payload.Label)
	}

	after := getSIPRECSession(t, srs, legID)
	if len(after.Participants) != 3 {
		t.Fatalf("participants = %d after the join, want 3", len(after.Participants))
	}
	if len(after.Streams) != 3 {
		t.Fatalf("streams = %d after the join, want 3", len(after.Streams))
	}
	for _, st := range after.Streams {
		if st.Label != "3" {
			continue
		}
		if st.ParticipantAOR != "sip:carol@example.com" {
			t.Errorf("stream label 3 participant = %q, want Carol", st.ParticipantAOR)
		}
		if st.Direction != "recvonly" {
			t.Errorf("new stream direction = %q, want recvonly", st.Direction)
		}
		// The new stream must have been pulled into the session room too.
		if st.RoomID != "siprec-"+legID {
			t.Errorf("new stream room = %q, want the session room", st.RoomID)
		}
	}
}

// sendSIPRECAudio pushes PCMU tone packets on every negotiated stream, so the
// SRS has real audio to record on each channel.
func sendSIPRECAudio(t *testing.T, call *sipmod.OutboundCall, packets int) {
	t.Helper()

	sessions := []*sipmod.RTPSession{call.RTPSess}
	sessions = append(sessions, call.ExtraRTPSess...)
	if len(sessions) != len(call.RemoteSDP.Audio) {
		t.Fatalf("have %d local sockets for %d answered sections", len(sessions), len(call.RemoteSDP.Audio))
	}

	// Only the first section's remote is pinned by the engine; the rest are
	// adopted by a leg, which this test does not have.
	for i, a := range call.RemoteSDP.Audio {
		if err := sessions[i].SetRemote(a.RemoteIP, a.RemotePort); err != nil {
			t.Fatalf("set remote for section %d: %v", i, err)
		}
	}

	payload := make([]byte, 160) // 20 ms of PCMU at 8 kHz
	for i := range payload {
		payload[i] = byte(0x30 + i%16)
	}

	for n := 0; n < packets; n++ {
		for i, sess := range sessions {
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    0, // PCMU
					SequenceNumber: uint16(n + 1),
					Timestamp:      uint32(n * 160),
					SSRC:           uint32(0x1000 + i),
				},
				Payload: payload,
			}
			if err := sess.WriteRTP(pkt); err != nil {
				t.Fatalf("write RTP on section %d: %v", i, err)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSIPREC_RecordsOneChannelPerParticipant(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	legID := evt.Data.(interface{ GetLegID() string }).GetLegID()

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", srs.baseURL(), legID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /record = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	sendSIPRECAudio(t, call, 25) // ~500 ms on each stream

	stop := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", srs.baseURL(), legID))
	defer stop.Body.Close()
	if stop.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /record = %d, want 200", stop.StatusCode)
	}
	var stopped struct {
		Status string `json:"status"`
		File   string `json:"file"`
	}
	if err := json.NewDecoder(stop.Body).Decode(&stopped); err != nil {
		t.Fatalf("decode stop response: %v", err)
	}
	if stopped.File == "" {
		t.Fatal("stop returned no file; the merged recording was not produced")
	}

	// One channel per recorded party, at the server's mixing rate.
	assertWAVAudio(t, stopped.File, 2, srs.cfg.DefaultSampleRate, 1000)

	fin := srs.collector.waitForMatch(t, events.RecordingFinished, nil, 5*time.Second)
	raw, err := json.Marshal(fin.Data)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var payload struct {
		MultiChannelFile string                    `json:"multi_channel_file"`
		Channels         map[string]map[string]any `json:"channels"`
		OmittedLegs      []string                  `json:"omitted_legs"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if payload.MultiChannelFile == "" {
		t.Error("recording.finished carries no multi_channel_file")
	}
	if len(payload.OmittedLegs) != 0 {
		t.Errorf("omitted_legs = %v, want none", payload.OmittedLegs)
	}
	// Channels are keyed by participant identity, not by leg or stream ID.
	for _, want := range []string{"sip:alice@example.com", "sip:bob@example.com"} {
		if _, ok := payload.Channels[want]; !ok {
			t.Errorf("channels has no entry for %q; got keys %v", want, mapKeys(payload.Channels))
		}
	}
}

func mapKeys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSIPREC_SRCForksRoomToRecordingServer runs the full loop: one VoiceBlender
// instance acts as the recording client and forks a room's participants to a
// second instance acting as the recording server.
func TestSIPREC_SRCForksRoomToRecordingServer(t *testing.T) {
	// Only SIPRECSRCEnabled: dialling out over TCP needs no local listener.
	src := newTestInstanceWithOpts(t, "src", func(c *config.Config) {
		c.SIPRECSRCEnabled = true
	})
	srs := siprecInstance(t, "srs", nil)
	peer := newTestInstance(t, "peer")

	// Put two ordinary calls into a room on the SRC instance.
	roomID := "conf-1"
	resp := httpPost(t, src.baseURL()+"/v1/rooms", map[string]any{"id": roomID})
	resp.Body.Close()

	var legIDs []string
	for i := 0; i < 2; i++ {
		outID, _ := establishCall(t, src, peer)
		join := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", src.baseURL(), roomID),
			map[string]any{"leg_id": outID})
		if join.StatusCode != http.StatusOK {
			t.Fatalf("add leg to room = %d, want 200", join.StatusCode)
		}
		join.Body.Close()
		legIDs = append(legIDs, outID)
	}

	// Fork the room to the recording server.
	start := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/siprec", src.baseURL(), roomID), map[string]any{
		"srs_uri":    fmt.Sprintf("sip:srs@127.0.0.1:%d;transport=tcp", srs.sipPort),
		"session_id": "conf-1-rec",
	})
	defer start.Body.Close()
	if start.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(start.Body)
		t.Fatalf("POST /rooms/{id}/siprec = %d, want 201: %s", start.StatusCode, body)
	}
	var srcLeg struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(start.Body).Decode(&srcLeg); err != nil {
		t.Fatalf("decode leg view: %v", err)
	}
	if srcLeg.Type != "siprec_out" {
		t.Errorf("leg type = %q, want siprec_out", srcLeg.Type)
	}

	// The recording server accepted it and bound both parties.
	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	srsLegID := evt.Data.(interface{ GetLegID() string }).GetLegID()

	view := getSIPRECSession(t, srs, srsLegID)
	if view.SessionID != "conf-1-rec" {
		t.Errorf("session_id = %q, want conf-1-rec", view.SessionID)
	}
	if len(view.Participants) != 2 {
		t.Fatalf("participants = %d, want 2 (one per room member)", len(view.Participants))
	}
	if len(view.Streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(view.Streams))
	}
	// Every recorded section must be bound to one of the room's legs and be
	// receive-only on the server side.
	bound := map[string]bool{}
	for _, st := range view.Streams {
		if st.Direction != "recvonly" {
			t.Errorf("stream %q direction = %q, want recvonly", st.Label, st.Direction)
		}
		if st.ParticipantID == "" {
			t.Errorf("stream %q is not bound to a participant", st.Label)
			continue
		}
		bound[st.ParticipantID] = true
	}
	for _, id := range legIDs {
		if !bound[id] {
			t.Errorf("room leg %s has no recorded stream", id)
		}
	}

	// Prove the tap→pipe→pump→RTP path actually carries audio: record on the
	// server side and check both channels received frames.
	rec := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", srs.baseURL(), srsLegID), nil)
	if rec.StatusCode != http.StatusOK {
		t.Fatalf("POST /record on the recording server = %d, want 200", rec.StatusCode)
	}
	rec.Body.Close()

	time.Sleep(600 * time.Millisecond) // let the room mixer drive the pumps

	stop := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", srs.baseURL(), srsLegID))
	defer stop.Body.Close()
	var stopped struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(stop.Body).Decode(&stopped); err != nil {
		t.Fatalf("decode stop response: %v", err)
	}
	if stopped.File == "" {
		t.Fatal("recording server produced no file; no audio reached it from the SRC")
	}
	assertWAVAudio(t, stopped.File, 2, srs.cfg.DefaultSampleRate, 1000)

	// Ending the SRC leg ends the recording session on the server.
	del := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", src.baseURL(), srcLeg.ID))
	del.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srs.collector.hasEvent(events.SIPRECSessionEnded, nil) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("recording server never saw the session end")
}

func TestSIPREC_SRCDisabledByDefault(t *testing.T) {
	src := newTestInstance(t, "src")

	resp := httpPost(t, src.baseURL()+"/v1/rooms", map[string]any{"id": "r1"})
	resp.Body.Close()

	start := httpPost(t, src.baseURL()+"/v1/rooms/r1/siprec", map[string]any{
		"srs_uri": "sip:srs@127.0.0.1:5060",
	})
	defer start.Body.Close()
	if start.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /rooms/{id}/siprec = %d, want 403 when SIPREC_SRC_ENABLED is off", start.StatusCode)
	}
}

func TestSIPREC_SRCSelectsSubsetOfRoom(t *testing.T) {
	src := newTestInstanceWithOpts(t, "src", func(c *config.Config) {
		c.SIPRECSRCEnabled = true
	})
	srs := siprecInstance(t, "srs", nil)
	peer := newTestInstance(t, "peer")

	roomID := "conf-subset"
	resp := httpPost(t, src.baseURL()+"/v1/rooms", map[string]any{"id": roomID})
	resp.Body.Close()

	var legIDs []string
	for i := 0; i < 3; i++ {
		outID, _ := establishCall(t, src, peer)
		join := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", src.baseURL(), roomID),
			map[string]any{"leg_id": outID})
		join.Body.Close()
		legIDs = append(legIDs, outID)
	}
	sort.Strings(legIDs)

	// Record only the first two of the three.
	want := legIDs[:2]
	start := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/siprec", src.baseURL(), roomID), map[string]any{
		"srs_uri": fmt.Sprintf("sip:srs@127.0.0.1:%d;transport=tcp", srs.sipPort),
		"leg_ids": want,
	})
	defer start.Body.Close()
	if start.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(start.Body)
		t.Fatalf("POST /siprec = %d, want 201: %s", start.StatusCode, body)
	}

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	view := getSIPRECSession(t, srs, evt.Data.(interface{ GetLegID() string }).GetLegID())

	if len(view.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 — only the selected legs should be forked", len(view.Streams))
	}
	got := map[string]bool{}
	for _, st := range view.Streams {
		got[st.ParticipantID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("selected leg %s has no recorded stream", id)
		}
	}
	if got[legIDs[2]] {
		t.Errorf("unselected leg %s was recorded anyway", legIDs[2])
	}
}

func TestSIPREC_SRCRejectsUnknownLegID(t *testing.T) {
	src := newTestInstanceWithOpts(t, "src", func(c *config.Config) {
		c.SIPRECSRCEnabled = true
	})
	peer := newTestInstance(t, "peer")

	roomID := "conf-unknown"
	resp := httpPost(t, src.baseURL()+"/v1/rooms", map[string]any{"id": roomID})
	resp.Body.Close()

	outID, _ := establishCall(t, src, peer)
	join := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", src.baseURL(), roomID),
		map[string]any{"leg_id": outID})
	join.Body.Close()

	start := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/siprec", src.baseURL(), roomID), map[string]any{
		"srs_uri": "sip:srs@127.0.0.1:5060;transport=tcp",
		"leg_ids": []string{outID, "not-in-this-room"},
	})
	defer start.Body.Close()
	if start.StatusCode != http.StatusNotFound {
		t.Fatalf("POST /siprec with an unknown leg_id = %d, want 404", start.StatusCode)
	}
}

func TestSIPREC_SRCForksASingleCall(t *testing.T) {
	src := newTestInstanceWithOpts(t, "src", func(c *config.Config) {
		c.SIPRECSRCEnabled = true
	})
	srs := siprecInstance(t, "srs", nil)
	peer := newTestInstance(t, "peer")

	outID, _ := establishCall(t, src, peer)

	// No room: the call's own inbound and outbound audio become two sections.
	start := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/siprec", src.baseURL(), outID), map[string]any{
		"srs_uri": fmt.Sprintf("sip:srs@127.0.0.1:%d;transport=tcp", srs.sipPort),
	})
	defer start.Body.Close()
	if start.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(start.Body)
		t.Fatalf("POST /legs/{id}/siprec = %d, want 201: %s", start.StatusCode, body)
	}
	var srcLeg struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(start.Body).Decode(&srcLeg); err != nil {
		t.Fatalf("decode leg view: %v", err)
	}
	if srcLeg.Type != "siprec_out" {
		t.Errorf("leg type = %q, want siprec_out", srcLeg.Type)
	}

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	view := getSIPRECSession(t, srs, evt.Data.(interface{ GetLegID() string }).GetLegID())

	if len(view.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 (the call's in and out)", len(view.Streams))
	}
	for _, st := range view.Streams {
		if st.Direction != "recvonly" {
			t.Errorf("stream %q direction = %q, want recvonly", st.Label, st.Direction)
		}
		if st.ParticipantID == "" {
			t.Errorf("stream %q is not bound to a participant", st.Label)
		}
	}
	// The two sections must be distinct parties, not the same one twice.
	if view.Streams[0].ParticipantID == view.Streams[1].ParticipantID {
		t.Error("both sections bound to the same participant; in and out must differ")
	}

	// A file recording on the same leg must still work: SIPREC uses its own taps.
	rec := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", src.baseURL(), outID), nil)
	if rec.StatusCode != http.StatusOK {
		t.Fatalf("POST /record alongside a recording session = %d, want 200", rec.StatusCode)
	}
	rec.Body.Close()
	stop := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", src.baseURL(), outID))
	stop.Body.Close()
}

func TestSIPREC_SRCRefusesRecordingALegTwice(t *testing.T) {
	src := newTestInstanceWithOpts(t, "src", func(c *config.Config) {
		c.SIPRECSRCEnabled = true
	})
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	srsLegID := evt.Data.(interface{ GetLegID() string }).GetLegID()

	// The recording server's own siprec_in leg must not be forkable onward.
	start := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/siprec", srs.baseURL(), srsLegID), map[string]any{
		"srs_uri": "sip:elsewhere@127.0.0.1:5060",
	})
	defer start.Body.Close()
	if start.StatusCode != http.StatusForbidden && start.StatusCode != http.StatusConflict {
		t.Fatalf("forking a recording session onward = %d, want 403 or 409", start.StatusCode)
	}
}

func TestSIPREC_SRCSelectsASecondaryStream(t *testing.T) {
	src := newTestInstanceWithOpts(t, "src", func(c *config.Config) {
		c.SIPRECSRCEnabled = true
	})
	srs := siprecInstance(t, "srs", nil)
	peer := newTestInstance(t, "peer")

	// The src instance receives a two-stream call: primary audio plus a
	// translated feed.
	call := dialMultiStream(t, peer, src, []sipmod.OfferStream{
		{},
		{Direction: sipmod.DirSendOnly, Content: "alt", Lang: "es"},
	})
	defer call.Dialog.Bye(context.Background())

	legID := waitForLegOfType(t, src, "sip_inbound")

	roomID := "xlat"
	resp := httpPost(t, src.baseURL()+"/v1/rooms", map[string]any{"id": roomID})
	resp.Body.Close()

	join := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", src.baseURL(), roomID),
		map[string]any{"leg_id": legID})
	join.Body.Close()

	attach := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/streams/1/room", src.baseURL(), legID),
		map[string]any{"room_id": roomID, "role": "translator"})
	if attach.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(attach.Body)
		attach.Body.Close()
		t.Fatalf("attach stream to room = %d: %s", attach.StatusCode, body)
	}
	attach.Body.Close()

	// Record only the translated feed, addressed as "<legID>#1" — not the call.
	streamParty := legID + "#1"
	start := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/siprec", src.baseURL(), roomID), map[string]any{
		"srs_uri": fmt.Sprintf("sip:srs@127.0.0.1:%d;transport=tcp", srs.sipPort),
		"leg_ids": []string{streamParty},
	})
	defer start.Body.Close()
	if start.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(start.Body)
		t.Fatalf("POST /siprec selecting a stream = %d, want 201: %s", start.StatusCode, body)
	}

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	view := getSIPRECSession(t, srs, evt.Data.(interface{ GetLegID() string }).GetLegID())

	if len(view.Streams) != 1 {
		t.Fatalf("streams = %d, want 1 — only the selected secondary stream", len(view.Streams))
	}
	if got := view.Streams[0].ParticipantID; got != streamParty {
		t.Errorf("recorded participant = %q, want %q", got, streamParty)
	}
	// The leg's own audio must not have been forked alongside it.
	for _, p := range view.Participants {
		if p.ParticipantID == legID {
			t.Errorf("the leg's primary audio was recorded despite selecting only %q", streamParty)
		}
	}
}

// waitForLegOfType returns the ID of the first leg of the given type.
func waitForLegOfType(t *testing.T, inst *testInstance, legType string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := httpGet(t, inst.baseURL()+"/v1/legs")
		var legs []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		err := json.NewDecoder(resp.Body).Decode(&legs)
		resp.Body.Close()
		if err == nil {
			for _, l := range legs {
				if l.Type == legType {
					return l.ID
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no leg of type %q appeared", legType)
	return ""
}

// --- helpers ---

type siprecParticipantView struct {
	ParticipantID string `json:"participant_id"`
	AOR           string `json:"aor"`
	Name          string `json:"name"`
}

type siprecStreamView struct {
	LegStreamID     string `json:"leg_stream_id"`
	MID             string `json:"mid"`
	Label           string `json:"label"`
	Direction       string `json:"direction"`
	Codec           string `json:"codec"`
	RoomID          string `json:"room_id"`
	Role            string `json:"role"`
	ParticipantID   string `json:"participant_id"`
	ParticipantAOR  string `json:"participant_aor"`
	ParticipantName string `json:"participant_name"`
}

type siprecSessionView struct {
	LegID        string                  `json:"leg_id"`
	SessionID    string                  `json:"session_id"`
	DataMode     string                  `json:"data_mode"`
	RoomID       string                  `json:"room_id"`
	Participants []siprecParticipantView `json:"participants"`
	Streams      []siprecStreamView      `json:"streams"`
	Metadata     string                  `json:"metadata"`
}

func getSIPRECSession(t *testing.T, inst *testInstance, legID string) siprecSessionView {
	t.Helper()
	resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s/siprec", inst.baseURL(), legID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /siprec = %d, want 200", resp.StatusCode)
	}
	var view siprecSessionView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode siprec view: %v", err)
	}
	return view
}

type legTypeView struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func getLegView(t *testing.T, inst *testInstance, legID string) legTypeView {
	t.Helper()
	resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /legs/%s = %d, want 200", legID, resp.StatusCode)
	}
	var view legTypeView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode leg view: %v", err)
	}
	return view
}

// TestSIPREC_SessionSurvivesACK asserts that a recording session stays
// established after it has been answered and ACKed.
//
// Every other SIPREC test asserts on the answer and then hangs up, so none of
// them notice a session that dies a moment later.
//
// This catches an immediate teardown. A client whose ACK never confirms the
// dialog instead trips the 64*T1 retransmit timeout, which this does not wait
// for.
func TestSIPREC_SessionSurvivesACK(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	if call.RemoteSDP == nil {
		t.Fatal("no answer SDP — the SRS never answered the recording session")
	}

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	legIDer, _ := evt.Data.(interface{ GetLegID() string })
	if legIDer == nil || legIDer.GetLegID() == "" {
		t.Fatal("siprec.session_started carries no leg ID")
	}
	legID := legIDer.GetLegID()

	// Hold the dialog open and watch it. A green run has to observe the whole
	// window -- absence of a teardown is only provable by waiting -- but
	// polling makes a real teardown fail at once and report when it happened.
	const observe = 2 * time.Second
	answered := time.Now()
	for deadline := answered.Add(observe); time.Now().Before(deadline); {
		if srs.collector.hasEvent(events.SIPRECSessionEnded, nil) {
			t.Fatalf("recording session ended on its own %v after being answered: "+
				"the dialog was torn down instead of kept up", time.Since(answered).Round(time.Millisecond))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if srs.collector.hasEvent(events.SIPRECSessionEnded, nil) {
		t.Fatalf("recording session ended on its own within %v of being answered: "+
			"the dialog was torn down instead of kept up", observe)
	}

	// The session must still be addressable, not merely un-ended.
	view := getSIPRECSession(t, srs, legID)
	if len(view.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 still attached after the ACK", len(view.Streams))
	}

	// And the leg itself must still be connected — a session view can outlive
	// the SIP dialog that justifies it.
	resp := httpGet(t, srs.baseURL()+"/v1/legs")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /v1/legs = %d, want 200", resp.StatusCode)
	}
	var legs []legView
	decodeJSON(t, resp, &legs)
	var found bool
	for _, l := range legs {
		if l.ID == legID {
			found = true
			if l.State != "connected" {
				t.Errorf("recording leg state = %q, want connected", l.State)
			}
		}
	}
	if !found {
		t.Errorf("recording leg %s is gone from /v1/legs after the ACK", legID)
	}
}
