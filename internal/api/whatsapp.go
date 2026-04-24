package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/webrtc/v4"
)

const whatsAppInviteTimeout = 30 * time.Second

// handleWhatsAppInbound answers an inbound WhatsApp Business Calling INVITE.
// The SDP offer describes ICE+DTLS-SRTP+Opus; we feed it into a PCMedia,
// gather local ICE, respond 180 Ringing, register a WhatsAppLeg in "ringing"
// state and block until REST issues POST /v1/legs/{id}/answer.
func (s *Server) handleWhatsAppInbound(call *sipmod.InboundCall) {
	ctx := call.Dialog.Context()

	media, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:      codec.CodecOpus,
		ICEServers: s.Config.ICEServers,
		RTPPortMin: uint16(s.Config.RTPPortMin),
		RTPPortMax: uint16(s.Config.RTPPortMax),
		Log:        s.Log,
	})
	if err != nil {
		s.Log.Error("whatsapp inbound: create PCMedia", "error", err)
		_ = call.Dialog.Respond(sip.StatusInternalServerError, "Media Setup Failed", nil, s.SIPEngine.ServerHeader())
		return
	}

	pc := media.PC()
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(call.Request.Body())}
	if err := pc.SetRemoteDescription(offer); err != nil {
		s.Log.Error("whatsapp inbound: SetRemoteDescription", "error", err)
		media.Close()
		_ = call.Dialog.Respond(sip.StatusBadRequest, "Bad SDP Offer", nil, s.SIPEngine.ServerHeader())
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.Log.Error("whatsapp inbound: CreateAnswer", "error", err)
		media.Close()
		_ = call.Dialog.Respond(sip.StatusInternalServerError, "Answer Failed", nil, s.SIPEngine.ServerHeader())
		return
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.Log.Error("whatsapp inbound: SetLocalDescription", "error", err)
		media.Close()
		_ = call.Dialog.Respond(sip.StatusInternalServerError, "Answer Failed", nil, s.SIPEngine.ServerHeader())
		return
	}
	// Block until ICE gathering finishes so the 200 OK carries a complete SDP
	// (Meta does not support re-INVITE / trickle ICE over SIP).
	select {
	case <-gatherDone:
	case <-ctx.Done():
		media.Close()
		return
	}
	finalSDP := []byte(pc.LocalDescription().SDP)

	if err := call.Dialog.Respond(sip.StatusRinging, "Ringing", nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Error("whatsapp inbound: respond 180", "error", err)
		media.Close()
		return
	}

	headers := sipHeadersFromRequest(call.Request)
	l := leg.NewWhatsAppInboundLeg(call.Dialog, media, call.From, call.To, headers, finalSDP, s.Log)
	if appID, ok := headers["X-App-ID"]; ok {
		l.SetAppID(appID)
	}
	s.LegMgr.Add(l)

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
		LegType:    string(l.Type()),
		From:       call.From,
		To:         call.To,
		SIPHeaders: headers,
	})

	select {
	case <-l.AnswerCh():
		if err := l.Answer(context.Background()); err != nil {
			s.Log.Error("whatsapp inbound: answer failed", "leg_id", l.ID(), "error", err)
			s.cleanupLeg(l)
			return
		}
		s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
			LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			LegType:  string(l.Type()),
		})
		<-ctx.Done()
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "remote_bye")
		}
	case <-ctx.Done():
		s.cleanupLeg(l)
		s.publishDisconnect(l, "caller_cancel")
	}
}

