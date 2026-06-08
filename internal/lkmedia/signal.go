// Package lkmedia implements the LiveKit signaling protocol and media
// transport for VoiceBlender's livekit_room leg type.
//
// The signaling client (this file) speaks the LiveKit signaling protocol
// directly over a WebSocket using only the protobuf message types from
// github.com/livekit/protocol — no LiveKit SDK is imported. This keeps the
// pion stack pinned to VoiceBlender's own versions.
package lkmedia

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
)

// signalProtocolVersion is the LiveKit signaling protocol version we
// negotiate. Bumping this should be coordinated with the server we test
// against (see livekit-server release notes).
const signalProtocolVersion = 15

// signalSDKName identifies VoiceBlender in the LiveKit server logs.
const signalSDKName = "voiceblender"

// signalSDKVersion is sent on the connect query string.
const signalSDKVersion = "1.0"

// SignalConfig configures the WebSocket dial.
type SignalConfig struct {
	// URL is the LiveKit server endpoint, e.g. "wss://lk.example.com" or
	// "ws://localhost:7880". The "/rtc" path is appended automatically;
	// any existing path is overwritten.
	URL string
	// Token is the LiveKit JWT carried on the access_token query string.
	Token string
	// AutoSubscribe, when true (the default), asks the server to subscribe
	// the participant to all published tracks. Set false only for advanced
	// flows that drive UpdateSubscription manually.
	AutoSubscribe *bool
	// Log receives debug/info diagnostics; defaults to slog.Default().
	Log *slog.Logger
}

// SignalEvent is the closed sum type of typed events emitted on
// SignalClient.Events.
type SignalEvent interface{ isSignalEvent() }

// SignalEventOffer is the server's SDP offer (server→client). Client must
// call SetRemoteDescription then send an Answer.
type SignalEventOffer struct {
	SDP          webrtc.SessionDescription
	ID           uint32
	MidToTrackID map[string]string
}

// SignalEventAnswer is the server's SDP answer to our publisher offer.
type SignalEventAnswer struct {
	SDP          webrtc.SessionDescription
	ID           uint32
	MidToTrackID map[string]string
}

// SignalEventTrickle is a remote ICE candidate from the server.
type SignalEventTrickle struct {
	Candidate webrtc.ICECandidateInit
	Target    livekit.SignalTarget
	Final     bool
}

// SignalEventParticipantUpdate is a participant joined/left/state-changed update.
type SignalEventParticipantUpdate struct {
	Participants []*livekit.ParticipantInfo
}

// SignalEventTrackPublished is the server's ack of an AddTrack we sent —
// the Track.Sid is now bound to our CID.
type SignalEventTrackPublished struct {
	CID   string
	Track *livekit.TrackInfo
}

// SignalEventTrackUnpublished is the server notifying a track was removed.
type SignalEventTrackUnpublished struct {
	TrackSID string
}

// SignalEventSpeakersChanged carries the active-speaker list.
type SignalEventSpeakersChanged struct {
	Speakers []*livekit.SpeakerInfo
}

// SignalEventConnectionQuality is per-participant connection quality info.
type SignalEventConnectionQuality struct {
	Updates []*livekit.ConnectionQualityInfo
}

// SignalEventMute is a forced mute/unmute issued by the server for a local track.
type SignalEventMute struct {
	SID   string
	Muted bool
}

// SignalEventLeave is sent when the server tells us to leave.
type SignalEventLeave struct {
	Reason       livekit.DisconnectReason
	CanReconnect bool
}

// SignalEventRefreshToken carries an updated JWT (server-issued) for
// long-lived sessions. We persist it but do not auto-reconnect in v1.
type SignalEventRefreshToken struct {
	Token string
}

func (SignalEventOffer) isSignalEvent()             {}
func (SignalEventAnswer) isSignalEvent()            {}
func (SignalEventTrickle) isSignalEvent()           {}
func (SignalEventParticipantUpdate) isSignalEvent() {}
func (SignalEventTrackPublished) isSignalEvent()    {}
func (SignalEventTrackUnpublished) isSignalEvent()  {}
func (SignalEventSpeakersChanged) isSignalEvent()   {}
func (SignalEventConnectionQuality) isSignalEvent() {}
func (SignalEventMute) isSignalEvent()              {}
func (SignalEventLeave) isSignalEvent()             {}
func (SignalEventRefreshToken) isSignalEvent()      {}

