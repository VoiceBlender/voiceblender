package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/moqmedia"
)

// moqLeg handles CONNECT /v1/legs/moq — the inbound MoQ-over-WebTransport
// leg endpoint. Reachable only over HTTP/3 (the HTTP/1.1 chi listener on
// HTTP_ADDR has no WebTransport support, and webtransport.Server.Upgrade
// will reject anything that isn't an extended-CONNECT request).
//
// PoC scope: leg goes straight to StateConnected; no DTMF, no RTT/text,
// no event parity beyond LegConnected / LegDisconnected.
func (s *Server) moqLeg(w http.ResponseWriter, r *http.Request) {
	if s.MoQWebTransport == nil {
		writeError(w, http.StatusServiceUnavailable, "MoQ endpoint is not enabled (set MOQ_ENABLED=true)")
		return
	}

	q := r.URL.Query()
	roomID := q.Get("room_id")
	appID := q.Get("app_id")
	webhookURL := q.Get("webhook_url")
	webhookSecret := q.Get("webhook_secret")

	sampleRate := moqmedia.DefaultSampleRate
	if v := q.Get("sample_rate"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("sample_rate: %v", err))
			return
		}
		sampleRate = n
	}

	bitrate := s.Config.MoQOpusBitrate
	if bitrate == 0 {
		bitrate = moqmedia.DefaultOpusBitrate
	}

	cfg := moqmedia.Config{
		SampleRate:  sampleRate,
		OpusBitrate: bitrate,
		Log:         s.Log,
	}
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if roomID != "" {
		if _, ok := s.RoomMgr.Get(roomID); !ok {
			if _, err := s.RoomMgr.Create(roomID, appID, s.Config.DefaultSampleRate); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("create room: %v", err))
				return
			}
		}
	}

	headers := captureCustomHeaders(r.Header)

	tr, err := moqmedia.UpgradeServer(w, r, s.MoQWebTransport, cfg)
	if err != nil {
		s.Log.Warn("moq leg upgrade failed", "error", err)
		return
	}

	l := leg.NewMoQInboundLeg(tr, headers, cfg.SampleRate, s.Log)
	if appID != "" {
		l.SetAppID(appID)
	}
	s.LegMgr.Add(l)
	if webhookURL != "" {
		s.Webhooks.SetLegWebhook(l.ID(), webhookURL, webhookSecret)
	}

	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  string(l.Type()),
	})

	if roomID != "" {
		if err := s.RoomMgr.AddLeg(roomID, l.ID()); err != nil {
			s.Log.Warn("auto-add moq leg to room failed", "leg_id", l.ID(), "room_id", roomID, "error", err)
		} else {
			s.onLegJoinedRoom(roomID, l.ID())
		}
	}

	connectedAt := time.Now()
	<-tr.Done()
	reason := "hangup"
	if e := tr.Err(); e != nil {
		reason = "moq_error"
	}
	s.Log.Info("moq leg session closed",
		"leg_id", l.ID(),
		"reason", reason,
		"duration_ms", time.Since(connectedAt).Milliseconds(),
	)
	if l.State() != leg.StateHungUp {
		s.cleanupLeg(l)
		s.publishDisconnect(l, reason)
	}
}
