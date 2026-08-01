//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
)

func multiStreamInstance(t *testing.T, name string) *testInstance {
	return newTestInstanceWithOpts(t, name, func(c *config.Config) {
		c.SIPMultiStreamEnabled = true
		c.SIPMultiStreamMax = 4
	})
}

// dialMultiStream places an outbound INVITE offering the given audio sections
// from the start and returns the resulting call. It drives the engine directly
// because the REST create-leg path always offers a single section; adding one
// later goes through POST /v1/legs/{id}/streams instead.
func dialMultiStream(t *testing.T, from, to *testInstance, streams []sipmod.OfferStream) *sipmod.OutboundCall {
	t.Helper()
	recipient := sip.Uri{User: "test", Host: "127.0.0.1", Port: to.sipPort}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	var call *sipmod.OutboundCall
	var err error
	go func() {
		defer close(done)
		call, err = from.engine.Invite(ctx, recipient, sipmod.InviteOptions{Streams: streams})
	}()

	inbound := waitForInboundLeg(t, to.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", to.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("INVITE did not complete")
	}
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	return call
}

// TestMultiStream_OfferTwoAudioLines_Answered is the end-to-end shape of the
// translation use case: m-line 0 carries the original bidirectional audio and
// m-line 1 a sendonly translated feed, each on its own port.
func TestMultiStream_OfferTwoAudioLines_Answered(t *testing.T) {
	instA := multiStreamInstance(t, "instance-a")
	instB := multiStreamInstance(t, "instance-b")

	call := dialMultiStream(t, instA, instB, []sipmod.OfferStream{
		{Content: "main", Lang: "en"},
		{Direction: sipmod.DirSendOnly, Content: "alt", Lang: "es"},
	})
	defer call.Dialog.Bye(context.Background())

	if len(call.OfferedStreams) != 2 {
		t.Fatalf("offered %d audio sections, want 2", len(call.OfferedStreams))
	}
	if len(call.ExtraRTPSess) != 1 {
		t.Fatalf("allocated %d extra sockets, want 1", len(call.ExtraRTPSess))
	}
	if call.RTPSess.LocalPort() == call.ExtraRTPSess[0].LocalPort() {
		t.Error("streams must bind distinct ports — a shared transport is undefined without BUNDLE")
	}

	// RFC 3264 §6: the answer carries one section per offered section.
	if got := len(call.RemoteSDP.Audio); got != 2 {
		t.Fatalf("answer has %d audio sections, want 2", got)
	}
	for i, a := range call.RemoteSDP.Audio {
		if a.RemotePort == 0 {
			t.Errorf("section %d was rejected; the answerer has multi-stream enabled", i)
		}
	}
	// Our sendonly stream must come back recvonly (RFC 3264 §6.1).
	if got := call.RemoteSDP.Audio[1].Direction; got != sipmod.DirRecvOnly {
		t.Errorf("second section answered %q, want recvonly", got)
	}
	if got := call.RemoteSDP.Audio[1].Lang; got != "es" {
		t.Errorf("second section lang = %q, want es echoed back", got)
	}

	// The answering side must have materialized two streams on its leg.
	inbound := findSIPLeg(t, instB, func(l *leg.SIPLeg) bool { return l.StreamCount() == 2 })
	if inbound == nil {
		t.Error("answering leg did not materialize two media streams")
	}
}

// TestMultiStream_PeerRejectsExtraStreamCallSurvives pins the graceful
// degradation path: B has multi-stream disabled, so it answers the extra
// section with port 0 and the call continues on the primary stream alone.
func TestMultiStream_PeerRejectsExtraStreamCallSurvives(t *testing.T) {
	instA := multiStreamInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b") // multi-stream off (the default)

	call := dialMultiStream(t, instA, instB, []sipmod.OfferStream{
		{Content: "main", Lang: "en"},
		{Direction: sipmod.DirSendOnly, Content: "alt", Lang: "es"},
	})
	defer call.Dialog.Bye(context.Background())

	if got := len(call.RemoteSDP.Audio); got != 2 {
		t.Fatalf("answer has %d audio sections, want 2 — the count must match the offer", got)
	}
	if call.RemoteSDP.Audio[0].RemotePort == 0 {
		t.Error("the primary section must survive")
	}
	if call.RemoteSDP.Audio[1].RemotePort != 0 {
		t.Error("the extra section must be rejected with port 0 when multi-stream is disabled")
	}

	// The call itself is up and single-stream.
	inbound := findSIPLeg(t, instB, func(l *leg.SIPLeg) bool { return l.StreamCount() == 1 })
	if inbound == nil {
		t.Error("answering leg should be running exactly one stream")
	}
}

