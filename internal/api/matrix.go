package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/matrix"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	mautrix "maunium.net/go/mautrix"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func defaultMatrixLifetime(reqMs, cfgMs int) int {
	if reqMs > 0 {
		return reqMs
	}
	if cfgMs > 0 {
		return cfgMs
	}
	return 60000
}

// parseMatrixDestination resolves the `to` field of a matrix leg request to
// a Matrix room id. Accepted forms:
//
//   - "!abc:server"                         — raw room id
//   - "#name:server"                        — room alias (resolved via /directory/room)
//   - "matrix:roomid/abc:server[?query]"    — MSC2312 URI for a room id
//   - "matrix:r/name:server[?query]"        — MSC2312 URI for an alias
//
// Aliases trigger one HTTP round-trip against the homeserver. Query strings
// on the matrix: URI are accepted (MSC2312 defines `via=`) and ignored —
// VoiceBlender does not need federation routing hints at the client layer.
func parseMatrixDestination(ctx context.Context, homeserverURL, accessToken, userID, to string) (id.RoomID, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return "", errors.New("'to' is required for matrix legs (room id, alias, or matrix: URI)")
	}
	switch {
	case strings.HasPrefix(to, "!"):
		return id.RoomID(to), nil
	case strings.HasPrefix(to, "#"):
		return resolveMatrixAlias(ctx, homeserverURL, accessToken, userID, id.RoomAlias(to))
	case strings.HasPrefix(to, "matrix:"):
		rest := strings.TrimPrefix(to, "matrix:")
		if i := strings.Index(rest, "?"); i >= 0 {
			rest = rest[:i]
		}
		switch {
		case strings.HasPrefix(rest, "roomid/"):
			return id.RoomID("!" + strings.TrimPrefix(rest, "roomid/")), nil
		case strings.HasPrefix(rest, "r/"):
			return resolveMatrixAlias(ctx, homeserverURL, accessToken, userID,
				id.RoomAlias("#"+strings.TrimPrefix(rest, "r/")))
		default:
			return "", fmt.Errorf("unsupported matrix URI %q (expected matrix:roomid/... or matrix:r/...)", to)
		}
	default:
		return "", fmt.Errorf("invalid matrix destination %q (expected room id \"!abc:server\", alias \"#name:server\", or matrix: URI)", to)
	}
}

func resolveMatrixAlias(ctx context.Context, homeserverURL, accessToken, userID string, alias id.RoomAlias) (id.RoomID, error) {
	mx, err := mautrix.NewClient(homeserverURL, id.UserID(userID), accessToken)
	if err != nil {
		return "", fmt.Errorf("resolve alias: new mautrix client: %w", err)
	}
	resp, err := mx.ResolveAlias(ctx, alias)
	if err != nil {
		return "", fmt.Errorf("resolve alias %s: %w", alias, err)
	}
	return resp.RoomID, nil
}

