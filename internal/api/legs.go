package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/amd"
	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/VoiceBlender/voiceblender/internal/speaking"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/go-chi/chi/v5"
)

func toLegView(l leg.Leg) LegView {
	return LegView{
		ID:         l.ID(),
		Type:       l.Type(),
		State:      l.State(),
		RoomID:     l.RoomID(),
		Muted:      l.IsMuted(),
		Deaf:       l.IsDeaf(),
		AcceptDTMF: l.AcceptDTMF(),
		Held:       l.IsHeld(),
		AppID:      l.AppID(),
		SIPHeaders: l.SIPHeaders(),
	}
}

// disconnectData builds the typed event data for a leg.disconnected event,
// including CDR (reason, timing) and optional quality metrics.
func disconnectData(l leg.Leg, reason string) *events.LegDisconnectedData {
	now := time.Now()
	d := &events.LegDisconnectedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		CDR: events.CallCDR{
			Reason:        reason,
			DurationTotal: roundTo2(now.Sub(l.CreatedAt()).Seconds()),
		},
	}
	if answered := l.AnsweredAt(); !answered.IsZero() {
		d.CDR.DurationAnswered = roundTo2(now.Sub(answered).Seconds())
	}
	if stats := l.RTPStats(); stats.PacketsReceived > 0 {
		d.Quality = &events.CallQuality{
			MOSScore:        stats.MOSScore,
			PacketsReceived: stats.PacketsReceived,
			PacketsLost:     stats.PacketsLost,
			JitterMs:        stats.JitterMs,
		}
	}
	return d
}

// publishDisconnect publishes the leg.disconnected event and then clears the
// per-leg webhook. The clear MUST happen after publish so the event has a route.
func (s *Server) publishDisconnect(l leg.Leg, reason string) {
	s.Bus.Publish(events.LegDisconnected, disconnectData(l, reason))
	s.Webhooks.ClearLegWebhook(l.ID())
}

func roundTo2(v float64) float64 {
	return math.Round(v*100) / 100
}

// inviteFailureReason maps a SIP INVITE error to a disconnect reason string.
func inviteFailureReason(err error, hasRingTimeout bool, ctx context.Context) string {
	// Ring timeout — context deadline exceeded while waiting for answer.
	if hasRingTimeout && ctx.Err() == context.DeadlineExceeded {
		return "ring_timeout"
	}

	// SIP response codes from sipgo's ErrDialogResponse.
	// sipgo returns both *ErrDialogResponse and ErrDialogResponse, so try both.
	var dialogErrPtr *sipgo.ErrDialogResponse
	var dialogErr sipgo.ErrDialogResponse
	var res *sip.Response
	if errors.As(err, &dialogErrPtr) {
		res = dialogErrPtr.Res
	} else if errors.As(err, &dialogErr) {
		res = dialogErr.Res
	}
	if res != nil {
		switch res.StatusCode {
		case sip.StatusBusyHere: // 486
			return "busy"
		case 480: // Temporarily Unavailable
			return "unavailable"
		case sip.StatusNotFound: // 404
			return "not_found"
		case sip.StatusForbidden: // 403
			return "forbidden"
		case 401, 407: // Unauthorized / Proxy Authentication Required
			return "unauthorized"
		case 408: // Request Timeout
			return "timeout"
		case 487: // Request Terminated (CANCEL was sent)
			return "cancelled"
		case 488: // Not Acceptable Here
			return "not_acceptable"
		case 503: // Service Unavailable
			return "service_unavailable"
		case 603: // Decline
			return "declined"
		default:
			if res.StatusCode >= 400 {
				return fmt.Sprintf("sip_%d", res.StatusCode)
			}
		}
	}

	return "invite_failed"
}

