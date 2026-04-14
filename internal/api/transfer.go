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

type transferState struct {
	legID         string
	replacesLegID string
	target        string
	replacesLeg   *leg.SIPLeg
	direction     transferDirection
}

// transferStore maps Call-ID → outbound transferState (for routing NOTIFYs).
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

// transferLeg implements POST /v1/legs/{id}/transfer.
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
	// sipgo's ParseUri accepts "sip:" with no host.
	if target.Host == "" {
		writeError(w, http.StatusBadRequest, "target URI missing host")
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
		// RFC 3891: tags are from the replaced party's view → swap.
		replaces = &sipmod.ReplacesParams{
			CallID:  callID,
			FromTag: localTag,
			ToTag:   remoteTag,
		}
		kind = "attended"
		replacesLeg = osl
	}

	// Registered before Transfer() so NOTIFY arriving immediately after
	// the peer's 202 finds a route entry.
	s.transfers.set(sl.CallID(), &transferState{
		legID:         sl.ID(),
		replacesLegID: req.ReplacesLegID,
		target:        req.Target,
		replacesLeg:   replacesLeg,
		direction:     transferOutbound,
	})

	// Some peers skip 202 Accepted and signal only via NOTIFY; emit
	// transfer_initiated before Do() so the event fires regardless.
	s.Bus.Publish(events.LegTransferInitiated, &events.LegTransferInitiatedData{
		LegScope:      events.LegScope{LegID: sl.ID()},
		Kind:          kind,
		Target:        req.Target,
		ReplacesLegID: req.ReplacesLegID,
	})

	// Use the leg's context, not r.Context() (HTTP request closes early).
	go func() {
		callID := sl.CallID()
		if err := sl.Transfer(sl.Context(), req.Target, replaces); err != nil {
			// NOTIFY-terminated may have resolved this already.
			if _, stillPending := s.transfers.get(callID); !stillPending {
				s.Log.Debug("transfer REFER returned after NOTIFY-terminated resolution", "leg_id", sl.ID(), "error", err)
				return
			}
			s.Log.Info("transfer REFER failed", "leg_id", sl.ID(), "error", err)
			s.transfers.del(callID)
			s.Bus.Publish(events.LegTransferFailed, &events.LegTransferFailedData{
				LegScope: events.LegScope{LegID: sl.ID()},
				Error:    err.Error(),
			})
			return
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "transfer_initiated"})
}

// HandleReferNotify dispatches NOTIFY sipfrag updates for outbound transfers.
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

// HandleIncomingRefer handles inbound REFER. Default-deny via SIP_REFER_AUTO_DIAL.
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
			if err := s.SIPEngine.RespondFromSource(tx, req, 603, "Decline"); err != nil {
				s.Log.Error("REFER respond 603 failed", "error", err)
			}
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
		// Auto-dial on but no leg matches — can't NOTIFY back, so reject.
		if tx != nil {
			if err := s.SIPEngine.RespondFromSource(tx, req, 481, "Call/Transaction Does Not Exist"); err != nil {
				s.Log.Error("REFER respond 481 failed", "error", err)
			}
		}
		return
	}

	if tx != nil {
		if err := s.SIPEngine.RespondFromSource(tx, req, sip.StatusAccepted, "Accepted"); err != nil {
			s.Log.Error("REFER respond 202 failed", "error", err)
		} else {
			s.Log.Info("REFER accepted with 202", "leg_id", sl.ID(), "target", target)
		}
	}
	s.Bus.Publish(events.LegTransferRequested, &events.LegTransferRequestedData{
		LegScope:       scope,
		Kind:           kind,
		Target:         target,
		ReplacesCallID: replacesCallID,
		Declined:       false,
	})

	go s.originateForRefer(sl, target, replaces)
}

// originateForRefer dials the REFER target and NOTIFYs sipfrag back to referrer.
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

	if err := referrer.SendNotifySipfrag(context.Background(), 200, "OK", true); err != nil {
		s.Log.Warn("transfer NOTIFY 200 failed", "error", err)
	}
	s.Bus.Publish(events.LegTransferCompleted, &events.LegTransferCompletedData{
		LegScope:   events.LegScope{LegID: referrer.ID()},
		StatusCode: 200,
		Reason:     "OK",
	})

	s.cleanupLeg(referrer)
	s.publishDisconnect(referrer, "transfer_completed")
}

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
