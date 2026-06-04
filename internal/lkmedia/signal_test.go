package lkmedia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
)

// fakeSignalServer is a minimal LiveKit-compatible signaling server for
// tests. It upgrades the request on /rtc to a WebSocket, sends a canned
// JoinResponse, and then runs a per-test handler against the upgraded
// conn.
type fakeSignalServer struct {
	t       *testing.T
	srv     *httptest.Server
	join    *livekit.JoinResponse
	handler func(t *testing.T, conn net.Conn)

	// gotURL records the URL the client dialed (for assertions on query string).
	gotURL atomic.Value // string
}

func newFakeSignalServer(t *testing.T, join *livekit.JoinResponse, handler func(*testing.T, net.Conn)) *fakeSignalServer {
	t.Helper()
	f := &fakeSignalServer{t: t, join: join, handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/rtc") {
			http.NotFound(w, r)
			return
		}
		f.gotURL.Store(r.URL.String())
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Errorf("ws upgrade: %v", err)
			return
		}
		defer conn.Close()
		if f.join != nil {
			if err := writeServerSignal(conn, &livekit.SignalResponse{
				Message: &livekit.SignalResponse_Join{Join: f.join},
			}); err != nil {
				t.Errorf("send join: %v", err)
				return
			}
		}
		if f.handler != nil {
			f.handler(t, conn)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSignalServer) wsURL() string {
	u, _ := url.Parse(f.srv.URL)
	u.Scheme = "ws"
	return u.String()
}

func (f *fakeSignalServer) dialQuery() url.Values {
	raw, _ := f.gotURL.Load().(string)
	u, _ := url.Parse(raw)
	if u == nil {
		return nil
	}
	return u.Query()
}

func writeServerSignal(conn net.Conn, resp *livekit.SignalResponse) error {
	data, err := proto.Marshal(resp)
	if err != nil {
		return err
	}
	return wsutil.WriteServerBinary(conn, data)
}