// SignalClient is a connected LiveKit signaling session. Methods are safe
// for concurrent use; one client manages one WebSocket.
type SignalClient struct {
	cfg  SignalConfig
	log  *slog.Logger
	conn net.Conn
	join *livekit.JoinResponse
	url  string

	writeMu sync.Mutex // serializes Send* calls

	events chan SignalEvent
	done   chan struct{}

	cancel       context.CancelFunc
	closed       atomic.Bool
	closeErr     atomic.Pointer[error]
	closeReason  atomic.Pointer[string]
	lastPingTSNs atomic.Int64 // for RTT calculation on next Ping

	// preJoin buffers signaling frames that arrive before the JoinResponse
	// (LK can send Offer / RefreshToken / TrickleICE first). recvLoop
	// dispatches these before reading the WS.
	preJoin []*livekit.SignalResponse
}

// Connect dials the LiveKit signaling WebSocket and completes the JOIN
// handshake. On success it returns a client that is already pumping
// SignalResponse messages into Events(). On failure it returns an error
// and no goroutines have been started.
func Connect(ctx context.Context, cfg SignalConfig) (*SignalClient, error) {
	if cfg.URL == "" {
		return nil, errors.New("lkmedia: URL is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("lkmedia: Token is required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "lkmedia.signal")

	dialURL, err := buildConnectURL(cfg)
	if err != nil {
		return nil, fmt.Errorf("build URL: %w", err)
	}

	dialer := ws.Dialer{}
	conn, br, _, err := dialer.Dial(ctx, dialURL)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", redactToken(dialURL), err)
	}
	// gobwas may have buffered bytes BEYOND the HTTP handshake into br
	// (LiveKit sends JoinResponse immediately after the 101 response, in
	// the same TCP segment). If we ignore br, those bytes are gone — and
	// the next wsutil.ReadServerData(conn) blocks forever waiting for
	// data the server already sent. Wrap conn so reads go through br
	// first, then fall through to the raw socket.
	if br != nil && br.Buffered() > 0 {
		conn = &bufferedConn{Conn: conn, r: br}
	}

	c := &SignalClient{
		cfg:    cfg,
		log:    log,
		conn:   conn,
		url:    dialURL,
		events: make(chan SignalEvent, 64),
		done:   make(chan struct{}),
	}

	// Read first frame — must be JoinResponse.
	join, err := c.readJoin(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	c.join = join

	loopCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	stopWatch := wsutilx.WatchCancel(loopCtx, conn)

	go c.recvLoop(loopCtx, stopWatch)
	if join.PingInterval > 0 {
		go c.pingLoop(loopCtx, time.Duration(join.PingInterval)*time.Second)
	}

	log.Info("livekit signaling connected",
		"room", join.Room.GetName(),
		"identity", join.Participant.GetIdentity(),
		"server_version", join.ServerInfo.GetVersion(),
		"ping_interval_s", join.PingInterval,
		"ping_timeout_s", join.PingTimeout,
	)

	return c, nil
}

// buildConnectURL constructs the LiveKit /rtc URL with the standard query
// parameters. The token is carried in the URL (the LiveKit convention) —
// not in an Authorization header.
func buildConnectURL(cfg SignalConfig) (string, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("scheme %q: must be ws or wss", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("URL missing host")
	}
	u.Path = "/rtc"
	q := u.Query()
	q.Set("access_token", cfg.Token)
	q.Set("protocol", strconv.Itoa(signalProtocolVersion))
	q.Set("sdk", signalSDKName)
	q.Set("version", signalSDKVersion)
	autoSub := true
	if cfg.AutoSubscribe != nil {
		autoSub = *cfg.AutoSubscribe
	}
	q.Set("auto_subscribe", strconv.FormatBool(autoSub))
	q.Set("adaptive_stream", "false")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// readJoin reads frames from the WebSocket until a SignalResponse{Join}
// arrives. LiveKit does not guarantee Join is the first frame on the WS:
// in subscriber-primary mode the server starts the subscriber-PC SDP
// offer immediately after participant admission, so Offer / RefreshToken
// / Trickle may interleave with — and occasionally precede — Join.
// Any non-Join frame is buffered into c.preJoin so the recv loop can
// dispatch it after the handshake completes.
func (c *SignalClient) readJoin(ctx context.Context) (*livekit.JoinResponse, error) {
	// Bound the total wait so a server that accepts the TCP handshake but
	// never sends Join cannot hang us indefinitely.
	deadline := time.Now().Add(15 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = c.conn.SetReadDeadline(deadline)
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	for {
		resp, err := c.readMessage()
		if err != nil {
			return nil, fmt.Errorf("read JoinResponse: %w", err)
		}
		switch m := resp.Message.(type) {
		case *livekit.SignalResponse_Join:
			if m.Join == nil || m.Join.Participant == nil {
				return nil, errors.New("JoinResponse missing participant")
			}
			return m.Join, nil
		case *livekit.SignalResponse_Leave:
			return nil, fmt.Errorf("server sent Leave before Join: reason=%s", m.Leave.GetReason())
		default:
			// Buffer for the recv loop to dispatch after Join.
			c.preJoin = append(c.preJoin, resp)
		}
	}
}

// readMessage reads one binary frame and unmarshals it as a SignalResponse.
func (c *SignalClient) readMessage() (*livekit.SignalResponse, error) {
	data, op, err := wsutil.ReadServerData(c.conn)
	if err != nil {
		return nil, err
	}
	if op == ws.OpClose {
		return nil, io.EOF
	}
	if op != ws.OpBinary {
		return nil, fmt.Errorf("expected binary frame, got op=%d", op)
	}
	var resp livekit.SignalResponse
	if err := proto.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("proto unmarshal: %w", err)
	}
	return &resp, nil
}

// recvLoop is the WebSocket read goroutine. It runs until the connection
// closes (server-side leave, network error, or local ctx cancel via
// WatchCancel) and then signals shutdown via c.done.
func (c *SignalClient) recvLoop(ctx context.Context, stopWatch func()) {
	defer stopWatch()
	defer c.shutdown()

	// Dispatch any pre-Join frames the handshake buffered (Offer/Trickle
	// /RefreshToken from LK that arrived before JoinResponse). Drain in
	// order so the publisher/subscriber PCs see a coherent stream.
	for _, resp := range c.preJoin {
		if !c.dispatch(ctx, resp) {
			return
		}
	}
	c.preJoin = nil

	for {
		wsutilx.SetReadDeadline(c.conn, wsutilx.DefaultReadTimeout.Load())

		resp, err := c.readMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.setCloseErr(nil, "livekit_signal_closed")
			} else {
				c.setCloseErr(err, "livekit_signal_error")
				c.log.Debug("signal recv error", "error", err)
			}
			return
		}
		if !c.dispatch(ctx, resp) {
			return
		}
	}
}