func (s *Server) listLegs(w http.ResponseWriter, r *http.Request) {
	legs := s.LegMgr.List()
	views := make([]LegView, len(legs))
	for i, l := range legs {
		views[i] = toLegView(l)
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	writeJSON(w, http.StatusOK, toLegView(l))
}

func (s *Server) doAnswerLeg(id string, speechDetection *bool) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	sipLeg, ok := l.(*leg.SIPLeg)
	if !ok {
		return newAPIError(http.StatusBadRequest, "only SIP inbound legs can be answered")
	}

	if l.State() != leg.StateRinging && l.State() != leg.StateEarlyMedia {
		return newAPIError(http.StatusConflict, "leg is %s, expected ringing or early_media", l.State())
	}

	if speechDetection != nil {
		s.setSpeechOverride(id, speechDetection)
	}
	sipLeg.SignalAnswer()
	return nil
}

func (s *Server) answerLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req AnswerLegRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
			return
		}
	}

	if err := s.doAnswerLeg(id, req.SpeechDetection); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "answering"})
}

func (s *Server) earlyMediaLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	sipLeg, ok := l.(*leg.SIPLeg)
	if !ok {
		writeError(w, http.StatusBadRequest, "only SIP inbound legs support early media")
		return
	}

	if l.State() != leg.StateRinging {
		writeError(w, http.StatusConflict, fmt.Sprintf("leg is %s, not ringing", l.State()))
		return
	}

	if err := sipLeg.EnableEarlyMedia(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("early media failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "early_media"})
}

func (s *Server) doMuteLeg(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	l.SetMuted(true)

	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			rm.Mixer().SetParticipantMuted(id, true)
		}
	}

	s.Bus.Publish(events.LegMuted, &events.LegMutedData{LegScope: events.LegScope{LegID: id, AppID: l.AppID()}})
	return nil
}

func (s *Server) muteLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doMuteLeg(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "muted"})
}

func (s *Server) doUnmuteLeg(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	l.SetMuted(false)

	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			rm.Mixer().SetParticipantMuted(id, false)
		}
	}

	s.Bus.Publish(events.LegUnmuted, &events.LegUnmutedData{LegScope: events.LegScope{LegID: id, AppID: l.AppID()}})
	return nil
}

func (s *Server) unmuteLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doUnmuteLeg(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unmuted"})
}

func (s *Server) doDeafLeg(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	l.SetDeaf(true)

	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			rm.Mixer().SetParticipantDeaf(id, true)
		}
	}

	s.Bus.Publish(events.LegDeaf, &events.LegDeafData{LegScope: events.LegScope{LegID: id, AppID: l.AppID()}})
	return nil
}

func (s *Server) deafLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doDeafLeg(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deaf"})
}

func (s *Server) doUndeafLeg(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	l.SetDeaf(false)

	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			rm.Mixer().SetParticipantDeaf(id, false)
		}
	}

	s.Bus.Publish(events.LegUndeaf, &events.LegUndeafData{LegScope: events.LegScope{LegID: id, AppID: l.AppID()}})
	return nil
}

func (s *Server) undeafLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doUndeafLeg(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "undeaf"})
}

func (s *Server) doHoldLeg(ctx context.Context, id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	sipLeg, ok := l.(*leg.SIPLeg)
	if !ok {
		return newAPIError(http.StatusBadRequest, "only SIP legs support hold")
	}

	if err := sipLeg.Hold(ctx); err != nil {
		return newAPIError(http.StatusConflict, "%s", err.Error())
	}

	return nil
}

func (s *Server) holdLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doHoldLeg(r.Context(), id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "held"})
}

// setupHoldCallbacks wires hold/unhold event publishing on a SIPLeg.
func (s *Server) setupHoldCallbacks(l *leg.SIPLeg) {
	l.OnHold(func() {
		s.Bus.Publish(events.LegHold, &events.LegHoldData{
			LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			LegType:  string(l.Type()),
		})
	})
	l.OnUnhold(func() {
		s.Bus.Publish(events.LegUnhold, &events.LegUnholdData{
			LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			LegType:  string(l.Type()),
		})
	})
}