func readClientSignal(conn net.Conn) (*livekit.SignalRequest, error) {
	data, op, err := wsutil.ReadClientData(conn)
	if err != nil {
		return nil, err
	}
	if op == ws.OpClose {
		return nil, io.EOF
	}
	if op != ws.OpBinary {
		return nil, errors.New("expected binary frame")
	}
	var req livekit.SignalRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// canned JoinResponse used by most tests.
func canonicalJoin() *livekit.JoinResponse {
	return &livekit.JoinResponse{
		Room: &livekit.Room{Name: "test-room", Sid: "RM_test"},
		Participant: &livekit.ParticipantInfo{
			Identity: "vb-bridge",
			Sid:      "PA_test",
			Name:     "VoiceBlender",
		},
		PingInterval: 0, // disable client pings for predictable tests
		PingTimeout:  60,
		ServerInfo:   &livekit.ServerInfo{Version: "test-fake"},
	}
}

func TestBuildConnectURL(t *testing.T) {
	t.Run("appends /rtc and standard query", func(t *testing.T) {
		u, err := buildConnectURL(SignalConfig{URL: "wss://lk.example.com", Token: "tok"})
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := url.Parse(u)
		if parsed.Path != "/rtc" {
			t.Errorf("path = %q, want /rtc", parsed.Path)
		}
		q := parsed.Query()
		if q.Get("access_token") != "tok" {
			t.Errorf("access_token missing")
		}
		if q.Get("protocol") == "" {
			t.Errorf("protocol missing")
		}
		if q.Get("sdk") != signalSDKName {
			t.Errorf("sdk = %q", q.Get("sdk"))
		}
		if q.Get("auto_subscribe") != "true" {
			t.Errorf("auto_subscribe = %q, want true", q.Get("auto_subscribe"))
		}
	})

	t.Run("overwrites existing path", func(t *testing.T) {
		u, err := buildConnectURL(SignalConfig{URL: "wss://lk.example.com/rtc", Token: "tok"})
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := url.Parse(u)
		if parsed.Path != "/rtc" {
			t.Errorf("path = %q, want /rtc", parsed.Path)
		}
	})

	t.Run("auto_subscribe false when configured", func(t *testing.T) {
		f := false
		u, err := buildConnectURL(SignalConfig{URL: "wss://lk.example.com", Token: "tok", AutoSubscribe: &f})
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := url.Parse(u)
		if parsed.Query().Get("auto_subscribe") != "false" {
			t.Errorf("auto_subscribe = %q, want false", parsed.Query().Get("auto_subscribe"))
		}
	})

	t.Run("rejects http scheme", func(t *testing.T) {
		_, err := buildConnectURL(SignalConfig{URL: "http://lk.example.com", Token: "tok"})
		if err == nil {
			t.Error("expected error for http scheme")
		}
	})

	t.Run("rejects missing host", func(t *testing.T) {
		_, err := buildConnectURL(SignalConfig{URL: "wss://", Token: "tok"})
		if err == nil {
			t.Error("expected error for missing host")
		}
	})
}

func TestRedactToken(t *testing.T) {
	in := "wss://lk.example.com/rtc?access_token=secret&protocol=15"
	out := redactToken(in)
	if strings.Contains(out, "secret") {
		t.Errorf("token leaked: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("REDACTED marker missing: %s", out)
	}
}

func TestConnect_Success(t *testing.T) {
	join := canonicalJoin()
	srv := newFakeSignalServer(t, join, func(t *testing.T, conn net.Conn) {
		// Keep the connection open; let the test drive shutdown.
		_, _ = readClientSignal(conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Connect(ctx, SignalConfig{URL: srv.wsURL(), Token: "jwt-test"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(livekit.DisconnectReason_CLIENT_INITIATED) })

	if got := c.JoinResponse(); got == nil || got.Room.GetName() != "test-room" {
		t.Errorf("JoinResponse = %+v", got)
	}

	q := srv.dialQuery()
	if q.Get("access_token") != "jwt-test" {
		t.Errorf("server saw token = %q", q.Get("access_token"))
	}
	if q.Get("sdk") != signalSDKName {
		t.Errorf("server saw sdk = %q", q.Get("sdk"))
	}
}

func TestConnect_RejectsMissingFields(t *testing.T) {
	cases := []SignalConfig{
		{Token: "tok"},             // missing URL
		{URL: "wss://example.com"}, // missing Token
	}
	for _, cfg := range cases {
		_, err := Connect(context.Background(), cfg)
		if err == nil {
			t.Errorf("Connect(%+v): want error", cfg)
		}
	}
}

func TestConnect_ServerSendsLeaveBeforeJoin(t *testing.T) {
	srv := newFakeSignalServer(t, nil, func(t *testing.T, conn net.Conn) {
		_ = writeServerSignal(conn, &livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{
				Leave: &livekit.LeaveRequest{Reason: livekit.DisconnectReason_JOIN_FAILURE},
			},
		})
		// Block until the client closes so our writes finish flushing
		// before the conn is torn down.
		_, _ = readClientSignal(conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := Connect(ctx, SignalConfig{URL: srv.wsURL(), Token: "tok"})
	if err == nil {
		t.Fatal("expected Connect to fail when server sends Leave first")
	}
	if !strings.Contains(err.Error(), "Leave") {
		t.Errorf("err = %v, expected mention of Leave", err)
	}
}

func TestConnect_BadURL(t *testing.T) {
	const secret = "supersecret-jwt-payload"
	_, err := Connect(context.Background(), SignalConfig{URL: "ws://127.0.0.1:1/rtc", Token: secret})
	if err == nil {
		t.Fatal("expected dial failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("token leaked in error: %v", err)
	}
}

func TestSend_RoundTrip(t *testing.T) {
	join := canonicalJoin()
	type seen struct {
		offer   *livekit.SessionDescription
		answer  *livekit.SessionDescription
		trickle *livekit.TrickleRequest
		addTrk  *livekit.AddTrackRequest
		mute    *livekit.MuteTrackRequest
		leave   *livekit.LeaveRequest
	}
	got := &seen{}
	doneCh := make(chan struct{})

	srv := newFakeSignalServer(t, join, func(t *testing.T, conn net.Conn) {
		defer close(doneCh)
		for {
			req, err := readClientSignal(conn)
			if err != nil {
				return
			}
			switch m := req.Message.(type) {
			case *livekit.SignalRequest_Offer:
				got.offer = m.Offer
			case *livekit.SignalRequest_Answer:
				got.answer = m.Answer
			case *livekit.SignalRequest_Trickle:
				got.trickle = m.Trickle
			case *livekit.SignalRequest_AddTrack:
				got.addTrk = m.AddTrack
			case *livekit.SignalRequest_Mute:
				got.mute = m.Mute
			case *livekit.SignalRequest_Leave:
				got.leave = m.Leave
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Connect(ctx, SignalConfig{URL: srv.wsURL(), Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.SendOffer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\noffer"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SendAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: "v=0\r\nanswer"}); err != nil {
		t.Fatal(err)
	}
	mline := uint16(0)
	if err := c.SendTrickle(webrtc.ICECandidateInit{Candidate: "candidate:foo", SDPMid: strPtr("0"), SDPMLineIndex: &mline}, livekit.SignalTarget_PUBLISHER); err != nil {
		t.Fatal(err)
	}
	if err := c.AddTrack(&livekit.AddTrackRequest{Cid: "cid-1", Name: "voice", Type: livekit.TrackType_AUDIO}); err != nil {
		t.Fatal(err)
	}
	if err := c.MuteTrack("TR_test", true); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(livekit.DisconnectReason_CLIENT_INITIATED)

	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("server handler did not see Leave")
	}

	if got.offer == nil || got.offer.GetSdp() != "v=0\r\noffer" {
		t.Errorf("offer round-trip: %+v", got.offer)
	}
	if got.answer == nil || got.answer.GetSdp() != "v=0\r\nanswer" {
		t.Errorf("answer round-trip: %+v", got.answer)
	}
	if got.trickle == nil || !strings.Contains(got.trickle.GetCandidateInit(), "candidate:foo") {
		t.Errorf("trickle round-trip: %+v", got.trickle)
	}
	if got.trickle == nil || got.trickle.GetTarget() != livekit.SignalTarget_PUBLISHER {
		t.Errorf("trickle target = %v", got.trickle.GetTarget())
	}
	if got.addTrk == nil || got.addTrk.GetCid() != "cid-1" {
		t.Errorf("add track round-trip: %+v", got.addTrk)
	}
	if got.mute == nil || got.mute.GetSid() != "TR_test" || !got.mute.GetMuted() {
		t.Errorf("mute round-trip: %+v", got.mute)
	}
	if got.leave == nil {
		t.Errorf("server did not see Leave")
	}
}

func TestDispatch_ServerEvents(t *testing.T) {
	join := canonicalJoin()
	srv := newFakeSignalServer(t, join, func(t *testing.T, conn net.Conn) {
		// Server sends a sequence of events, then closes.
		mline := int32(0)
		_ = writeServerSignal(conn, &livekit.SignalResponse{
			Message: &livekit.SignalResponse_Offer{Offer: &livekit.SessionDescription{Type: "offer", Sdp: "v=0\r\nserver-offer"}},
		})
		_ = writeServerSignal(conn, &livekit.SignalResponse{
			Message: &livekit.SignalResponse_Trickle{Trickle: &livekit.TrickleRequest{
				CandidateInit: `{"candidate":"candidate:srv","sdpMid":"0","sdpMLineIndex":` + strconvInt(int(mline)) + `}`,
				Target:        livekit.SignalTarget_SUBSCRIBER,
			}},
		})
		_ = writeServerSignal(conn, &livekit.SignalResponse{
			Message: &livekit.SignalResponse_Update{Update: &livekit.ParticipantUpdate{
				Participants: []*livekit.ParticipantInfo{{Identity: "alice", State: livekit.ParticipantInfo_ACTIVE}},
			}},
		})
		_ = writeServerSignal(conn, &livekit.SignalResponse{
			Message: &livekit.SignalResponse_SpeakersChanged{SpeakersChanged: &livekit.SpeakersChanged{
				Speakers: []*livekit.SpeakerInfo{{Sid: "PA_alice", Level: 0.5, Active: true}},
			}},
		})
		_ = writeServerSignal(conn, &livekit.SignalResponse{
			Message: &livekit.SignalResponse_TrackPublished{TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   "cid-1",
				Track: &livekit.TrackInfo{Sid: "TR_alice_audio", Type: livekit.TrackType_AUDIO},
			}},
		})
		_ = writeServerSignal(conn, &livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{Leave: &livekit.LeaveRequest{
				Reason: livekit.DisconnectReason_PARTICIPANT_REMOVED,
			}},
		})
		// Wait for client to ack and close.
		_, _ = readClientSignal(conn)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Connect(ctx, SignalConfig{URL: srv.wsURL(), Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}

	collected := drainEvents(t, c, 6, 2*time.Second)

	want := []string{
		"lkmedia.SignalEventOffer",
		"lkmedia.SignalEventTrickle",
		"lkmedia.SignalEventParticipantUpdate",
		"lkmedia.SignalEventSpeakersChanged",
		"lkmedia.SignalEventTrackPublished",
		"lkmedia.SignalEventLeave",
	}
	if len(collected) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(collected), len(want), collected)
	}
	for i, ev := range collected {
		if !strings.HasSuffix(typeName(ev), want[i]) {
			t.Errorf("event[%d] = %s, want suffix %s", i, typeName(ev), want[i])
		}
	}

	// Inspect a couple of events for content correctness.
	if offer, ok := collected[0].(SignalEventOffer); !ok || offer.SDP.SDP != "v=0\r\nserver-offer" {
		t.Errorf("offer event payload: %+v", collected[0])
	}
	if trickle, ok := collected[1].(SignalEventTrickle); !ok || trickle.Candidate.Candidate != "candidate:srv" {
		t.Errorf("trickle event payload: %+v", collected[1])
	}
	if leave, ok := collected[5].(SignalEventLeave); !ok || leave.Reason != livekit.DisconnectReason_PARTICIPANT_REMOVED {
		t.Errorf("leave event payload: %+v", collected[5])
	}

	// Done() should close after the Leave.
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close after Leave")
	}
	if got := c.CloseReason(); got != "livekit_kicked" {
		t.Errorf("CloseReason = %q, want livekit_kicked", got)
	}
}

func TestClose_Idempotent(t *testing.T) {
	join := canonicalJoin()
	srv := newFakeSignalServer(t, join, func(t *testing.T, conn net.Conn) {
		_, _ = readClientSignal(conn)
	})

	c, err := Connect(context.Background(), SignalConfig{URL: srv.wsURL(), Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(livekit.DisconnectReason_CLIENT_INITIATED); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(livekit.DisconnectReason_CLIENT_INITIATED); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	<-c.Done()
}

func TestLeaveReasonString_CoversEnum(t *testing.T) {
	// Spot-check a few values; the function returns a known tag for each
	// case and a fallback for unknown values.
	cases := map[livekit.DisconnectReason]string{
		livekit.DisconnectReason_CLIENT_INITIATED:    "livekit_client_initiated",
		livekit.DisconnectReason_PARTICIPANT_REMOVED: "livekit_kicked",
		livekit.DisconnectReason_ROOM_DELETED:        "livekit_room_deleted",
		livekit.DisconnectReason_CONNECTION_TIMEOUT:  "livekit_token_expired",
		livekit.DisconnectReason(9999):               "livekit_disconnected",
	}
	for r, want := range cases {
		if got := leaveReasonString(r); got != want {
			t.Errorf("leaveReasonString(%v) = %q, want %q", r, got, want)
		}
	}
}

// drainEvents reads up to n events from c.Events() or fails the test on timeout.
func drainEvents(t *testing.T, c *SignalClient, n int, timeout time.Duration) []SignalEvent {
	t.Helper()
	out := make([]SignalEvent, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timeout after %d events (wanted %d)", len(out), n)
		}
	}
	return out
}

func typeName(v interface{}) string { return fmt.Sprintf("%T", v) }

func strPtr(s string) *string { return &s }

func strconvInt(i int) string { return strconv.Itoa(i) }