// TestMultiStream_DisabledEmitsSingleMLine pins that the feature is genuinely
// off by default: a plain call still negotiates exactly one m=audio section.
func TestMultiStream_DisabledEmitsSingleMLine(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	outboundID, inboundID := establishCall(t, instA, instB)
	_ = inboundID

	l, ok := instA.legMgr.Get(outboundID)
	if !ok {
		t.Fatal("outbound leg not found")
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		t.Fatal("expected a SIP leg")
	}
	if got := sl.StreamCount(); got != 1 {
		t.Errorf("stream count = %d, want 1 with multi-stream disabled", got)
	}
}

// TestMultiStream_RejectedWhenDisabledOnOfferer keeps the gate honest on the
// offering side too.
func TestMultiStream_RejectedWhenDisabledOnOfferer(t *testing.T) {
	instA := newTestInstance(t, "instance-a") // multi-stream off
	instB := multiStreamInstance(t, "instance-b")

	recipient := sip.Uri{User: "test", Host: "127.0.0.1", Port: instB.sipPort}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := instA.engine.Invite(ctx, recipient, sipmod.InviteOptions{
		Streams: []sipmod.OfferStream{{}, {Direction: sipmod.DirSendOnly}},
	})
	if err == nil {
		t.Fatal("want an error when offering multiple streams with the gate off")
	}
	if !strings.Contains(err.Error(), "SIP_MULTI_STREAM_ENABLED") {
		t.Errorf("error = %v, want it to name the disabled gate", err)
	}
}

// TestMultiStream_TranslationTopology is the shape the feature exists for: the
// leg's original audio is mixed in room A while its translated stream is mixed
// in room B, and neither hears the other.
func TestMultiStream_TranslationTopology(t *testing.T) {
	instA := multiStreamInstance(t, "instance-a")
	instB := multiStreamInstance(t, "instance-b")

	call := dialMultiStream(t, instA, instB, []sipmod.OfferStream{
		{Content: "main", Lang: "en"},
		{Direction: sipmod.DirSendOnly, Content: "alt", Lang: "es"},
	})
	defer call.Dialog.Bye(context.Background())

	sl := findSIPLeg(t, instB, func(l *leg.SIPLeg) bool { return l.StreamCount() == 2 })
	if sl == nil {
		t.Fatal("answering leg did not materialize two media streams")
	}
	secondary := sl.SecondaryStreamIDs()
	if len(secondary) != 1 {
		t.Fatalf("secondary stream IDs = %v, want one", secondary)
	}

	roomA, err := instB.roomMgr.Create("room-original", "", 16000)
	if err != nil {
		t.Fatalf("create room A: %v", err)
	}
	roomB, err := instB.roomMgr.Create("room-translated", "", 16000)
	if err != nil {
		t.Fatalf("create room B: %v", err)
	}

	roomA.AddLeg(sl)
	// StreamCount goes up when the section is negotiated; the media pipeline
	// starts a moment later, so wait for the stream to actually carry audio.
	waitForStreamMedia(t, sl, secondary[0])
	if _, ok := roomB.AddLegStream(sl, secondary[0], "translator"); !ok {
		t.Fatal("AddLegStream failed")
	}

	// The leg's own room is unchanged; only the stream moved.
	if got := sl.RoomID(); got != "room-original" {
		t.Errorf("leg room = %q, want room-original", got)
	}
	if got := sl.StreamRooms()[secondary[0]]; got != "room-translated" {
		t.Errorf("stream room = %q, want room-translated", got)
	}
	wantPID := room.StreamParticipantID(sl.ID(), secondary[0])
	if ids := roomB.LegStreamIDs(); len(ids) != 1 || ids[0] != wantPID {
		t.Errorf("room B stream participants = %v, want [%s]", ids, wantPID)
	}
	if ids := roomA.LegStreamIDs(); len(ids) != 0 {
		t.Errorf("room A must hold no streams, got %v", ids)
	}

	// Tearing the leg down must detach the stream from the room it was parked
	// in, which removing the leg from its own room would never reach.
	resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), sl.ID()))
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(roomB.LegStreamIDs()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("cross-room stream still attached after leg teardown: %v", roomB.LegStreamIDs())
}