// HandleReInvite processes a remote re-INVITE by finding the matching SIPLeg
// via Call-ID and delegating to its hold/unhold handler. Returns the SDP
// answer to include in the 200 OK response.
func (s *Server) HandleReInvite(callID string, direction string) []byte {
	for _, l := range s.LegMgr.List() {
		sl, ok := l.(*leg.SIPLeg)
		if !ok {
			continue
		}
		if sl.CallID() == callID {
			sdp := sl.ReInviteAnswerSDP(direction)
			sl.HandleRemoteHold(direction)
			// Reset session timer on any in-dialog re-INVITE (RFC 4028 §10).
			sl.ResetSessionTimer()
			return sdp
		}
	}
	s.Log.Warn("re-INVITE: no matching leg", "call_id", callID)
	return nil
}

func (s *Server) doUnholdLeg(ctx context.Context, id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	sipLeg, ok := l.(*leg.SIPLeg)
	if !ok {
		return newAPIError(http.StatusBadRequest, "only SIP legs support hold")
	}

	if err := sipLeg.Unhold(ctx); err != nil {
		return newAPIError(http.StatusConflict, "%s", err.Error())
	}

	return nil
}

func (s *Server) unholdLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doUnholdLeg(r.Context(), id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// cleanupLeg tears down the leg. Order matters: room removal first so the
// mixer stops pushing frames before Hangup closes the socket.
// Caller MUST publish LegDisconnected before any webhook is cleared.
func (s *Server) cleanupLeg(l leg.Leg) {
	if roomID := l.RoomID(); roomID != "" {
		s.onLegLeavingRoomRecording(roomID, l.ID())
		if err := s.RoomMgr.RemoveLeg(roomID, l.ID()); err != nil {
			s.Log.Debug("remove leg from room on cleanup", "leg_id", l.ID(), "room_id", roomID, "error", err)
		}
		s.stopRoomAgentIfEmpty(roomID)
		s.stopRoomRecordingIfEmpty(roomID)
	}

	if err := l.Hangup(context.Background()); err != nil {
		s.Log.Debug("cleanupLeg hangup", "leg_id", l.ID(), "error", err)
	}

	s.stopSpeakingDetector(l.ID())
	s.cleanupLegAgent(l.ID())
	s.stopLegRecording(l.ID())
	s.LegMgr.Remove(l.ID())
}

func (s *Server) doDeleteLeg(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}

	if err := l.Hangup(context.Background()); err != nil {
		s.Log.Warn("hangup error", "error", err)
	}
	s.cleanupLeg(l)
	s.publishDisconnect(l, "api_hangup")
	return nil
}

func (s *Server) deleteLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doDeleteLeg(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "hung_up"})
}

func (s *Server) createLeg(w http.ResponseWriter, r *http.Request) {
	var req CreateLegRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	switch req.Type {
	case "sip":
		s.createSIPOutboundLeg(w, r, req)
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported leg type: %s", req.Type))
	}
}

