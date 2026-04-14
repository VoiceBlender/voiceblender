package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
	"github.com/go-chi/chi/v5"
)

type transferDirection int

const (
	transferOutbound transferDirection = iota // we sent REFER, awaiting NOTIFY
)

// transferState records what we need to know about an outstanding transfer
// when subsequent NOTIFY sipfrag messages arrive on the same dialog.
type transferState struct {
	legID         string
	replacesLegID string
	target        string
	replacesLeg   *leg.SIPLeg
	direction     transferDirection
}

// transferStore is a small Call-ID → transferState map. Outbound transfers
// only — inbound REFERs are handled inline because their progress is driven
// by our own outbound origination, not by a NOTIFY we receive.
type transferStore struct {
	mu sync.Mutex
	m  map[string]*transferState
}

func newTransferStore() *transferStore {
	return &transferStore{m: make(map[string]*transferState)}
}

func (t *transferStore) set(callID string, st *transferState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[callID] = st
}

func (t *transferStore) get(callID string) (*transferState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.m[callID]
	return st, ok
}

func (t *transferStore) del(callID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, callID)
}

// transferLeg implements POST /v1/legs/{id}/transfer — sends a SIP REFER on
// the leg's existing dialog asking the peer to transfer to the target URI.
// Blind transfer when ReplacesLegID is empty; attended otherwise.
func (s *Server) transferLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req TransferRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	if req.Target == "" {
		writeError(w, http.StatusBadRequest, "missing target")
		return
	}
	target := sip.Uri{}
	if err := sip.ParseUri(req.Target, &target); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid target URI: %v", err))
		return
	}

	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		writeError(w, http.StatusConflict, "transfer is supported only on SIP legs")
		return
	}
	if sl.State() != leg.StateConnected {
		writeError(w, http.StatusConflict, "leg must be connected to transfer")
		return
	}

	kind := "blind"
	var replaces *sipmod.ReplacesParams
	var replacesLeg *leg.SIPLeg
	if req.ReplacesLegID != "" {
		other, found := s.LegMgr.Get(req.ReplacesLegID)
		if !found {
			writeError(w, http.StatusConflict, "replaces_leg_id not found")
			return
		}
		osl, isSip := other.(*leg.SIPLeg)
		if !isSip || osl.State() != leg.StateConnected {
			writeError(w, http.StatusConflict, "replaces_leg_id must be a connected SIP leg")
			return
		}
		callID, localTag, remoteTag, ok := osl.DialogIdentity()
		if !ok {
			writeError(w, http.StatusConflict, "replaces_leg_id has no usable dialog identity")
			return
		}
		// Per RFC 3891 the to-tag/from-tag in Replaces are written from the
		// perspective of the party being replaced. From our local view that
		// translates to: from-tag = our local tag, to-tag = our remote tag.
		replaces = &sipmod.ReplacesParams{
			CallID:  callID,
			FromTag: localTag,
			ToTag:   remoteTag,
		}
		kind = "attended"
		replacesLeg = osl
	}

	if err := sl.Transfer(r.Context(), req.Target, replaces); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("send REFER: %v", err))
		return
	}

	s.Bus.Publish(events.LegTransferInitiated, &events.LegTransferInitiatedData{
		LegScope:      events.LegScope{LegID: sl.ID()},
		Kind:          kind,
		Target:        req.Target,
		ReplacesLegID: req.ReplacesLegID,
	})

	// Remember enough state to act on subsequent NOTIFY sipfrag messages
	// addressed to this leg's dialog.
	s.transfers.set(sl.CallID(), &transferState{
		legID:         sl.ID(),
		replacesLegID: req.ReplacesLegID,
		target:        req.Target,
		replacesLeg:   replacesLeg,
		direction:     transferOutbound,
	})

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "transfer_initiated"})
}

// HandleReferNotify is invoked by the SIP engine for every NOTIFY sipfrag
// arriving on a leg that owns an outstanding outbound transfer. It
// translates the sipfrag status into transfer events and, on terminal
// success, hangs up the legs that the transfer obsoleted.
func (s *Server) HandleReferNotify(callID string, statusCode int, reason string, terminated bool) {
	st, ok := s.transfers.get(callID)
	if !ok || st.direction != transferOutbound {
		return
	}
	scope := events.LegScope{LegID: st.legID}

	if !terminated {
		s.Bus.Publish(events.LegTransferProgress, &events.LegTransferProgressData{
			LegScope:   scope,
			StatusCode: statusCode,
			Reason:     reason,
		})
		return
	}

	s.transfers.del(callID)

	if statusCode >= 200 && statusCode < 300 {
		s.Bus.Publish(events.LegTransferCompleted, &events.LegTransferCompletedData{
			LegScope:   scope,
			StatusCode: statusCode,
			Reason:     reason,
		})
		// Standard semantics: once the peer has the new call, the
		// original leg(s) are obsolete. Hang them up.
		if l, ok := s.LegMgr.Get(st.legID); ok {
			if sl, ok := l.(*leg.SIPLeg); ok && sl.State() != leg.StateHungUp {
				s.cleanupLeg(sl)
				s.publishDisconnect(sl, "transfer_completed")
			}
		}
		if st.replacesLeg != nil && st.replacesLeg.State() != leg.StateHungUp {
			s.cleanupLeg(st.replacesLeg)
			s.publishDisconnect(st.replacesLeg, "transfer_completed")
		}
		return
	}

	s.Bus.Publish(events.LegTransferFailed, &events.LegTransferFailedData{
		LegScope:   scope,
		StatusCode: statusCode,
		Reason:     reason,
	})
}

