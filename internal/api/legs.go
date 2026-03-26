package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/go-chi/chi/v5"
)

type legView struct {
	ID         string            `json:"leg_id"`
	Type       leg.LegType       `json:"type"`
	State      leg.LegState      `json:"state"`
	RoomID     string            `json:"room_id,omitempty"`
	Muted      bool              `json:"muted"`
	Held       bool              `json:"held"`
	SIPHeaders map[string]string `json:"sip_headers,omitempty"`
}

func toLegView(l leg.Leg) legView {
	return legView{
		ID:         l.ID(),
		Type:       l.Type(),
		State:      l.State(),
		RoomID:     l.RoomID(),
		Muted:      l.IsMuted(),
		Held:       l.IsHeld(),
		SIPHeaders: l.SIPHeaders(),
	}
}

// disconnectData builds the event data map for a leg.disconnected event,
// including duration_total and duration_answered (in seconds).
func disconnectData(l leg.Leg, reason string) map[string]interface{} {
	now := time.Now()
	data := map[string]interface{}{
		"leg_id":         l.ID(),
		"reason":         reason,
		"duration_total": roundTo2(now.Sub(l.CreatedAt()).Seconds()),
	}
	if answered := l.AnsweredAt(); !answered.IsZero() {
		data["duration_answered"] = roundTo2(now.Sub(answered).Seconds())
	} else {
		data["duration_answered"] = float64(0)
	}
	return data
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
	views := make([]legView, len(legs))
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

func (s *Server) answerLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	sipLeg, ok := l.(*leg.SIPLeg)
	if !ok {
		writeError(w, http.StatusBadRequest, "only SIP inbound legs can be answered")
		return
	}

	if l.State() != leg.StateRinging && l.State() != leg.StateEarlyMedia {
		writeError(w, http.StatusConflict, fmt.Sprintf("leg is %s, expected ringing or early_media", l.State()))
		return
	}

	sipLeg.SignalAnswer()
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

func (s *Server) muteLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	l.SetMuted(true)

	// Sync to mixer if the leg is in a room.
	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			rm.Mixer().SetParticipantMuted(id, true)
		}
	}

	s.Bus.Publish(events.LegMuted, map[string]interface{}{"leg_id": id})
	writeJSON(w, http.StatusOK, map[string]string{"status": "muted"})
}

func (s *Server) unmuteLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	l.SetMuted(false)

	// Sync to mixer if the leg is in a room.
	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			rm.Mixer().SetParticipantMuted(id, false)
		}
	}

	s.Bus.Publish(events.LegUnmuted, map[string]interface{}{"leg_id": id})
	writeJSON(w, http.StatusOK, map[string]string{"status": "unmuted"})
}

func (s *Server) holdLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	sipLeg, ok := l.(*leg.SIPLeg)
	if !ok {
		writeError(w, http.StatusBadRequest, "only SIP legs support hold")
		return
	}

	if err := sipLeg.Hold(r.Context()); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "held"})
}