// dispatch converts a SignalResponse into a typed SignalEvent and emits
// it. Returns false if the loop should terminate (e.g. Leave received).
func (c *SignalClient) dispatch(ctx context.Context, resp *livekit.SignalResponse) bool {
	switch m := resp.Message.(type) {
	case *livekit.SignalResponse_Answer:
		c.emit(ctx, SignalEventAnswer{
			SDP:          webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Answer.GetSdp()},
			ID:           m.Answer.GetId(),
			MidToTrackID: m.Answer.GetMidToTrackId(),
		})
	case *livekit.SignalResponse_Offer:
		c.emit(ctx, SignalEventOffer{
			SDP:          webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.Offer.GetSdp()},
			ID:           m.Offer.GetId(),
			MidToTrackID: m.Offer.GetMidToTrackId(),
		})
	case *livekit.SignalResponse_Trickle:
		init, err := parseICECandidateInit(m.Trickle.GetCandidateInit())
		if err != nil {
			c.log.Debug("trickle parse error", "error", err)
			return true
		}
		c.emit(ctx, SignalEventTrickle{
			Candidate: init,
			Target:    m.Trickle.GetTarget(),
			Final:     m.Trickle.GetFinal(),
		})
	case *livekit.SignalResponse_Update:
		c.emit(ctx, SignalEventParticipantUpdate{Participants: m.Update.GetParticipants()})
	case *livekit.SignalResponse_TrackPublished:
		c.emit(ctx, SignalEventTrackPublished{
			CID:   m.TrackPublished.GetCid(),
			Track: m.TrackPublished.GetTrack(),
		})
	case *livekit.SignalResponse_TrackUnpublished:
		c.emit(ctx, SignalEventTrackUnpublished{TrackSID: m.TrackUnpublished.GetTrackSid()})
	case *livekit.SignalResponse_SpeakersChanged:
		c.emit(ctx, SignalEventSpeakersChanged{Speakers: m.SpeakersChanged.GetSpeakers()})
	case *livekit.SignalResponse_ConnectionQuality:
		c.emit(ctx, SignalEventConnectionQuality{Updates: m.ConnectionQuality.GetUpdates()})
	case *livekit.SignalResponse_Mute:
		c.emit(ctx, SignalEventMute{SID: m.Mute.GetSid(), Muted: m.Mute.GetMuted()})
	case *livekit.SignalResponse_RefreshToken:
		c.emit(ctx, SignalEventRefreshToken{Token: m.RefreshToken})
	case *livekit.SignalResponse_Leave:
		reason := m.Leave.GetReason()
		c.emit(ctx, SignalEventLeave{Reason: reason, CanReconnect: m.Leave.GetCanReconnect()})
		c.setCloseErr(nil, leaveReasonString(reason))
		return false
	case *livekit.SignalResponse_Pong:
		// Legacy pong (just a timestamp echo); used for RTT but otherwise inert.
	case *livekit.SignalResponse_PongResp:
		if m.PongResp != nil {
			sent := m.PongResp.GetLastPingTimestamp()
			if sent > 0 {
				c.lastPingTSNs.Store(time.Now().UnixMilli() - sent)
			}
		}
	default:
		c.log.Debug("ignored signal response", "type", fmt.Sprintf("%T", resp.Message))
	}
	return true
}