type legStreamView struct {
	ID        string `json:"id"`
	MID       string `json:"mid"`
	Index     int    `json:"index"`
	Primary   bool   `json:"primary"`
	State     string `json:"state"`
	Direction string `json:"direction"`
	Codec     string `json:"codec"`
	LocalPort int    `json:"local_port"`
	Content   string `json:"content"`
	Lang      string `json:"lang"`
	RoomID    string `json:"room_id"`
	Role      string `json:"role"`
}

// TestMultiStream_RESTAddAttachRemove drives the whole per-leg stream API over
// HTTP: add a translated stream to a live call with a re-INVITE, park it in its
// own room, then remove it.
func TestMultiStream_RESTAddAttachRemove(t *testing.T) {
	instA := multiStreamInstance(t, "instance-a")
	instB := multiStreamInstance(t, "instance-b")

	outboundID, _ := establishCall(t, instA, instB)

	// A single-stream call starts with exactly the primary stream.
	resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), outboundID))
	var streams []legStreamView
	decodeJSON(t, resp, &streams)
	if len(streams) != 1 || !streams[0].Primary || streams[0].ID != "0" {
		t.Fatalf("initial streams = %+v, want just the primary", streams)
	}

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"id": "room-translated"})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	roomResp.Body.Close()

	addResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), outboundID), map[string]interface{}{
		"direction": "sendonly",
		"content":   "alt",
		"lang":      "es",
		"room_id":   "room-translated",
		"role":      "translator",
	})
	if addResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(addResp.Body)
		t.Fatalf("add stream: unexpected status %d: %s", addResp.StatusCode, b)
	}
	var added legStreamView
	decodeJSON(t, addResp, &added)

	if added.Primary || added.Index != 1 {
		t.Errorf("added stream = %+v, want a secondary at m-line 1", added)
	}
	if added.Direction != "sendonly" || added.Lang != "es" || added.Content != "alt" {
		t.Errorf("added stream attributes = %+v", added)
	}
	if added.RoomID != "room-translated" || added.Role != "translator" {
		t.Errorf("added stream room/role = %q/%q, want room-translated/translator", added.RoomID, added.Role)
	}
	if added.LocalPort == 0 || added.LocalPort == streams[0].LocalPort {
		t.Errorf("stream port = %d, want its own port distinct from the primary's %d",
			added.LocalPort, streams[0].LocalPort)
	}

	// The peer accepted a second section, so its leg carries two streams too.
	if findSIPLeg(t, instB, func(l *leg.SIPLeg) bool { return l.StreamCount() == 2 }) == nil {
		t.Error("the answering side did not materialize the added stream")
	}

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/streams/%s", instA.baseURL(), outboundID, added.ID))
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove stream: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// The slot survives as a tombstone (RFC 3264 §8.2), so the stream list is
	// back to the primary alone but a future add would take index 2.
	resp = httpGet(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), outboundID))
	decodeJSON(t, resp, &streams)
	if len(streams) != 1 || !streams[0].Primary {
		t.Errorf("streams after removal = %+v, want just the primary", streams)
	}
}

// TestMultiStream_RESTCreateWithStreams establishes a two-stream call from the
// very first INVITE — no follow-up re-INVITE — and parks the translated feed in
// its own room.
func TestMultiStream_RESTCreateWithStreams(t *testing.T) {
	instA := multiStreamInstance(t, "instance-a")
	instB := multiStreamInstance(t, "instance-b")

	for _, id := range []string{"room-original", "room-translated"} {
		resp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"id": id})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create room %s: unexpected status %d", id, resp.StatusCode)
		}
		resp.Body.Close()
	}

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"to":      fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":  []string{"PCMU"},
		"room_id": "room-original",
		"streams": []map[string]interface{}{
			{"direction": "sendonly", "content": "alt", "lang": "es",
				"room_id": "room-translated", "role": "translator"},
		},
	})
	if createResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create leg: unexpected status %d: %s", createResp.StatusCode, b)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	answerResp.Body.Close()
	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)

	// Both sections were negotiated by the initial offer/answer.
	var streams []legStreamView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), outbound.ID))
		decodeJSON(t, resp, &streams)
		if len(streams) == 2 && streams[1].RoomID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %+v, want 2 from the initial INVITE", streams)
	}

	if !streams[0].Primary || streams[0].RoomID != "room-original" {
		t.Errorf("primary stream = %+v, want the leg's own room", streams[0])
	}
	extra := streams[1]
	if extra.Primary || extra.Index != 1 {
		t.Errorf("second stream = %+v, want a secondary at m-line 1", extra)
	}
	if extra.Direction != "sendonly" || extra.Content != "alt" || extra.Lang != "es" {
		t.Errorf("second stream attributes = %+v", extra)
	}
	if extra.RoomID != "room-translated" || extra.Role != "translator" {
		t.Errorf("second stream room/role = %q/%q, want room-translated/translator", extra.RoomID, extra.Role)
	}
	if extra.LocalPort == 0 || extra.LocalPort == streams[0].LocalPort {
		t.Errorf("stream ports = %d and %d, want distinct non-zero ports",
			streams[0].LocalPort, extra.LocalPort)
	}

	// The answering side accepted both sections too.
	if findSIPLeg(t, instB, func(l *leg.SIPLeg) bool { return l.StreamCount() == 2 }) == nil {
		t.Error("the answering side did not materialize both streams")
	}
}