func (s *Server) createSIPOutboundLeg(w http.ResponseWriter, r *http.Request, req CreateLegRequest) {
	recipient := sip.Uri{}
	if err := sip.ParseUri(req.URI, &recipient); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid SIP URI: %v", err))
		return
	}

	// Parse codec overrides from request.
	var codecs []codec.CodecType
	for _, name := range req.Codecs {
		ct := codec.CodecTypeFromName(name)
		if ct == codec.CodecUnknown {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown codec: %s", name))
			return
		}
		codecs = append(codecs, ct)
	}

	// Ensure room exists if room_id is specified; create it if it doesn't.
	if req.RoomID != "" {
		if _, ok := s.RoomMgr.Get(req.RoomID); !ok {
			if _, err := s.RoomMgr.Create(req.RoomID, req.AppID, s.Config.DefaultSampleRate); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("create room: %v", err))
				return
			}
		}
	}

	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, codecs, s.Log)

	// Apply server-default jitter buffer. No per-request override: jitter
	// buffer tuning is operator-driven via the SIP_JITTER_BUFFER_MS env var.
	l.SetJitterBuffer(s.Config.SIPJitterBufferMs, s.Config.SIPJitterBufferMaxMs)

	if req.AcceptDTMF != nil {
		l.SetAcceptDTMF(*req.AcceptDTMF)
	}
	if req.AppID != "" {
		l.SetAppID(req.AppID)
	}

	var dtmfSeq atomic.Uint64
	l.OnDTMF(func(digit rune) {
		seq := dtmfSeq.Add(1)
		s.Bus.Publish(events.DTMFReceived, &events.DTMFReceivedData{
			LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			Digit:    string(digit),
			Seq:      seq,
		})
		s.broadcastDTMF(l.ID(), digit)
	})

	l.OnRTPTimeout(func() {
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "rtp_timeout")
		}
	})

	s.setupHoldCallbacks(l)

	// addToRoom adds the leg to the requested room at most once (on early
	// media or on connect, whichever comes first).
	var roomJoinOnce sync.Once
	addToRoom := func() {
		if req.RoomID == "" {
			return
		}
		roomJoinOnce.Do(func() {
			if err := s.RoomMgr.AddLeg(req.RoomID, l.ID()); err != nil {
				s.Log.Warn("auto-add leg to room failed", "leg_id", l.ID(), "room_id", req.RoomID, "error", err)
				return
			}
			s.onLegJoinedRoom(req.RoomID, l.ID())
		})
	}

	// Prepare AMD if requested.
	var startAMD func()
	if req.AMD != nil {
		var err error
		startAMD, err = s.prepareAMD(l, req.AMD)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		startAMD = func() {}
	}

	// Build invite options.
	inviteOpts := sipmod.InviteOptions{Codecs: codecs, FromUser: req.From}
	if req.Auth != nil {
		inviteOpts.AuthUsername = req.Auth.Username
		inviteOpts.AuthPassword = req.Auth.Password
	}
	inviteOpts.OnEarlyMedia = func(remoteSDP *sipmod.SDPMedia, rtpSess *sipmod.RTPSession) {
		if err := l.SetupEarlyMediaOutbound(remoteSDP, rtpSess); err != nil {
			s.Log.Warn("outbound early media failed", "leg_id", l.ID(), "error", err)
			return
		}
		s.Bus.Publish(events.LegEarlyMedia, &events.LegEarlyMediaData{
			LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			LegType:  string(l.Type()),
		})
		// NOTE: AMD is NOT started here — early media carries ringback
		// tones whose cadence (e.g. 2s on / 4s off) mimics a short human
		// greeting and would cause false "human" classifications. AMD
		// starts only after the call is answered (200 OK).
		addToRoom()
	}
	if req.Privacy != "" {
		inviteOpts.Headers = append(inviteOpts.Headers, sip.NewHeader("Privacy", req.Privacy))
	}
	for k, v := range req.Headers {
		inviteOpts.Headers = append(inviteOpts.Headers, sip.NewHeader(k, v))
	}

	s.LegMgr.Add(l)
	if req.WebhookURL != "" {
		s.Webhooks.SetLegWebhook(l.ID(), req.WebhookURL, req.WebhookSecret)
	}
	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope:   events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		URI:        req.URI,
		From:       req.From,
		SIPHeaders: req.Headers,
	})

	go func() {
		// Derive invite context from the leg's context so that
		// Hangup (via DELETE) cancels the INVITE and sends CANCEL.
		ctx := l.Context()
		if req.RingTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(req.RingTimeout)*time.Second)
			defer cancel()
		}

		call, err := s.SIPEngine.Invite(ctx, recipient, inviteOpts)
		if err != nil {
			s.Log.Info("outbound invite failed", "leg_id", l.ID(), "error", err)
			if l.State() != leg.StateHungUp { // not already deleted via API
				reason := inviteFailureReason(err, req.RingTimeout > 0, ctx)
				s.cleanupLeg(l)
				s.publishDisconnect(l, reason)
			}
			return
		}

		if err := l.ConnectOutbound(call); err != nil {
			s.Log.Error("connect outbound failed", "leg_id", l.ID(), "error", err)
			call.RTPSess.Close()
			call.Dialog.Bye(context.Background())
			s.cleanupLeg(l)
			s.publishDisconnect(l, "connect_failed")
			return
		}

		// Wire session timer expiry to hangup + event.
		l.OnSessionExpired(func() {
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.publishDisconnect(l, "session_expired")
			}
		})

		s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
			LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			LegType:  string(l.Type()),
		})
		s.maybeStartSpeakingDetector(l, req.SpeechDetection)
		startAMD()
		addToRoom()

		// Monitor for remote hangup or max duration.
		if req.MaxDuration > 0 {
			maxTimer := time.NewTimer(time.Duration(req.MaxDuration) * time.Second)
			defer maxTimer.Stop()
			select {
			case <-call.Dialog.Context().Done():
				if l.State() != leg.StateHungUp {
					s.cleanupLeg(l)
					s.publishDisconnect(l, "remote_bye")
				}
			case <-maxTimer.C:
				if l.State() != leg.StateHungUp {
					s.Log.Info("max duration reached", "leg_id", l.ID(), "max_duration", req.MaxDuration)
					s.cleanupLeg(l)
					s.publishDisconnect(l, "max_duration")
				}
			}
		} else {
			<-call.Dialog.Context().Done()
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.publishDisconnect(l, "remote_bye")
			}
		}
	}()

	writeJSON(w, http.StatusCreated, toLegView(l))
}

