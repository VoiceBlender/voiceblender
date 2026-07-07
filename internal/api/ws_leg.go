package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/wsmedia"
)

// wsLeg handles GET /v1/legs/websocket — the inbound WebSocket leg endpoint.
// Clients connect here, the request is upgraded, and a WebSocketLeg is
// registered with the manager. The leg transitions straight to
// StateConnected (no ringing flow).
func (s *Server) wsLeg(w http.ResponseWriter, r *http.Request) {
	cfg, err := wsCfgFromQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Log = s.Log
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	q := r.URL.Query()
	roomID := q.Get("room_id")
	appID := q.Get("app_id")
	webhookURL := q.Get("webhook_url")
	webhookSecret := q.Get("webhook_secret")
	rtt := parseBoolQuery(q.Get("rtt"))

	if roomID != "" {
		if _, ok := s.RoomMgr.Get(roomID); !ok {
			if _, err := s.RoomMgr.Create(roomID, appID, s.Config.DefaultSampleRate); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("create room: %v", err))
				return
			}
		}
	}

	headers := captureCustomHeaders(r.Header)

	tr, _, err := wsmedia.UpgradeServer(w, r, cfg)
	if err != nil {
		s.Log.Warn("ws leg upgrade failed", "error", err)
		return
	}

	l := leg.NewWebSocketInboundLeg(tr, headers, cfg.SampleRate, rtt, s.Log)
	if appID != "" {
		l.SetAppID(appID)
	}
	s.LegMgr.Add(l)
	if webhookURL != "" {
		s.Webhooks.SetLegWebhook(l.ID(), webhookURL, webhookSecret)
	}

	// Welcome frame mirrors /v1/rooms/{id}/ws so a client written
	// against either endpoint sees the same handshake shape.
	// `participant_id` is included as an alias for `leg_id` for drop-in
	// compatibility with room-WS clients.
	_ = tr.SendStructured(map[string]any{
		"type":           "connected",
		"leg_id":         l.ID(),
		"participant_id": l.ID(),
		"sample_rate":    cfg.SampleRate,
		"format":         "pcm_s16le",
	})

	s.wireWSLegEventForwarding(l)

	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope:   events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:    string(l.Type()),
		SIPHeaders: headers,
	})
	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  string(l.Type()),
	})

	if roomID != "" {
		if err := s.RoomMgr.AddLeg(roomID, l.ID()); err != nil {
			s.Log.Warn("auto-add ws leg to room failed", "leg_id", l.ID(), "room_id", roomID, "error", err)
		} else {
			s.onLegJoinedRoom(roomID, l.ID())
		}
	}

	connectedAt := time.Now()
	<-tr.Done()
	reason := classifyWSReason(tr.Err())
	s.Log.Info("ws leg session closed",
		"leg_id", l.ID(),
		"reason", reason,
		"duration_ms", time.Since(connectedAt).Milliseconds(),
	)
	if l.State() != leg.StateHungUp {
		s.cleanupLeg(l)
		s.publishDisconnect(l, reason)
	}
}

// createWebSocketOutboundLeg dials a remote WebSocket and registers the
// resulting leg. Responds 201 immediately; the dial completes
// asynchronously and publishes LegConnected or LegDisconnected.
func (s *Server) createWebSocketOutboundLeg(w http.ResponseWriter, r *http.Request, req CreateLegRequest) {
	view, err := s.doCreateWebSocketOutboundLeg(req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// doCreateWebSocketOutboundLeg validates and starts an outbound WebSocket leg,
// returning the leg view. Shared by the REST handler and VSI create_leg.
func (s *Server) doCreateWebSocketOutboundLeg(req CreateLegRequest) (LegView, error) {
	if req.URL == "" {
		return LegView{}, newAPIError(http.StatusBadRequest, "url is required for type=websocket")
	}
	cfg, err := wsCfgFromCreateReq(req, s.Log)
	if err != nil {
		return LegView{}, newAPIError(http.StatusBadRequest, "%s", err.Error())
	}

	if req.RoomID != "" {
		if _, ok := s.RoomMgr.Get(req.RoomID); !ok {
			if _, err := s.RoomMgr.Create(req.RoomID, req.AppID, s.Config.DefaultSampleRate); err != nil {
				return LegView{}, newAPIError(http.StatusInternalServerError, "create room: %v", err)
			}
		}
	}

	l := leg.NewWebSocketOutboundPendingLeg(cfg.SampleRate, req.RTT, s.Log)
	if req.AcceptDTMF != nil {
		l.SetAcceptDTMF(*req.AcceptDTMF)
	}
	if req.AppID != "" {
		l.SetAppID(req.AppID)
	}
	s.LegMgr.Add(l)
	if req.WebhookURL != "" {
		s.Webhooks.SetLegWebhook(l.ID(), req.WebhookURL, req.WebhookSecret)
	}

	s.wireWSLegEventForwarding(l)

	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope:   events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:    string(l.Type()),
		URI:        req.URL,
		From:       req.From,
		SIPHeaders: req.Headers,
	})

	go s.runWSOutboundDial(l, req, cfg)

	return toLegView(l), nil
}