// createWhatsAppOutboundLeg places an outbound call to a WhatsApp user.
func (s *Server) createWhatsAppOutboundLeg(w http.ResponseWriter, r *http.Request, req CreateLegRequest) {
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "'to' is required")
		return
	}
	if req.From == "" {
		writeError(w, http.StatusBadRequest, "'from' is required (business phone number, E.164 without '+')")
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "'password' is required (Meta-issued digest password)")
		return
	}
	if s.SIPEngine.TLSPort() == 0 {
		writeError(w, http.StatusServiceUnavailable, "SIP TLS not configured on this instance")
		return
	}

	if req.RoomID != "" {
		if _, ok := s.RoomMgr.Get(req.RoomID); !ok {
			if _, err := s.RoomMgr.Create(req.RoomID, req.AppID, s.Config.DefaultSampleRate); err != nil {
				writeError(w, http.StatusInternalServerError, "create room: "+err.Error())
				return
			}
		}
	}

	media, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:      codec.CodecOpus,
		ICEServers: s.Config.ICEServers,
		RTPPortMin: uint16(s.Config.RTPPortMin),
		RTPPortMax: uint16(s.Config.RTPPortMax),
		Log:        s.Log,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create PCMedia")
		return
	}

	pc := media.PC()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		media.Close()
		writeError(w, http.StatusInternalServerError, "failed to create offer")
		return
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		media.Close()
		writeError(w, http.StatusInternalServerError, "failed to set local description")
		return
	}

	select {
	case <-gatherDone:
	case <-r.Context().Done():
		media.Close()
		writeError(w, http.StatusGatewayTimeout, "client disconnected during ICE gathering")
		return
	}
	sdpOffer := []byte(pc.LocalDescription().SDP)

	fromUser := strings.TrimPrefix(req.From, "+")
	recipient := sipmod.WhatsAppRecipientURI(req.To)

	inviteCtx, cancel := context.WithTimeout(context.Background(), whatsAppInviteTimeout)
	defer cancel()

	call, err := s.SIPEngine.InviteWhatsApp(inviteCtx, recipient, sipmod.WhatsAppInviteOptions{
		FromUser: fromUser,
		Password: req.Password,
		SDPOffer: sdpOffer,
	})
	if err != nil {
		media.Close()
		writeError(w, http.StatusBadGateway, "invite failed: "+err.Error())
		return
	}

	answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(call.AnswerSDP)}
	if err := pc.SetRemoteDescription(answer); err != nil {
		_ = call.Dialog.Bye(context.Background())
		media.Close()
		writeError(w, http.StatusBadGateway, "invalid SDP answer from WhatsApp")
		return
	}

	l := leg.NewWhatsAppOutboundLeg(call.Dialog, media, req.From, req.To, s.Log)
	if req.AppID != "" {
		l.SetAppID(req.AppID)
	}
	s.LegMgr.Add(l)

	if req.RoomID != "" {
		if err := s.RoomMgr.AddLeg(req.RoomID, l.ID()); err != nil {
			s.Log.Warn("whatsapp outbound: add to room failed", "leg_id", l.ID(), "room_id", req.RoomID, "error", err)
		}
	}

	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  string(l.Type()),
	})

	go func() {
		<-call.Dialog.Context().Done()
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "remote_bye")
		}
	}()

	writeJSON(w, http.StatusCreated, legViewFrom(l))
}

// legViewFrom builds a LegView from a Leg. Kept local to avoid coupling the
// WhatsApp handler to other callers that use different field subsets.
func legViewFrom(l leg.Leg) LegView {
	return LegView{
		ID:         l.ID(),
		Type:       l.Type(),
		State:      l.State(),
		RoomID:     l.RoomID(),
		AppID:      l.AppID(),
		Muted:      l.IsMuted(),
		Deaf:       l.IsDeaf(),
		AcceptDTMF: l.AcceptDTMF(),
		Held:       l.IsHeld(),
		SIPHeaders: l.SIPHeaders(),
	}
}

// sipHeadersFromRequest copies X-* headers from an inbound INVITE for
// propagation into the LegRinging event.
func sipHeadersFromRequest(req *sip.Request) map[string]string {
	out := map[string]string{}
	for _, h := range req.Headers() {
		name := h.Name()
		if strings.HasPrefix(strings.ToUpper(name), "X-") {
			out[name] = h.Value()
		}
	}
	return out
}