// setupHoldCallbacks wires hold/unhold event publishing on a SIPLeg.
func (s *Server) setupHoldCallbacks(l *leg.SIPLeg) {
	l.OnHold(func() {
		s.Bus.Publish(events.LegHold, map[string]interface{}{
			"leg_id": l.ID(),
			"type":   string(l.Type()),
		})
	})
	l.OnUnhold(func() {
		s.Bus.Publish(events.LegUnhold, map[string]interface{}{
			"leg_id": l.ID(),
			"type":   string(l.Type()),
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

func (s *Server) unholdLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	sipLeg, ok := l.(*leg.SIPLeg)
	if !ok {
		writeError(w, http.StatusBadRequest, "only SIP legs support hold")
		return
	}

	if err := sipLeg.Unhold(r.Context()); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// cleanupLeg stops any active recording, removes the leg from its room (if any),
// and removes it from the leg manager. Must be called on every disconnect path.
func (s *Server) cleanupLeg(l leg.Leg) {
	// Ensure the leg's RTP session is closed and its context cancelled so that
	// any readers (recording, agent) unblock promptly. For remote-BYE the dialog
	// is already done; the BYE send error is harmless and ignored.
	if err := l.Hangup(context.Background()); err != nil {
		s.Log.Debug("cleanupLeg hangup", "leg_id", l.ID(), "error", err)
	}

	// Stop agent before recording so mixer taps can still be cleared.
	s.cleanupLegAgent(l.ID())
	// Stop recording before room removal so mixer taps can still be cleared.
	s.stopLegRecording(l.ID())

	if roomID := l.RoomID(); roomID != "" {
		if err := s.RoomMgr.RemoveLeg(roomID, l.ID()); err != nil {
			s.Log.Debug("remove leg from room on cleanup", "leg_id", l.ID(), "room_id", roomID, "error", err)
		}
		s.stopRoomAgentIfEmpty(roomID)
	}
	s.LegMgr.Remove(l.ID())
}

func (s *Server) deleteLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	if err := l.Hangup(r.Context()); err != nil {
		s.Log.Warn("hangup error", "error", err)
	}
	s.cleanupLeg(l)
	s.Bus.Publish(events.LegDisconnected, disconnectData(l, "api_hangup"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "hung_up"})
}

type sipAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type createLegRequest struct {
	Type        string            `json:"type"`                   // "sip" or "webrtc"
	URI         string            `json:"uri"`                    // SIP URI for outbound
	From        string            `json:"from,omitempty"`         // caller ID (user part of the SIP From header, e.g. "+15551234567")
	Privacy     string            `json:"privacy,omitempty"`      // SIP Privacy header value (e.g. "id", "none")
	RingTimeout int               `json:"ring_timeout,omitempty"` // seconds; 0 = no timeout
	MaxDuration int               `json:"max_duration,omitempty"` // seconds; 0 = no limit
	Codecs      []string          `json:"codecs,omitempty"`       // codec preference order, e.g. ["PCMU","PCMA","G722","opus"]
	Headers     map[string]string `json:"headers,omitempty"`      // custom SIP headers for outbound INVITE
	RoomID      string            `json:"room_id,omitempty"`      // add leg to this room once media is ready (early_media or connected)
	Auth        *sipAuth          `json:"auth,omitempty"`         // SIP digest auth credentials (optional)
}

func (s *Server) createLeg(w http.ResponseWriter, r *http.Request) {
	var req createLegRequest
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

func (s *Server) createSIPOutboundLeg(w http.ResponseWriter, r *http.Request, req createLegRequest) {
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
			if _, err := s.RoomMgr.Create(req.RoomID); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("create room: %v", err))
				return
			}
		}
	}

	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, codecs, s.Log)

	l.OnDTMF(func(digit rune) {
		s.Bus.Publish(events.DTMFReceived, map[string]interface{}{
			"leg_id": l.ID(),
			"digit":  string(digit),
		})
	})

	l.OnRTPTimeout(func() {
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.Bus.Publish(events.LegDisconnected, disconnectData(l, "rtp_timeout"))
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
		s.Bus.Publish(events.LegEarlyMedia, map[string]interface{}{
			"leg_id": l.ID(),
			"type":   string(l.Type()),
		})
		addToRoom()
	}
	if req.Privacy != "" {
		inviteOpts.Headers = append(inviteOpts.Headers, sip.NewHeader("Privacy", req.Privacy))
	}
	for k, v := range req.Headers {
		inviteOpts.Headers = append(inviteOpts.Headers, sip.NewHeader(k, v))
	}

	s.LegMgr.Add(l)
	ringingData := map[string]interface{}{"leg_id": l.ID(), "uri": req.URI}
	if req.From != "" {
		ringingData["from"] = req.From
	}
	if len(req.Headers) > 0 {
		ringingData["sip_headers"] = req.Headers
	}
	s.Bus.Publish(events.LegRinging, ringingData)

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
				s.Bus.Publish(events.LegDisconnected, disconnectData(l, reason))
			}
			return
		}

		if err := l.ConnectOutbound(call); err != nil {
			s.Log.Error("connect outbound failed", "leg_id", l.ID(), "error", err)
			call.RTPSess.Close()
			call.Dialog.Bye(context.Background())
			s.cleanupLeg(l)
			s.Bus.Publish(events.LegDisconnected, disconnectData(l, "connect_failed"))
			return
		}

		// Wire session timer expiry to hangup + event.
		l.OnSessionExpired(func() {
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.Bus.Publish(events.LegDisconnected, disconnectData(l, "session_expired"))
			}
		})

		s.Bus.Publish(events.LegConnected, map[string]interface{}{"leg_id": l.ID(), "type": string(l.Type())})
		addToRoom()

		// Monitor for remote hangup or max duration.
		if req.MaxDuration > 0 {
			maxTimer := time.NewTimer(time.Duration(req.MaxDuration) * time.Second)
			defer maxTimer.Stop()
			select {
			case <-call.Dialog.Context().Done():
				if l.State() != leg.StateHungUp {
					s.cleanupLeg(l)
					s.Bus.Publish(events.LegDisconnected, disconnectData(l, "remote_bye"))
				}
			case <-maxTimer.C:
				if l.State() != leg.StateHungUp {
					s.Log.Info("max duration reached", "leg_id", l.ID(), "max_duration", req.MaxDuration)
					s.cleanupLeg(l)
					s.Bus.Publish(events.LegDisconnected, disconnectData(l, "max_duration"))
				}
			}
		} else {
			<-call.Dialog.Context().Done()
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.Bus.Publish(events.LegDisconnected, disconnectData(l, "remote_bye"))
			}
		}
	}()

	writeJSON(w, http.StatusCreated, toLegView(l))
}