// HandleInboundCall is called from the SIP engine for inbound INVITE requests.
func (s *Server) HandleInboundCall(call *sipmod.InboundCall) {
	// Send provisional responses
	if err := call.Dialog.Respond(sip.StatusTrying, "Trying", nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Error("failed to send 100 Trying", "error", err)
		return
	}
	if err := call.Dialog.Respond(sip.StatusRinging, "Ringing", nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Error("failed to send 180 Ringing", "error", err)
		return
	}

	l := leg.NewSIPInboundLeg(call, s.SIPEngine, s.Log)
	if appID, ok := l.SIPHeaders()["X-App-ID"]; ok {
		l.SetAppID(appID)
	}
	s.LegMgr.Add(l)

	// Apply server-default jitter buffer to inbound legs. No per-call
	// override for inbound: inbound tuning is operator-driven via the
	// SIP_JITTER_BUFFER_MS env var.
	l.SetJitterBuffer(s.Config.SIPJitterBufferMs, s.Config.SIPJitterBufferMaxMs)

	// Route events for this leg to the per-leg webhook. Extract URL from SIP
	// X-Webhook-URL header, falling back to the configured default.
	webhookURL := ""
	if h := call.Request.GetHeader("X-Webhook-URL"); h != nil {
		webhookURL = h.Value()
	}
	if webhookURL == "" {
		webhookURL = s.Config.WebhookURL
	}
	webhookSecret := ""
	if h := call.Request.GetHeader("X-Webhook-Secret"); h != nil {
		webhookSecret = h.Value()
	}
	if webhookURL != "" {
		s.Webhooks.SetLegWebhook(l.ID(), webhookURL, webhookSecret)
	}

	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope:   events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		From:       call.From,
		To:         call.To,
		SIPHeaders: l.SIPHeaders(),
	})

	// Wait for REST answer or context cancellation (caller hangup / timeout)
	select {
	case <-l.AnswerCh():
		if err := l.Answer(context.Background()); err != nil {
			s.Log.Error("answer failed", "leg_id", l.ID(), "error", err)
			s.LegMgr.Remove(l.ID())
			s.Webhooks.ClearLegWebhook(l.ID())
			return
		}

		// Set up DTMF event forwarding
		var dtmfSeq atomic.Uint64
		l.OnDTMF(func(digit rune) {
			seq := dtmfSeq.Add(1)
			s.Bus.Publish(events.DTMFReceived, &events.DTMFReceivedData{
				LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
				Digit:    string(digit),
				Seq:      seq,
			})
			s.broadcastDTMF(l.ID(), digit)
		})

		l.OnRTPTimeout(func() {
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.publishDisconnect(l, "rtp_timeout")
			}
		})

		s.setupHoldCallbacks(l)

		// Wire session timer expiry to hangup + event.
		l.OnSessionExpired(func() {
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.publishDisconnect(l, "session_expired")
			}
		})

		s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
			LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			LegType:  string(l.Type()),
		})
		s.maybeStartSpeakingDetector(l, s.takeSpeechOverride(l.ID()))

		// Block until call ends (BYE received or context cancelled)
		<-call.Dialog.Context().Done()
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "remote_bye")
		}
		return

	case <-call.Dialog.Context().Done():
		// Caller hung up before answer
	}

	s.cleanupLeg(l)
	s.publishDisconnect(l, "caller_cancel")
}