// HandleIncomingRefer is invoked by the SIP engine for every inbound REFER.
// Default-deny: when SIP_REFER_AUTO_DIAL is unset (false) we 603 Decline
// every REFER and emit an audit event so operators can monitor attempts.
func (s *Server) HandleIncomingRefer(callID, target string, replaces *sipmod.ReplacesParams, req *sip.Request, tx sip.ServerTransaction) {
	kind := "blind"
	replacesCallID := ""
	if replaces != nil {
		kind = "attended"
		replacesCallID = replaces.CallID
	}

	sl := s.LegMgr.FindSIPByCallID(callID)
	scope := events.LegScope{}
	if sl != nil {
		scope.LegID = sl.ID()
	}

	if !s.Config.SIPReferAutoDial {
		if tx != nil {
			res := sip.NewResponseFromRequest(req, 603, "Decline", nil)
			tx.Respond(res)
		}
		s.Bus.Publish(events.LegTransferRequested, &events.LegTransferRequestedData{
			LegScope:       scope,
			Kind:           kind,
			Target:         target,
			ReplacesCallID: replacesCallID,
			Declined:       true,
		})
		return
	}

	if sl == nil {
		// We accepted REFER (auto-dial enabled) but can't find the leg
		// to drive NOTIFY against — reject so the peer doesn't wait.
		if tx != nil {
			res := sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)
			tx.Respond(res)
		}
		return
	}

	if tx != nil {
		res := sip.NewResponseFromRequest(req, sip.StatusAccepted, "Accepted", nil)
		tx.Respond(res)
	}
	s.Bus.Publish(events.LegTransferRequested, &events.LegTransferRequestedData{
		LegScope:       scope,
		Kind:           kind,
		Target:         target,
		ReplacesCallID: replacesCallID,
		Declined:       false,
	})

	// Originate the new outbound leg toward target. Wire its lifecycle
	// callbacks to ship NOTIFY sipfrag back to the referrer leg.
	go s.originateForRefer(sl, target, replaces)
}

// originateForRefer dials the REFER target and reports progress to the
// referrer via NOTIFY sipfrag. Called from HandleIncomingRefer.
func (s *Server) originateForRefer(referrer *leg.SIPLeg, target string, replaces *sipmod.ReplacesParams) {
	recipient := sip.Uri{}
	if err := sip.ParseUri(target, &recipient); err != nil {
		s.notifyAndFail(referrer, 400, "Bad Refer-To URI")
		return
	}

	newLeg := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Log)
	newLeg.SetJitterBuffer(s.Config.SIPJitterBufferMs, s.Config.SIPJitterBufferMaxMs)
	s.LegMgr.Add(newLeg)
	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: newLeg.ID()},
		URI:      target,
	})

	// Active subscription: 100 Trying.
	if err := referrer.SendNotifySipfrag(context.Background(), 100, "Trying", false); err != nil {
		s.Log.Warn("transfer NOTIFY 100 failed", "error", err)
	}

	inviteOpts := sipmod.InviteOptions{
		OnEarlyMedia: func(remoteSDP *sipmod.SDPMedia, rtpSess *sipmod.RTPSession) {
			_ = newLeg.SetupEarlyMediaOutbound(remoteSDP, rtpSess)
			referrer.SendNotifySipfrag(context.Background(), 180, "Ringing", false)
		},
	}
	if replaces != nil {
		inviteOpts.Headers = append(inviteOpts.Headers, sip.NewHeader("Replaces", replaces.String()))
	}

	call, err := s.SIPEngine.Invite(context.Background(), recipient, inviteOpts)
	if err != nil {
		s.Log.Info("transfer originate failed", "error", err)
		s.notifyAndFail(referrer, 500, "Server Error")
		s.cleanupLeg(newLeg)
		s.publishDisconnect(newLeg, "transfer_originate_failed")
		return
	}
	if err := newLeg.ConnectOutbound(call); err != nil {
		s.Log.Error("transfer connect failed", "error", err)
		call.RTPSess.Close()
		call.Dialog.Bye(context.Background())
		s.notifyAndFail(referrer, 500, "Server Error")
		s.cleanupLeg(newLeg)
		s.publishDisconnect(newLeg, "transfer_connect_failed")
		return
	}

	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: newLeg.ID()},
		LegType:  string(newLeg.Type()),
	})

	// Final NOTIFY: terminated, 200 OK.
	if err := referrer.SendNotifySipfrag(context.Background(), 200, "OK", true); err != nil {
		s.Log.Warn("transfer NOTIFY 200 failed", "error", err)
	}
	s.Bus.Publish(events.LegTransferCompleted, &events.LegTransferCompletedData{
		LegScope:   events.LegScope{LegID: referrer.ID()},
		StatusCode: 200,
		Reason:     "OK",
	})

	// Hang up the referrer leg — our peer asked to be transferred away.
	s.cleanupLeg(referrer)
	s.publishDisconnect(referrer, "transfer_completed")
}

// notifyAndFail sends a final NOTIFY sipfrag with a non-2xx status and
// publishes a transfer_failed event for the referrer leg.
func (s *Server) notifyAndFail(referrer *leg.SIPLeg, statusCode int, reason string) {
	if referrer != nil {
		referrer.SendNotifySipfrag(context.Background(), statusCode, reason, true)
		s.Bus.Publish(events.LegTransferFailed, &events.LegTransferFailedData{
			LegScope:   events.LegScope{LegID: referrer.ID()},
			StatusCode: statusCode,
			Reason:     reason,
		})
	}
}