func TestMultiStream_RESTCreateWithStreamsValidation(t *testing.T) {
	enabled := multiStreamInstance(t, "instance-enabled")
	disabled := newTestInstance(t, "instance-disabled")
	target := fmt.Sprintf("sip:test@127.0.0.1:%d", enabled.sipPort)

	cases := []struct {
		name   string
		inst   *testInstance
		body   map[string]interface{}
		status int
	}{
		{"gate off", disabled, map[string]interface{}{
			"type": "sip", "to": target,
			"streams": []map[string]interface{}{{"direction": "sendonly"}},
		}, http.StatusConflict},
		{"past the cap", enabled, map[string]interface{}{
			"type": "sip", "to": target,
			"streams": []map[string]interface{}{
				{}, {}, {}, {}, // 4 extras + primary = 5, cap is 4
			},
		}, http.StatusConflict},
		{"bad direction", enabled, map[string]interface{}{
			"type": "sip", "to": target,
			"streams": []map[string]interface{}{{"direction": "sideways"}},
		}, http.StatusBadRequest},
		{"unknown room", enabled, map[string]interface{}{
			"type": "sip", "to": target,
			"streams": []map[string]interface{}{{"room_id": "nope"}},
		}, http.StatusNotFound},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := httpPost(t, c.inst.baseURL()+"/v1/legs", c.body)
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status %d, want %d: %s", resp.StatusCode, c.status, b)
			}
		})
	}
}

// TestMultiStream_AnswerWithStreamRooms routes the caller's translated stream
// into its own room at answer time, in the same request that answers the call.
func TestMultiStream_AnswerWithStreamRooms(t *testing.T) {
	instA := multiStreamInstance(t, "instance-a")
	instB := multiStreamInstance(t, "instance-b")

	for _, id := range []string{"room-original", "room-translated"} {
		resp := httpPost(t, instB.baseURL()+"/v1/rooms", map[string]interface{}{"id": id})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create room %s: unexpected status %d", id, resp.StatusCode)
		}
		resp.Body.Close()
	}

	recipient := sip.Uri{User: "test", Host: "127.0.0.1", Port: instB.sipPort}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	var call *sipmod.OutboundCall
	var inviteErr error
	go func() {
		defer close(done)
		call, inviteErr = instA.engine.Invite(ctx, recipient, sipmod.InviteOptions{Streams: []sipmod.OfferStream{
			{Content: "main", Lang: "en"},
			{Direction: sipmod.DirSendOnly, Content: "alt", Lang: "es"},
		}})
	}()

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
		map[string]interface{}{
			"streams": []map[string]interface{}{
				{"room_id": "room-translated", "role": "translator"},
			},
		})
	if answerResp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(answerResp.Body)
		t.Fatalf("answer: unexpected status %d: %s", answerResp.StatusCode, b)
	}
	answerResp.Body.Close()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("INVITE did not complete")
	}
	if inviteErr != nil {
		t.Fatalf("Invite: %v", inviteErr)
	}
	defer call.Dialog.Bye(context.Background())

	// The answering leg's secondary stream lands in the room the answer named.
	var streams []legStreamView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s/streams", instB.baseURL(), inbound.ID))
		decodeJSON(t, resp, &streams)
		if len(streams) == 2 && streams[1].RoomID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %+v, want 2", streams)
	}
	if streams[1].RoomID != "room-translated" || streams[1].Role != "translator" {
		t.Errorf("second stream room/role = %q/%q, want room-translated/translator",
			streams[1].RoomID, streams[1].Role)
	}
	// The primary is untouched: the answer placed only the secondary.
	if streams[0].RoomID != "" {
		t.Errorf("primary stream room = %q, want empty", streams[0].RoomID)
	}
}

