package api

import (
	"context"
	"fmt"
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
// The SDP offer describes ICE+DTLS-SRTP+Opus; we feed it into a PCMedia,
// gather local ICE, respond 180 Ringing, register a WhatsAppLeg in "ringing"
// state and block until REST issues POST /v1/legs/{id}/answer.
func (s *Server) handleWhatsAppInbound(call *sipmod.InboundCall) {
	ctx := call.Dialog.Context()
	t0 := time.Now()
	callID := ""
	if c := call.Request.CallID(); c != nil {
		callID = c.Value()
	}
	s.Log.Info("whatsapp inbound: INVITE received", "call_id", callID, "from", call.From, "to", call.To)

	// 100 Trying → tell Meta we have the INVITE so their Timer B stops
	// retransmitting while we set up media.
	s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusTrying, "Trying", nil, s.SIPEngine.ServerHeader())
	if err := call.Dialog.Respond(sip.StatusTrying, "Trying", nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Warn("whatsapp inbound: respond 100 failed", "call_id", callID, "error", err)
	}

	// Dump the inbound INVITE's SDP so we can see Meta's offer (codec PTs,
	// ICE credentials, DTLS fingerprint, setup role, candidates).
	s.Log.Info("whatsapp inbound: remote SDP offer", "call_id", callID, "sdp", "\n"+string(call.Request.Body()))

	var legPtr *leg.WhatsAppLeg
	media, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:      codec.CodecOpus,
		ICEServers: s.Config.ICEServers,
		RTPPortMin: uint16(s.Config.RTPPortMin),
		RTPPortMax: uint16(s.Config.RTPPortMax),
		Log:        s.Log,
		// Meta's SDP is ice-lite + setup:actpass. ice-lite peers don't
		// initiate DTLS, so we must be the DTLS client (a=setup:active).
		AnsweringDTLSRole: webrtc.DTLSRoleClient,
		// Meta's offer advertises telephone-event/8000 at PT 126 for
		// DTMF. Register it in pion's MediaEngine so the answer
		// advertises it and inbound PT 126 packets reach handleTrack.
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
	// Dump pion's generated answer SDP before we apply it — lets us see
	// exactly which codecs pion selected, which is decisive when
	// SetLocalDescription fails with "codec is not supported by remote".
	s.Log.Info("whatsapp inbound: generated answer SDP", "call_id", callID, "sdp", "\n"+answer.SDP)
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.Log.Error("whatsapp inbound: SetLocalDescription", "call_id", callID, "error", err)
		media.Close()
		s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusInternalServerError, "Answer Failed", nil, s.SIPEngine.ServerHeader())
		_ = call.Dialog.Respond(sip.StatusInternalServerError, "Answer Failed", nil, s.SIPEngine.ServerHeader())
		return
	}

	// Inventory pion's view of the PC: one entry per transceiver with its
	// direction, mid and the receiver's codec/ssrc. If the inbound Opus
	// track isn't wired here, OnTrack will never fire and we won't receive
	// audio no matter what the network does.
	for i, tr := range pc.GetTransceivers() {
		dir := tr.Direction().String()
		mid := tr.Mid()
		var recvCodec, recvSSRC string
		if r := tr.Receiver(); r != nil {
			if t := r.Track(); t != nil {
				recvCodec = t.Codec().MimeType
				recvSSRC = fmt.Sprintf("%d", t.SSRC())
			}
		}
		s.Log.Info("whatsapp inbound: transceiver", "call_id", callID, "idx", i, "direction", dir, "mid", mid, "recv_codec", recvCodec, "recv_ssrc", recvSSRC)
	}

	// Send 180 Ringing immediately — ICE gathering can take several seconds
	// on hosts with multiple interfaces, and Meta must see a provisional
	// response well before Timer B expires.
	s.SIPEngine.LogSyntheticResponse(call.Request, sip.StatusRinging, "Ringing", nil, s.SIPEngine.ServerHeader())
	if err := call.Dialog.Respond(sip.StatusRinging, "Ringing", nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Error("whatsapp inbound: respond 180", "call_id", callID, "error", err)
		media.Close()
		return
	}
	s.Log.Info("whatsapp inbound: 180 Ringing sent", "call_id", callID, "elapsed", time.Since(t0))

	// Block until ICE gathering finishes so the 200 OK carries a complete SDP
	// (Meta does not support re-INVITE / trickle ICE over SIP).
	gatherStart := time.Now()
	select {
	case <-gatherDone:
		s.Log.Info("whatsapp inbound: ICE gathering complete", "call_id", callID, "took", time.Since(gatherStart))
	case <-ctx.Done():
		s.Log.Warn("whatsapp inbound: ctx cancelled during ICE gathering", "call_id", callID, "took", time.Since(gatherStart))
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
		elapsed := time.Since(t0)
		s.Log.Info("whatsapp inbound: sending 200 OK", "call_id", callID, "leg_id", l.ID(), "elapsed_since_invite", elapsed, "sdp_bytes", len(finalSDP))
		if elapsed > 30*time.Second {
			s.Log.Warn("whatsapp inbound: answer is past Meta Timer B (~32s) — likely transaction-terminated", "call_id", callID, "leg_id", l.ID(), "elapsed", elapsed)
		}
		if err := l.Answer(context.Background()); err != nil {
			s.Log.Error("whatsapp inbound: answer failed", "leg_id", l.ID(), "call_id", callID, "elapsed", elapsed, "error", err)
			s.cleanupLeg(l)
			return
		}
		s.Log.Info("whatsapp inbound: 200 OK sent", "call_id", callID, "leg_id", l.ID())
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
		cause := context.Cause(ctx)
		s.Log.Info("whatsapp inbound: dialog ctx done (post-answer)", "call_id", callID, "leg_id", l.ID(), "cause", fmt.Sprintf("%v", cause), "elapsed_since_invite", time.Since(t0))
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "remote_bye")
		}
	case <-ctx.Done():
		cause := context.Cause(ctx)
		s.Log.Warn("whatsapp inbound: dialog ended before answer (caller cancelled or Timer B)", "call_id", callID, "leg_id", l.ID(), "cause", fmt.Sprintf("%v", cause), "elapsed", time.Since(t0))
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