func (s *Server) runWSOutboundDial(l *leg.WebSocketLeg, req CreateLegRequest, cfg wsmedia.Config) {
	dialCfg := cfg
	if len(req.Headers) > 0 {
		dialCfg.Headers = http.Header{}
		for k, v := range req.Headers {
			dialCfg.Headers.Set(k, v)
		}
	}

	ctx := l.Context()
	if req.RingTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.RingTimeout)*time.Second)
		defer cancel()
	}

	tr, peerHdr, err := wsmedia.DialClient(ctx, req.URL, dialCfg)
	if err != nil {
		reason := classifyWSDialError(err, ctx.Err())
		s.cleanupLeg(l)
		s.publishDisconnect(l, reason)
		return
	}

	peerHeaders := captureCustomHeaders(peerHdr)
	l.AttachTransport(tr, peerHeaders)

	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  string(l.Type()),
	})
	s.maybeStartSpeakingDetector(l, req.SpeechDetection)
	if req.RoomID != "" {
		if err := s.RoomMgr.AddLeg(req.RoomID, l.ID()); err != nil {
			s.Log.Warn("auto-add ws outbound leg to room failed", "leg_id", l.ID(), "room_id", req.RoomID, "error", err)
		} else {
			s.onLegJoinedRoom(req.RoomID, l.ID())
		}
	}

	connectedAt := time.Now()
	if req.MaxDuration > 0 {
		t := time.NewTimer(time.Duration(req.MaxDuration) * time.Second)
		defer t.Stop()
		select {
		case <-tr.Done():
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.publishDisconnect(l, classifyWSReason(tr.Err()))
			}
		case <-t.C:
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.publishDisconnect(l, "max_duration")
			}
		}
	} else {
		<-tr.Done()
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, classifyWSReason(tr.Err()))
		}
	}
	s.Log.Info("ws outbound leg session closed",
		"leg_id", l.ID(),
		"duration_ms", time.Since(connectedAt).Milliseconds(),
	)
}

// wireWSLegEventForwarding hooks the leg's text channel into the bus,
// publishing RTTReceived events the same way SIP legs do.
func (s *Server) wireWSLegEventForwarding(l *leg.WebSocketLeg) {
	var rttSeq atomic.Uint64
	l.OnTextReceived(func(text string, lossMarker bool) {
		seq := rttSeq.Add(1)
		s.Bus.Publish(events.RTTReceived, &events.RTTReceivedData{
			LegScope:   events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			Text:       text,
			Seq:        seq,
			LossMarker: lossMarker,
		})
		s.broadcastRTT(l.ID(), text)
	})
}

// wsCfgFromQuery builds a wsmedia.Config from a request's query parameters.
// Defaults are applied by Config.Validate (which the caller must invoke).
func wsCfgFromQuery(r *http.Request) (wsmedia.Config, error) {
	q := r.URL.Query()
	cfg := wsmedia.Config{}
	if v := q.Get("sample_rate"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("sample_rate: %w", err)
		}
		cfg.SampleRate = n
	}
	if v := q.Get("wire_format"); v != "" {
		cfg.WireFormat = wsmedia.WireFormat(v)
	}
	if v := q.Get("sample_format"); v != "" {
		cfg.SampleFormat = wsmedia.SampleFormat(v)
	}
	return cfg, nil
}

// wsCfgFromCreateReq mirrors wsCfgFromQuery but pulls fields out of a
// JSON POST body. Validation runs with log already set so the resulting
// config is fully populated (FrameMs, FrameBytesPCM, etc.).
func wsCfgFromCreateReq(req CreateLegRequest, log *slog.Logger) (wsmedia.Config, error) {
	cfg := wsmedia.Config{
		SampleRate:   req.SampleRate,
		WireFormat:   wsmedia.WireFormat(req.WireFormat),
		SampleFormat: wsmedia.SampleFormat(req.SampleFormat),
		Log:          log,
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// captureCustomHeaders returns a map of caller-supplied custom headers
// (X-/P-/Authorization). Protocol headers added by the WS handshake itself
// (Sec-WebSocket-*, Upgrade, Connection, Host) are excluded.
func captureCustomHeaders(h http.Header) map[string]string {
	if h == nil {
		return nil
	}
	out := map[string]string{}
	for k, vs := range h {
		if len(vs) == 0 {
			continue
		}
		if !isCustomHeader(k) {
			continue
		}
		out[k] = vs[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isCustomHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "authorization":
		return true
	}
	if strings.HasPrefix(lower, "x-") || strings.HasPrefix(lower, "p-") {
		return true
	}
	return false
}

// classifyWSReason maps a terminal transport error to a disconnect reason
// string for the leg.disconnected event.
func classifyWSReason(err error) string {
	if err == nil {
		return "hangup"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "deadline exceeded") && strings.Contains(strings.ToLower(msg), "write"):
		return "peer_slow"
	case strings.Contains(msg, "i/o timeout"):
		return "timeout"
	case strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	case strings.Contains(msg, "connection reset"), strings.Contains(msg, "broken pipe"):
		return "connection_reset"
	case strings.Contains(msg, "use of closed network connection"):
		return "hangup"
	case strings.Contains(msg, "EOF"):
		return "hangup"
	}
	return "ws_error"
}

// classifyWSDialError maps an error from wsmedia.DialClient to a
// disconnect reason. ctxErr is the dial context's err (for distinguishing
// caller-induced timeouts).
func classifyWSDialError(err error, ctxErr error) string {
	if ctxErr == context.DeadlineExceeded {
		return "ring_timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "401"), strings.Contains(msg, "unauthorized"):
		return "unauthorized"
	case strings.Contains(msg, "403"), strings.Contains(msg, "forbidden"):
		return "forbidden"
	case strings.Contains(msg, "404"), strings.Contains(msg, "not found"):
		return "not_found"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no route to host"):
		return "service_unavailable"
	case strings.Contains(msg, "context canceled"):
		return "cancelled"
	}
	return "ws_dial_failed"
}

func parseBoolQuery(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	}
	return false
}