// prepareAMD creates an AMD analyzer and returns a function that, when called,
// installs the tap and starts the analyzer goroutine. The returned function is
// safe to call multiple times (only the first call has effect).
func (s *Server) prepareAMD(l *leg.SIPLeg, req *AMDParams) (func(), error) {
	params := amd.MergeMillis(
		amd.DefaultParams(),
		req.InitialSilenceTimeout,
		req.GreetingDuration,
		req.AfterGreetingSilence,
		req.TotalAnalysisTime,
		req.MinimumWordLength,
		req.BeepTimeout,
	)
	if err := params.Validate(); err != nil {
		return nil, fmt.Errorf("invalid AMD params: %w", err)
	}

	analyzer := amd.New(params)
	buf := newAMDBuffer(256) // ~5s of 20ms frames

	var once sync.Once
	start := func() {
		once.Do(func() {
			l.SetAMDTap(buf)
			go func() {
				resampleReader := mixer.NewResampleReader(buf, l.SampleRate(), mixer.DefaultSampleRate)
				detection := analyzer.Run(l.Context(), resampleReader)

				s.Bus.Publish(events.AMDResult, &events.AMDResultData{
					LegScope:           events.LegScope{LegID: l.ID(), AppID: l.AppID()},
					Result:             string(detection.Result),
					InitialSilenceMs:   detection.InitialSilenceMs,
					GreetingDurationMs: detection.GreetingDurationMs,
					TotalAnalysisMs:    detection.TotalAnalysisMs,
				})

				if detection.Result == amd.ResultMachine && analyzer.Params().BeepTimeout > 0 {
					beep := analyzer.WaitForBeep(l.Context(), resampleReader)
					if beep.Detected {
						s.Bus.Publish(events.AMDBeep, &events.AMDBeepData{
							LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
							BeepMs:   beep.BeepMs,
						})
					}
				}

				l.ClearAMDTap()
				buf.Close()
			}()
		})
	}
	return start, nil
}