// TestMultiStream_AddLegToRoomWithStreams puts a leg and one of its secondary
// streams into the same room in a single request.
func TestMultiStream_AddLegToRoomWithStreams(t *testing.T) {
	instA := multiStreamInstance(t, "instance-a")
	instB := multiStreamInstance(t, "instance-b")

	outboundID, _ := establishCall(t, instA, instB)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), outboundID),
		map[string]interface{}{"direction": "sendonly", "content": "alt", "lang": "es"})
	if addResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(addResp.Body)
		t.Fatalf("add stream: unexpected status %d: %s", addResp.StatusCode, b)
	}
	var added legStreamView
	decodeJSON(t, addResp, &added)
	if added.RoomID != "" {
		t.Fatalf("stream started in room %q, want unattached", added.RoomID)
	}

	joinResp := httpPost(t, instA.baseURL()+"/v1/rooms/room-shared/legs", map[string]interface{}{
		"leg_id":  outboundID,
		"streams": []map[string]interface{}{{"stream_id": added.ID, "role": "translator"}},
	})
	if joinResp.StatusCode != http.StatusOK && joinResp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(joinResp.Body)
		t.Fatalf("add leg to room: unexpected status %d: %s", joinResp.StatusCode, b)
	}
	joinResp.Body.Close()

	var streams []legStreamView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), outboundID))
		decodeJSON(t, resp, &streams)
		if len(streams) == 2 && streams[1].RoomID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(streams) != 2 {
		t.Fatalf("streams = %+v, want 2", streams)
	}
	// Leg and stream both landed in the room named by the request.
	if streams[0].RoomID != "room-shared" || streams[1].RoomID != "room-shared" {
		t.Errorf("rooms = %q / %q, want both room-shared", streams[0].RoomID, streams[1].RoomID)
	}
	if streams[1].Role != "translator" {
		t.Errorf("stream role = %q, want translator", streams[1].Role)
	}
}

func TestMultiStream_AddLegToRoomStreamValidation(t *testing.T) {
	instA := multiStreamInstance(t, "instance-a")
	instB := multiStreamInstance(t, "instance-b")

	outboundID, _ := establishCall(t, instA, instB)

	cases := []struct {
		name   string
		body   map[string]interface{}
		status int
	}{
		{"unknown stream", map[string]interface{}{
			"leg_id": outboundID, "streams": []map[string]interface{}{{"stream_id": "nope"}},
		}, http.StatusNotFound},
		{"missing stream_id", map[string]interface{}{
			"leg_id": outboundID, "streams": []map[string]interface{}{{"role": "x"}},
		}, http.StatusBadRequest},
		{"primary not addressable", map[string]interface{}{
			"leg_id": outboundID, "streams": []map[string]interface{}{{"stream_id": "0"}},
		}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := httpPost(t, instA.baseURL()+"/v1/rooms/room-x/legs", c.body)
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status %d, want %d: %s", resp.StatusCode, c.status, b)
			}
		})
	}
}

// TestMultiStream_RESTAddRejectedWhenDisabled keeps the gate honest at the API.
func TestMultiStream_RESTAddRejectedWhenDisabled(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), outboundID), map[string]interface{}{
		"direction": "sendonly",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("add stream with the gate off: status %d, want 409", resp.StatusCode)
	}
}

// waitForStreamMedia blocks until a stream's media endpoints are wired up.
func waitForStreamMedia(t *testing.T, sl *leg.SIPLeg, streamID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sm, ok := sl.StreamMedia(streamID); ok && (sm.Reader != nil || sm.Writer != nil) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stream %s never came up", streamID)
}

// findSIPLeg returns the first SIP leg on inst matching pred, polling briefly
// because leg setup races the INVITE completing.
func findSIPLeg(t *testing.T, inst *testInstance, pred func(*leg.SIPLeg) bool) *leg.SIPLeg {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range inst.legMgr.List() {
			if sl, ok := l.(*leg.SIPLeg); ok && pred(sl) {
				return sl
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}