// createMatrixOutboundLeg implements POST /v1/legs with type=matrix.
// Account-level credentials (homeserver_url, access_token, matrix_user_id,
// matrix_device_id) fall back to the corresponding MATRIX_* env vars when
// the request body omits them. The destination room is encoded in the
// existing `to` field — see parseMatrixDestination for accepted forms.
func (s *Server) createMatrixOutboundLeg(w http.ResponseWriter, r *http.Request, req CreateLegRequest) {
	homeserverURL := req.HomeserverURL
	if homeserverURL == "" {
		homeserverURL = s.Config.MatrixHomeserverURL
	}
	accessToken := req.AccessToken
	if accessToken == "" {
		accessToken = s.Config.MatrixAccessToken
	}
	matrixUserID := req.MatrixUserID
	if matrixUserID == "" {
		matrixUserID = s.Config.MatrixUserID
	}
	matrixDeviceID := req.MatrixDeviceID
	if matrixDeviceID == "" {
		matrixDeviceID = s.Config.MatrixDeviceID
	}

	if homeserverURL == "" {
		writeError(w, http.StatusBadRequest, "'homeserver_url' is required (in request body or via MATRIX_HOMESERVER_URL env var)")
		return
	}
	if accessToken == "" {
		writeError(w, http.StatusBadRequest, "'access_token' is required (in request body or via MATRIX_ACCESS_TOKEN env var)")
		return
	}
	if matrixUserID == "" {
		writeError(w, http.StatusBadRequest, "'matrix_user_id' is required (in request body or via MATRIX_USER_ID env var)")
		return
	}
	matrixRoomID, err := parseMatrixDestination(r.Context(), homeserverURL, accessToken, matrixUserID, req.To)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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

	client, err := matrix.NewClient(matrix.ClientConfig{
		HomeserverURL: homeserverURL,
		UserID:        id.UserID(matrixUserID),
		DeviceID:      id.DeviceID(matrixDeviceID),
		AccessToken:   accessToken,
		RoomID:        matrixRoomID,
		Log:           s.Log,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "matrix client: "+err.Error())
		return
	}

	var legPtr *leg.MatrixLeg
	media, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:                codec.CodecOpus,
		ICEServers:           s.Config.ICEServers,
		RTPPortMin:           uint16(s.Config.RTPPortMin),
		RTPPortMax:           uint16(s.Config.RTPPortMax),
		Log:                  s.Log,
		EnableTelephoneEvent: true,
		OnDisconnect: func(reason string) {
			if legPtr != nil && legPtr.State() != leg.StateHungUp {
				s.cleanupLeg(legPtr)
				s.publishDisconnect(legPtr, "ice_"+reason)
			}
		},
	})
	if err != nil {
		_ = client.Close()
		writeError(w, http.StatusInternalServerError, "create PCMedia: "+err.Error())
		return
	}

	pc := media.PC()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = client.Close()
		_ = media.Close()
		writeError(w, http.StatusInternalServerError, "create offer: "+err.Error())
		return
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = client.Close()
		_ = media.Close()
		writeError(w, http.StatusInternalServerError, "set local description: "+err.Error())
		return
	}

	callID := uuid.New().String()
	partyID := req.PartyID
	if partyID == "" {
		partyID = uuid.New().String()
	}

	l := leg.NewMatrixOutboundPendingLeg(leg.MatrixLegConfig{
		Media:        media,
		Sender:       client,
		MatrixRoomID: matrixRoomID,
		CallID:       callID,
		PartyID:      partyID,
		Log:          s.Log,
	})
	legPtr = l
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

	// Start sync (so AwaitAnswer can hear the remote answer) before sending the invite.
	if err := client.Start(l.Context()); err != nil {
		s.cleanupLeg(l)
		_ = client.Close()
		writeError(w, http.StatusBadGateway, "matrix sync: "+err.Error())
		return
	}

	l.SetOnRemoteHangup(func(reason string) {
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "remote_hangup_"+reason)
		}
	})

	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  string(l.Type()),
		URI:      string(matrixRoomID),
		From:     matrixUserID,
	})

	writeJSON(w, http.StatusCreated, toLegView(l))

	go s.driveMatrixOutbound(l, media, client, gatherDone, req, matrixRoomID, callID, partyID)
}

func (s *Server) driveMatrixOutbound(
	l *leg.MatrixLeg,
	media *leg.PCMedia,
	client *matrix.Client,
	gatherDone <-chan struct{},
	req CreateLegRequest,
	matrixRoomID id.RoomID,
	callID, partyID string,
) {
	select {
	case <-gatherDone:
	case <-l.Context().Done():
		_ = client.Close()
		return
	}
	offerSDP := media.PC().LocalDescription().SDP
	lifetimeMs := defaultMatrixLifetime(req.MatrixLifetimeMs, s.Config.MatrixCallLifetimeMs)

	invite := &mevent.CallInviteEventContent{
		BaseCallEventContent: mevent.BaseCallEventContent{
			CallID:  callID,
			PartyID: partyID,
			Version: "1",
		},
		Lifetime: lifetimeMs,
		Offer: mevent.CallData{
			Type: mevent.CallDataTypeOffer,
			SDP:  offerSDP,
		},
	}

	// Subscribe BEFORE sending so a fast answer is not lost.
	awaitCtx, cancel := context.WithTimeout(l.Context(), time.Duration(lifetimeMs)*time.Millisecond)
	defer cancel()

	if err := client.SendInvite(l.Context(), invite); err != nil {
		s.Log.Info("matrix outbound: send invite failed", "leg_id", l.ID(), "error", err)
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "invite_failed")
		}
		_ = client.Close()
		return
	}

	// AwaitAnswer must own the dispatcher channel exclusively until the
	// answer arrives. Starting the leg's pump now would race AwaitAnswer
	// for events: the pump's remote-event goroutine would read the answer
	// off the channel and simply record remotePartyID without driving
	// ConnectOutbound, leaving AwaitAnswer to time out. The pump starts
	// only after ConnectOutbound has applied the SDP answer.
	answer, _, err := client.AwaitAnswer(awaitCtx, callID)
	if err != nil {
		reason := "invite_failed"
		if errors.Is(err, context.DeadlineExceeded) {
			// On lifetime expiry the caller is expected to send hangup
			// with reason=invite_timeout per MSC2746.
			_ = client.SendHangup(context.Background(), matrixRoomID, &mevent.CallHangupEventContent{
				BaseCallEventContent: mevent.BaseCallEventContent{
					CallID:  callID,
					PartyID: partyID,
					Version: "1",
				},
				Reason: mevent.CallHangupInviteTimeout,
			})
			reason = "ring_timeout"
		}
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, reason)
		}
		_ = client.Close()
		return
	}

	if err := l.ConnectOutbound(answer.PartyID, "", answer.Answer.SDP); err != nil {
		s.Log.Error("matrix outbound: ConnectOutbound", "leg_id", l.ID(), "error", err)
		_ = client.SendHangup(context.Background(), matrixRoomID, &mevent.CallHangupEventContent{
			BaseCallEventContent: mevent.BaseCallEventContent{
				CallID:  callID,
				PartyID: partyID,
				Version: "1",
			},
			Reason: mevent.CallHangupUnknownError,
		})
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "bad_answer")
		}
		_ = client.Close()
		return
	}

	// Now that the answer has been applied (SetRemoteDescription succeeded),
	// hand the dispatcher channel to the leg's pump: it will drain any
	// gathered local candidates outbound and apply remote candidates as
	// they arrive.
	l.StartCandidatePump(l.Context())

	if req.RoomID != "" {
		if err := s.RoomMgr.AddLeg(req.RoomID, l.ID()); err != nil {
			s.Log.Warn("matrix outbound: add to room failed", "leg_id", l.ID(), "room_id", req.RoomID, "error", err)
		} else {
			s.onLegJoinedRoom(req.RoomID, l.ID())
		}
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
	s.maybeStartSpeakingDetector(l, req.SpeechDetection)

	<-l.Context().Done()
	_ = client.Close()
	if l.State() != leg.StateHungUp {
		s.cleanupLeg(l)
		s.publishDisconnect(l, "remote_bye")
	}
}