// emit pushes an event to Events. If the consumer is too slow we drop and
// log — the recv loop must not stall on a back-pressured consumer because
// that would also stall the WS read pump.
func (c *SignalClient) emit(ctx context.Context, ev SignalEvent) {
	select {
	case c.events <- ev:
	default:
		select {
		case c.events <- ev:
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
			c.log.Warn("signal event channel full, dropping", "event", fmt.Sprintf("%T", ev))
		}
	}
}

// pingLoop sends a PingReq every interval. Skipped when JoinResponse.PingInterval is 0.
func (c *SignalClient) pingLoop(ctx context.Context, interval time.Duration) {
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-t.C:
			ts := time.Now().UnixMilli()
			req := &livekit.SignalRequest{
				Message: &livekit.SignalRequest_PingReq{
					PingReq: &livekit.Ping{
						Timestamp: ts,
						Rtt:       c.lastPingTSNs.Load(),
					},
				},
			}
			if err := c.send(req); err != nil {
				c.log.Debug("ping send failed", "error", err)
				return
			}
		}
	}
}

// shutdown is called by recvLoop on exit. Closes the events channel and
// the done channel; idempotent.
func (c *SignalClient) shutdown() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	_ = c.conn.Close()
	close(c.events)
	close(c.done)
}

// setCloseErr records the disconnect reason. First call wins.
func (c *SignalClient) setCloseErr(err error, reason string) {
	if c.closeErr.Load() == nil && err != nil {
		c.closeErr.Store(&err)
	}
	if c.closeReason.Load() == nil && reason != "" {
		c.closeReason.Store(&reason)
	}
}

// JoinResponse returns the server's JoinResponse from the handshake.
func (c *SignalClient) JoinResponse() *livekit.JoinResponse { return c.join }

// Events returns the receive-only channel of typed signal events. The
// channel is closed when the client disconnects.
func (c *SignalClient) Events() <-chan SignalEvent { return c.events }

// Done is closed when the client disconnects (server-side leave, network
// error, or local Close).
func (c *SignalClient) Done() <-chan struct{} { return c.done }

// Err returns the disconnect error, or nil if the disconnect was clean.
// Only meaningful after Done() is closed.
func (c *SignalClient) Err() error {
	if p := c.closeErr.Load(); p != nil {
		return *p
	}
	return nil
}

// CloseReason returns a short string describing why the client closed —
// suitable for use as a leg.disconnected reason.
func (c *SignalClient) CloseReason() string {
	if p := c.closeReason.Load(); p != nil {
		return *p
	}
	return ""
}

// Close sends a LeaveRequest and shuts down the WS. Idempotent.
func (c *SignalClient) Close(reason livekit.DisconnectReason) error {
	if c.closed.Load() {
		return nil
	}
	leaveReq := &livekit.SignalRequest{
		Message: &livekit.SignalRequest_Leave{
			Leave: &livekit.LeaveRequest{
				Reason: reason,
				Action: livekit.LeaveRequest_DISCONNECT,
			},
		},
	}
	_ = c.send(leaveReq) // best-effort
	c.setCloseErr(nil, "livekit_client_closed")
	c.shutdown()
	return nil
}