// HandleInboundCall is called from the SIP engine for inbound INVITE requests.
func (s *Server) HandleInboundCall(call *sipmod.InboundCall) {
	// Register webhook from SIP X-Webhook-URL header, falling back to config default.
	webhookURL := ""
	if h := call.Request.GetHeader("X-Webhook-URL"); h != nil {
		webhookURL = h.Value()
	}
	if webhookURL == "" {
		webhookURL = s.Config.WebhookURL
	}
	if webhookURL != "" {
		s.Webhooks.RegisterIfNew(webhookURL, "")
	}

	// Send provisional responses
	if err := call.Dialog.Respond(sip.StatusTrying, "Trying", nil); err != nil {
		s.Log.Error("failed to send 100 Trying", "error", err)
		return
	}
	if err := call.Dialog.Respond(sip.StatusRinging, "Ringing", nil); err != nil {
		s.Log.Error("failed to send 180 Ringing", "error", err)
		return
	}

	l := leg.NewSIPInboundLeg(call, s.SIPEngine, s.Log)
	s.LegMgr.Add(l)
	inboundRinging := map[string]interface{}{
		"leg_id": l.ID(),
		"from":   call.From,
		"to":     call.To,
	}
	if hdrs := l.SIPHeaders(); len(hdrs) > 0 {
		inboundRinging["sip_headers"] = hdrs
	}
	s.Bus.Publish(events.LegRinging, inboundRinging)

	// Wait for REST answer or context cancellation (caller hangup / timeout)
	select {
	case <-l.AnswerCh():
		if err := l.Answer(context.Background()); err != nil {
			s.Log.Error("answer failed", "leg_id", l.ID(), "error", err)
			s.LegMgr.Remove(l.ID())
			return
		}

		// Set up DTMF event forwarding
		l.OnDTMF(func(digit rune) {
			s.Bus.Publish(events.DTMFReceived, map[string]interface{}{
				"leg_id": l.ID(),
				"digit":  string(digit),
			})
		})

		l.OnRTPTimeout(func() {
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.Bus.Publish(events.LegDisconnected, disconnectData(l, "rtp_timeout"))
			}
		})

		s.setupHoldCallbacks(l)

		// Wire session timer expiry to hangup + event.
		l.OnSessionExpired(func() {
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.Bus.Publish(events.LegDisconnected, disconnectData(l, "session_expired"))
			}
		})

		s.Bus.Publish(events.LegConnected, map[string]interface{}{"leg_id": l.ID(), "type": string(l.Type())})

		// Block until call ends (BYE received or context cancelled)
		<-call.Dialog.Context().Done()
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.Bus.Publish(events.LegDisconnected, disconnectData(l, "remote_bye"))
		}
		return

	case <-call.Dialog.Context().Done():
		// Caller hung up before answer
	}

	s.cleanupLeg(l)
	s.Bus.Publish(events.LegDisconnected, disconnectData(l, "caller_cancel"))
}
