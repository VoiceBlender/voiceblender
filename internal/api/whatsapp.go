package api

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
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
// Media is ICE + DTLS-SRTP + Opus via PCMedia. ICE gathering blocks before
// 180 because Meta does not support re-INVITE / trickle.
func (s *Server) handleWhatsAppInbound(call *sipmod.InboundCall) {
	ctx := call.Dialog.Context()
	callID := ""
	if c := call.Request.CallID(); c != nil {
		callID = c.Value()
	}
	s.Log.Info("whatsapp inbound: INVITE received", "call_id", callID, "from", call.From, "to", call.To)

	s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusTrying, "Trying", nil, s.SIPEngine.ServerHeader())
	if err := call.Dialog.Respond(sip.StatusTrying, "Trying", nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Warn("whatsapp inbound: respond 100 failed", "call_id", callID, "error", err)
	}

	var legPtr *leg.WhatsAppLeg
	media, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:      codec.CodecOpus,
		ICEServers: s.Config.ICEServers,
		RTPPortMin: uint16(s.Config.RTPPortMin),
		RTPPortMax: uint16(s.Config.RTPPortMax),
		Log:        s.Log,
		// Meta is ice-lite + setup:actpass; ice-lite peers don't initiate
		// DTLS, so we must be the DTLS client.
		AnsweringDTLSRole:    webrtc.DTLSRoleClient,
		EnableTelephoneEvent: true,
		OnDisconnect: func(reason string) {
			s.Log.Warn("whatsapp inbound: ICE disconnect", "call_id", callID, "reason", reason)
			if legPtr != nil && legPtr.State() != leg.StateHungUp {
				s.cleanupLeg(legPtr)
				s.publishDisconnect(legPtr, "ice_"+reason)
			}
		},
	})
	if err != nil {
		s.Log.Error("whatsapp inbound: create PCMedia", "call_id", callID, "error", err)
		s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusInternalServerError, "Media Setup Failed", nil, s.SIPEngine.ServerHeader())
		_ = call.Dialog.Respond(sip.StatusInternalServerError, "Media Setup Failed", nil, s.SIPEngine.ServerHeader())
		return
	}

	pc := media.PC()
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(call.Request.Body())}
	if err := pc.SetRemoteDescription(offer); err != nil {
		s.Log.Error("whatsapp inbound: SetRemoteDescription", "call_id", callID, "error", err)
		media.Close()
		s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusBadRequest, "Bad SDP Offer", nil, s.SIPEngine.ServerHeader())
		_ = call.Dialog.Respond(sip.StatusBadRequest, "Bad SDP Offer", nil, s.SIPEngine.ServerHeader())
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.Log.Error("whatsapp inbound: CreateAnswer", "call_id", callID, "error", err)
		media.Close()
		s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusInternalServerError, "Answer Failed", nil, s.SIPEngine.ServerHeader())
		_ = call.Dialog.Respond(sip.StatusInternalServerError, "Answer Failed", nil, s.SIPEngine.ServerHeader())
		return
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.Log.Error("whatsapp inbound: SetLocalDescription", "call_id", callID, "error", err)
		media.Close()
		s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusInternalServerError, "Answer Failed", nil, s.SIPEngine.ServerHeader())
		_ = call.Dialog.Respond(sip.StatusInternalServerError, "Answer Failed", nil, s.SIPEngine.ServerHeader())
		return
	}

	s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusRinging, "Ringing", nil, s.SIPEngine.ServerHeader())
	if err := call.Dialog.Respond(sip.StatusRinging, "Ringing", nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Error("whatsapp inbound: respond 180", "call_id", callID, "error", err)
		media.Close()
		return
	}

	select {
	case <-gatherDone:
	case <-ctx.Done():
		media.Close()
		return
	}
	finalSDP := []byte(pc.LocalDescription().SDP)

	headers := sipHeadersFromRequest(call.Request)
	l := leg.NewWhatsAppInboundLeg(call.Dialog, media, call.From, call.To, headers, finalSDP, s.Log)
	legPtr = l
	l.SetSIPResponseLogger(s.SIPEngine)
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
			s.Log.Error("whatsapp inbound: answer failed", "leg_id", l.ID(), "call_id", callID, "error", err)
			s.cleanupLeg(l)
			return
		}
		s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
			LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
			LegType:  string(l.Type()),
		})
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
		s.maybeStartSpeakingDetector(l, s.takeSpeechOverride(l.ID()))
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

	recipient := sipmod.WhatsAppRecipientURI(req.To)

	inviteCtx, cancel := context.WithTimeout(context.Background(), whatsAppInviteTimeout)
	defer cancel()

	call, err := s.SIPEngine.InviteWhatsApp(inviteCtx, recipient, sipmod.WhatsAppInviteOptions{
		FromNumber: req.From,
		Password:   req.Password,
		SDPOffer:   sdpOffer,
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