// HandleMatrixInbound is the Listener.InboundHandler. The Listener invokes
// this on its own goroutine for every fresh m.call.invite.
func (s *Server) HandleMatrixInbound(ctx context.Context, ev *matrix.CallEvent, sender matrix.EventSender) {
	if ev == nil || ev.Invite == nil {
		return
	}
	invite := ev.Invite

	var legPtr *leg.MatrixLeg
	media, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:                codec.CodecOpus,
		ICEServers:           s.Config.ICEServers,
		RTPPortMin:           uint16(s.Config.RTPPortMin),
		RTPPortMax:           uint16(s.Config.RTPPortMax),
		Log:                  s.Log,
		EnableTelephoneEvent: true,
		OnDisconnect: func(reason string) {
			if legPtr != nil && legPtr.State() != leg.StateHungUp {
				s.cleanupLeg(legPtr)
				s.publishDisconnect(legPtr, "ice_"+reason)
			}
		},
	})
	if err != nil {
		s.Log.Error("matrix inbound: create PCMedia", "call_id", invite.CallID, "error", err)
		return
	}

	pc := media.PC()
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  invite.Offer.SDP,
	}); err != nil {
		s.Log.Error("matrix inbound: SetRemoteDescription", "call_id", invite.CallID, "error", err)
		_ = media.Close()
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.Log.Error("matrix inbound: CreateAnswer", "call_id", invite.CallID, "error", err)
		_ = media.Close()
		return
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.Log.Error("matrix inbound: SetLocalDescription", "call_id", invite.CallID, "error", err)
		_ = media.Close()
		return
	}

	select {
	case <-gatherDone:
	case <-ctx.Done():
		_ = media.Close()
		return
	}
	answerSDP := pc.LocalDescription().SDP

	l := leg.NewMatrixInboundLeg(leg.MatrixLegConfig{
		Media:         media,
		Sender:        sender,
		MatrixRoomID:  ev.RoomID,
		CallID:        invite.CallID,
		PartyID:       uuid.New().String(),
		RemoteUserID:  ev.Sender,
		RemotePartyID: invite.PartyID,
		AnswerSDP:     answerSDP,
		Log:           s.Log,
	})
	legPtr = l
	s.LegMgr.Add(l)

	l.SetOnRemoteHangup(func(reason string) {
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "remote_hangup_"+reason)
		}
	})

	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  string(l.Type()),
		From:     string(ev.Sender),
		To:       string(s.MatrixListener.UserID()),
	})

	// Start the candidate pump immediately so trickled candidates from
	// the caller are accepted even before we answer.
	l.StartCandidatePump(ctx)

	lifetime := time.Duration(invite.Lifetime) * time.Millisecond
	if lifetime <= 0 {
		lifetime = time.Duration(s.Config.MatrixCallLifetimeMs) * time.Millisecond
	}
	if lifetime <= 0 {
		lifetime = 60 * time.Second
	}
	timer := time.NewTimer(lifetime)
	defer timer.Stop()

	select {
	case <-l.AnswerCh():
		if err := l.Answer(context.Background()); err != nil {
			s.Log.Error("matrix inbound: answer failed", "leg_id", l.ID(), "call_id", invite.CallID, "error", err)
			s.cleanupLeg(l)
			s.publishDisconnect(l, "answer_failed")
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
		<-l.Context().Done()
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "remote_bye")
		}
	case <-timer.C:
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "invite_timeout")
		}
	case <-ctx.Done():
		if l.State() != leg.StateHungUp {
			s.cleanupLeg(l)
			s.publishDisconnect(l, "caller_cancel")
		}
	}
}