// startAMDLeg handles POST /v1/legs/{id}/amd — starts AMD on a connected leg.
func (s *Server) startAMDLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	sipLeg, ok := l.(*leg.SIPLeg)
	if !ok {
		writeError(w, http.StatusBadRequest, "AMD is only supported on SIP legs")
		return
	}

	if l.State() != leg.StateConnected {
		writeError(w, http.StatusConflict, fmt.Sprintf("leg must be connected, current state: %s", l.State()))
		return
	}

	var req AMDParams
	if err := decodeJSON(r, &req); err != nil {
		// Empty body is fine — use all defaults.
		req = AMDParams{}
	}

	start, err := s.prepareAMD(sipLeg, &req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	start()
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

// amdBuffer is a channel-backed buffer that implements io.Writer (non-blocking,
// drops overflow) and io.Reader (blocking). This prevents the readLoop from
// blocking when writing to the AMD tap.
type amdBuffer struct {
	ch     chan []byte
	closed chan struct{}
	buf    []byte // leftover from partial Read
}

func newAMDBuffer(cap int) *amdBuffer {
	return &amdBuffer{
		ch:     make(chan []byte, cap),
		closed: make(chan struct{}),
	}
}

// Write copies p and enqueues it. Non-blocking: drops if buffer full.
func (b *amdBuffer) Write(p []byte) (int, error) {
	frame := make([]byte, len(p))
	copy(frame, p)
	select {
	case <-b.closed:
		return len(p), nil
	default:
	}
	select {
	case b.ch <- frame:
	default:
		// drop on overflow
	}
	return len(p), nil
}

// Read blocks until data is available or the buffer is closed.
func (b *amdBuffer) Read(p []byte) (int, error) {
	// Serve leftover first.
	if len(b.buf) > 0 {
		n := copy(p, b.buf)
		b.buf = b.buf[n:]
		return n, nil
	}
	select {
	case frame, ok := <-b.ch:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, frame)
		if n < len(frame) {
			b.buf = frame[n:]
		}
		return n, nil
	case <-b.closed:
		return 0, io.EOF
	}
}

// Close signals the reader that no more data will arrive.
func (b *amdBuffer) Close() {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
}

// resolveSpeechDetection returns the effective speech-detection enable state
// for a leg, given an optional per-call override and the server-wide default.
func resolveSpeechDetection(override *bool, defaultEnabled bool) bool {
	if override != nil {
		return *override
	}
	return defaultEnabled
}

func (s *Server) setSpeechOverride(legID string, override *bool) {
	s.speechOverrideMu.Lock()
	s.speechOverride[legID] = override
	s.speechOverrideMu.Unlock()
}

func (s *Server) takeSpeechOverride(legID string) *bool {
	s.speechOverrideMu.Lock()
	defer s.speechOverrideMu.Unlock()
	ov, ok := s.speechOverride[legID]
	if ok {
		delete(s.speechOverride, legID)
	}
	return ov
}

// maybeStartSpeakingDetector attaches the speaking detector only if the
// effective enable state (per-call override or server default) is true.
func (s *Server) maybeStartSpeakingDetector(l leg.Leg, override *bool) {
	if !resolveSpeechDetection(override, s.Config.SpeechDetectionEnabled) {
		return
	}
	s.startSpeakingDetector(l)
}

// startSpeakingDetector creates and starts a speaking detector for a connected leg.
func (s *Server) startSpeakingDetector(l leg.Leg) {
	det := speaking.New(l.ID(), l.SampleRate(), l.IsMuted, func(e speaking.Event) {
		typ := events.SpeakingStarted
		if !e.Speaking {
			typ = events.SpeakingStopped
		}
		s.Bus.Publish(typ, &events.SpeakingData{
			LegRoomScope: events.LegRoomScope{LegID: e.LegID, RoomID: l.RoomID(), AppID: l.AppID()},
		})
	})
	l.SetSpeakingTap(det)
	det.Start()

	s.speakMu.Lock()
	s.speakDets[l.ID()] = det
	s.speakMu.Unlock()
}

// HasSpeakingDetector reports whether a speaking detector is currently
// attached to the given leg. Primarily for tests.
func (s *Server) HasSpeakingDetector(legID string) bool {
	s.speakMu.Lock()
	defer s.speakMu.Unlock()
	_, ok := s.speakDets[legID]
	return ok
}

// stopSpeakingDetector stops and removes a speaking detector for a leg.
func (s *Server) stopSpeakingDetector(legID string) {
	s.speakMu.Lock()
	det, ok := s.speakDets[legID]
	if ok {
		delete(s.speakDets, legID)
	}
	s.speakMu.Unlock()
	if ok {
		det.Stop()
	}
}