// SendAnswer sends the client's SDP answer.
func (c *SignalClient) SendAnswer(sdp webrtc.SessionDescription) error {
	return c.send(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Answer{
			Answer: &livekit.SessionDescription{Type: "answer", Sdp: sdp.SDP},
		},
	})
}

// SendOffer sends a client-initiated SDP offer (publisher peer connection).
func (c *SignalClient) SendOffer(sdp webrtc.SessionDescription) error {
	return c.send(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Offer{
			Offer: &livekit.SessionDescription{Type: "offer", Sdp: sdp.SDP},
		},
	})
}

// SendTrickle sends a local ICE candidate to the server.
func (c *SignalClient) SendTrickle(cand webrtc.ICECandidateInit, target livekit.SignalTarget) error {
	candJSON, err := json.Marshal(cand)
	if err != nil {
		return fmt.Errorf("marshal candidate: %w", err)
	}
	return c.send(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Trickle{
			Trickle: &livekit.TrickleRequest{
				CandidateInit: string(candJSON),
				Target:        target,
			},
		},
	})
}

// AddTrack announces a new local track to be published. The server replies
// with a TrackPublished event whose Track.Sid is bound to req.Cid.
func (c *SignalClient) AddTrack(req *livekit.AddTrackRequest) error {
	if req == nil {
		return errors.New("AddTrack: nil request")
	}
	return c.send(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_AddTrack{AddTrack: req},
	})
}

// MuteTrack toggles the mute state of one of our published tracks.
func (c *SignalClient) MuteTrack(sid string, muted bool) error {
	return c.send(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Mute{
			Mute: &livekit.MuteTrackRequest{Sid: sid, Muted: muted},
		},
	})
}

// send marshals a SignalRequest and writes it as a binary WS frame. Safe
// for concurrent callers via writeMu.
func (c *SignalClient) send(req *livekit.SignalRequest) error {
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return errors.New("signal client closed")
	}
	return wsutil.WriteClientBinary(c.conn, data)
}

// parseICECandidateInit decodes a LiveKit-style trickle candidate JSON.
// LiveKit's CandidateInit is a string with the standard WebRTC
// ICECandidateInit JSON shape ({"candidate":"...","sdpMid":"...","sdpMLineIndex":N}).
func parseICECandidateInit(s string) (webrtc.ICECandidateInit, error) {
	var init webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(s), &init); err != nil {
		return init, err
	}
	return init, nil
}

// leaveReasonString maps a LiveKit DisconnectReason to the short tag used
// in leg.disconnected events.
func leaveReasonString(r livekit.DisconnectReason) string {
	switch r {
	case livekit.DisconnectReason_CLIENT_INITIATED:
		return "livekit_client_initiated"
	case livekit.DisconnectReason_DUPLICATE_IDENTITY:
		return "livekit_duplicate_identity"
	case livekit.DisconnectReason_SERVER_SHUTDOWN:
		return "livekit_server_shutdown"
	case livekit.DisconnectReason_PARTICIPANT_REMOVED:
		return "livekit_kicked"
	case livekit.DisconnectReason_ROOM_DELETED:
		return "livekit_room_deleted"
	case livekit.DisconnectReason_STATE_MISMATCH:
		return "livekit_state_mismatch"
	case livekit.DisconnectReason_JOIN_FAILURE:
		return "livekit_join_failure"
	case livekit.DisconnectReason_MIGRATION:
		return "livekit_migration"
	case livekit.DisconnectReason_SIGNAL_CLOSE:
		return "livekit_signal_close"
	case livekit.DisconnectReason_ROOM_CLOSED:
		return "livekit_room_closed"
	case livekit.DisconnectReason_USER_UNAVAILABLE:
		return "livekit_user_unavailable"
	case livekit.DisconnectReason_USER_REJECTED:
		return "livekit_user_rejected"
	case livekit.DisconnectReason_CONNECTION_TIMEOUT:
		return "livekit_token_expired"
	case livekit.DisconnectReason_MEDIA_FAILURE:
		return "livekit_media_failure"
	}
	return "livekit_disconnected"
}

// redactToken strips the access_token query param so URLs are safe to log.
func redactToken(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	if q.Get("access_token") != "" {
		q.Set("access_token", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// bufferedConn wraps a net.Conn so reads come through a bufio.Reader
// first. Used to preserve bytes the WebSocket dialer buffered alongside
// the HTTP 101 response (LiveKit sends the JoinResponse in the same TCP
// segment as the upgrade reply). Writes and connection control still go
// to the raw conn.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }
