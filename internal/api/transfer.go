package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

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

// TransferLegResult is the success payload for initiating a transfer.
type TransferLegResult struct {
	Status string `json:"status"`
}

func (s *Server) doTransferLeg(legID string, req TransferRequest) (*TransferLegResult, error) {
	if req.Target == "" {
		return nil, newAPIError(http.StatusBadRequest, "missing target")
	}
	target := sip.Uri{}
	if err := sip.ParseUri(req.Target, &target); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "invalid target URI: %v", err)
	}
	if target.Host == "" {
		return nil, newAPIError(http.StatusBadRequest, "target URI missing host")
	}
	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	if _, ok := l.(*leg.WhatsAppLeg); ok {
		return nil, newAPIError(http.StatusConflict, "transfer is not supported for WhatsApp legs (Meta disallows REFER)")
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		return nil, newAPIError(http.StatusConflict, "transfer is supported only on SIP legs")
	}
	if sl.State() != leg.StateConnected {
		return nil, newAPIError(http.StatusConflict, "leg must be connected to transfer")
	}

	kind := "blind"
	var replaces *sipmod.ReplacesParams
	var replacesLeg *leg.SIPLeg
	if req.ReplacesLegID != "" {
		other, found := s.LegMgr.Get(req.ReplacesLegID)
		if !found {
			return nil, newAPIError(http.StatusConflict, "replaces_leg_id not found")
		}
		osl, isSip := other.(*leg.SIPLeg)
		if !isSip || osl.State() != leg.StateConnected {
			return nil, newAPIError(http.StatusConflict, "replaces_leg_id must be a connected SIP leg")
		}
		callID, localTag, remoteTag, ok := osl.DialogIdentity()
		if !ok {
			return nil, newAPIError(http.StatusConflict, "replaces_leg_id has no usable dialog identity")
		}
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

	s.Bus.Publish(events.LegTransferInitiated, &events.LegTransferInitiatedData{
		LegScope:      events.LegScope{LegID: sl.ID(), AppID: sl.AppID()},
		Kind:          kind,
		Target:        req.Target,
		ReplacesLegID: req.ReplacesLegID,
	})

	go func() {
		callID := sl.CallID()
		if err := sl.Transfer(sl.Context(), req.Target, replaces); err != nil {
			if _, stillPending := s.transfers.get(callID); !stillPending {
				s.Log.Debug("transfer REFER returned after NOTIFY-terminated resolution", "leg_id", sl.ID(), "error", err)
				return
			}
			s.Log.Info("transfer REFER failed", "leg_id", sl.ID(), "error", err)
			s.transfers.del(callID)
			s.Bus.Publish(events.LegTransferFailed, &events.LegTransferFailedData{
				LegScope: events.LegScope{LegID: sl.ID(), AppID: sl.AppID()},
				Error:    err.Error(),
			})
			return
		}
	}()

	return &TransferLegResult{Status: "transfer_initiated"}, nil
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
	res, err := s.doTransferLeg(id, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// HandleReferNotify dispatches NOTIFY sipfrag updates for outbound transfers.
func (s *Server) HandleReferNotify(callID string, statusCode int, reason string, terminated bool) {
	st, ok := s.transfers.get(callID)
	if !ok || st.direction != transferOutbound {
		return
	}
	scope := events.LegScope{LegID: st.legID}
	if l, ok := s.LegMgr.Get(st.legID); ok {
		scope.AppID = l.AppID()
	}

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

// HandleIncomingRefer handles inbound REFER. With SIP_REFER_AUTO_DIAL=true the
// server accepts (202) and originates the target itself (legacy path). With it
// false (default) the REFER is parked for an app decision: leg.transfer_requested
// is emitted and the app drives the outcome via the transfer commands
// (accept/progress/complete/decline). If no decision arrives within
// SIP_REFER_CONSULT_TIMEOUT_MS the REFER auto-declines with 603 (fail-closed).
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
		scope.AppID = sl.AppID()
	}

	// Legacy server-driven auto-dial: accept and originate the target here.
	if s.Config.SIPReferAutoDial {
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
		return
	}

	// App-driven consult: park and ask the app to decide. Without a referrer leg
	// we can't drive the NOTIFY subscription, and without a transaction we can't
	// respond — decline in either case.
	if sl == nil || tx == nil {
		if tx != nil {
			if err := s.SIPEngine.RespondFromSource(tx, req, 603, "Decline"); err != nil {
				s.Log.Error("REFER respond 603 failed", "error", err)
			}
		}
		return
	}

	pr := &pendingRefer{
		legID:    sl.ID(),
		callID:   callID,
		referrer: sl,
		req:      req,
		tx:       tx,
		target:   target,
		replaces: replaces,
		kind:     kind,
	}
	timeout := time.Duration(s.Config.SIPReferConsultTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	pr.timer = time.AfterFunc(timeout, func() { s.declineReferOnTimeout(sl.ID()) })
	s.pendingRefers.put(pr)

	s.Bus.Publish(events.LegTransferRequested, &events.LegTransferRequestedData{
		LegScope:       scope,
		Kind:           kind,
		Target:         target,
		ReplacesCallID: replacesCallID,
		Declined:       false,
	})
}

// originateForRefer dials the REFER target and NOTIFYs sipfrag back to referrer.
func (s *Server) originateForRefer(referrer *leg.SIPLeg, target string, replaces *sipmod.ReplacesParams) {
	recipient := sip.Uri{}
	if err := sip.ParseUri(target, &recipient); err != nil {
		s.notifyAndFail(referrer, 400, "Bad Refer-To URI")
		return
	}

	newLeg := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Tracer, s.Log)
	newLeg.SetJitterBuffer(s.Config.SIPJitterBufferMs, s.Config.SIPJitterBufferMaxMs)
	s.LegMgr.Add(newLeg)
	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope: events.LegScope{LegID: newLeg.ID(), AppID: newLeg.AppID()},
		LegType:  string(newLeg.Type()),
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
		LegScope: events.LegScope{LegID: newLeg.ID(), AppID: newLeg.AppID()},
		LegType:  string(newLeg.Type()),
	})

	if err := referrer.SendNotifySipfrag(context.Background(), 200, "OK", true); err != nil {
		s.Log.Warn("transfer NOTIFY 200 failed", "error", err)
	}
	s.Bus.Publish(events.LegTransferCompleted, &events.LegTransferCompletedData{
		LegScope:   events.LegScope{LegID: referrer.ID(), AppID: referrer.AppID()},
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
			LegScope:   events.LegScope{LegID: referrer.ID(), AppID: referrer.AppID()},
			StatusCode: statusCode,
			Reason:     reason,
		})
	}
}

// ── App-driven inbound REFER (Option A: park + accept/progress/complete/decline) ──

// pendingRefer is a parked inbound REFER awaiting an app decision. The referrer
// leg's id is the correlation key. tx/req are held so the 202 or 6xx can be sent
// once the app decides; referrer drives the sipfrag NOTIFY subscription.
type pendingRefer struct {
	legID    string
	callID   string
	referrer *leg.SIPLeg
	req      *sip.Request
	tx       sip.ServerTransaction
	target   string
	replaces *sipmod.ReplacesParams
	kind     string
	accepted bool // true once accept_transfer has sent the 202
	timer    *time.Timer
}

// pendingReferStore holds parked REFERs keyed by referrer leg id. All state
// transitions run under the mutex so the consult-timeout race (auto-decline vs.
// app-accept) resolves to exactly one outcome.
type pendingReferStore struct {
	mu sync.Mutex
	m  map[string]*pendingRefer
}

func newPendingReferStore() *pendingReferStore {
	return &pendingReferStore{m: make(map[string]*pendingRefer)}
}

func (s *pendingReferStore) put(pr *pendingRefer) {
	s.mu.Lock()
	s.m[pr.legID] = pr
	s.mu.Unlock()
}

// markAccepted flips a parked REFER to accepted and stops its decline timer.
// Returns (nil, false) if unknown or already accepted.
func (s *pendingReferStore) markAccepted(legID string) (*pendingRefer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := s.m[legID]
	if !ok || pr.accepted {
		return nil, false
	}
	pr.accepted = true
	if pr.timer != nil {
		pr.timer.Stop()
	}
	return pr, true
}

// peekAccepted returns an accepted entry without removing it (for interim
// progress NOTIFYs).
func (s *pendingReferStore) peekAccepted(legID string) (*pendingRefer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := s.m[legID]
	if !ok || !pr.accepted {
		return nil, false
	}
	return pr, true
}

// takeAccepted removes and returns an accepted entry (for the terminal NOTIFY).
func (s *pendingReferStore) takeAccepted(legID string) (*pendingRefer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := s.m[legID]
	if !ok || !pr.accepted {
		return nil, false
	}
	delete(s.m, legID)
	return pr, true
}

// takeIfPending removes and returns an entry only if it has not been accepted
// (for decline and the consult-timeout auto-decline).
func (s *pendingReferStore) takeIfPending(legID string) (*pendingRefer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := s.m[legID]
	if !ok || pr.accepted {
		return nil, false
	}
	delete(s.m, legID)
	if pr.timer != nil {
		pr.timer.Stop()
	}
	return pr, true
}

// TransferProgressRequest reports an interim sipfrag status (e.g. 180 Ringing)
// on an accepted inbound transfer.
type TransferProgressRequest struct {
	StatusCode int    `json:"status_code"`
	Reason     string `json:"reason,omitempty"`
}

// TransferCompleteRequest reports the terminal outcome of an accepted inbound
// transfer. Success sends 200 OK; otherwise StatusCode/Reason (default 500)
// carry the failure. Either way the refer subscription is terminated.
type TransferCompleteRequest struct {
	Success    bool   `json:"success"`
	StatusCode int    `json:"status_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// TransferDeclineRequest rejects a parked (not-yet-accepted) inbound transfer.
// Code defaults to 603 and Reason to "Decline".
type TransferDeclineRequest struct {
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) doAcceptTransfer(legID string) error {
	pr, ok := s.pendingRefers.markAccepted(legID)
	if !ok {
		return newAPIError(http.StatusNotFound, "no pending transfer for leg (unknown or already decided)")
	}
	if pr.tx != nil {
		if err := s.SIPEngine.RespondFromSource(pr.tx, pr.req, sip.StatusAccepted, "Accepted"); err != nil {
			s.Log.Error("REFER respond 202 failed", "leg_id", legID, "error", err)
		}
	}
	if err := pr.referrer.SendNotifySipfrag(context.Background(), 100, "Trying", false); err != nil {
		s.Log.Warn("transfer NOTIFY 100 failed", "leg_id", legID, "error", err)
	}
	s.Log.Info("inbound REFER accepted by app", "leg_id", legID, "target", pr.target)
	return nil
}

func (s *Server) doTransferProgress(legID string, req TransferProgressRequest) error {
	if req.StatusCode < 100 || req.StatusCode > 699 {
		return newAPIError(http.StatusBadRequest, "status_code must be a SIP status (100-699)")
	}
	pr, ok := s.pendingRefers.peekAccepted(legID)
	if !ok {
		return newAPIError(http.StatusNotFound, "no accepted transfer for leg")
	}
	reason := req.Reason
	if reason == "" {
		reason = sipReasonPhrase(req.StatusCode)
	}
	if err := pr.referrer.SendNotifySipfrag(context.Background(), req.StatusCode, reason, false); err != nil {
		s.Log.Warn("transfer progress NOTIFY failed", "leg_id", legID, "error", err)
	}
	return nil
}

func (s *Server) doCompleteTransfer(legID string, req TransferCompleteRequest) error {
	pr, ok := s.pendingRefers.takeAccepted(legID)
	if !ok {
		return newAPIError(http.StatusNotFound, "no accepted transfer for leg (accept it first)")
	}
	code := req.StatusCode
	reason := req.Reason
	if req.Success {
		if code == 0 {
			code = 200
		}
		if reason == "" {
			reason = "OK"
		}
	} else {
		if code == 0 {
			code = 500
		}
		if reason == "" {
			reason = sipReasonPhrase(code)
		}
	}
	if err := pr.referrer.SendNotifySipfrag(context.Background(), code, reason, true); err != nil {
		s.Log.Warn("transfer terminal NOTIFY failed", "leg_id", legID, "error", err)
	}
	scope := events.LegScope{LegID: pr.referrer.ID(), AppID: pr.referrer.AppID()}
	if code >= 200 && code < 300 {
		s.Bus.Publish(events.LegTransferCompleted, &events.LegTransferCompletedData{
			LegScope: scope, StatusCode: code, Reason: reason,
		})
	} else {
		s.Bus.Publish(events.LegTransferFailed, &events.LegTransferFailedData{
			LegScope: scope, StatusCode: code, Reason: reason,
		})
	}
	return nil
}

func (s *Server) doDeclineTransfer(legID string, req TransferDeclineRequest) error {
	pr, ok := s.pendingRefers.takeIfPending(legID)
	if !ok {
		return newAPIError(http.StatusNotFound, "no pending transfer for leg (unknown or already accepted)")
	}
	code := req.Code
	if code == 0 {
		code = 603
	}
	reason := req.Reason
	if reason == "" {
		reason = "Decline"
	}
	if pr.tx != nil {
		if err := s.SIPEngine.RespondFromSource(pr.tx, pr.req, code, reason); err != nil {
			s.Log.Error("REFER decline respond failed", "leg_id", legID, "error", err)
		}
	}
	s.Bus.Publish(events.LegTransferFailed, &events.LegTransferFailedData{
		LegScope:   events.LegScope{LegID: pr.referrer.ID(), AppID: pr.referrer.AppID()},
		StatusCode: code,
		Reason:     reason,
	})
	return nil
}

// declineReferOnTimeout auto-declines a parked REFER that no app decided within
// the consult window. A no-op if the REFER was already accepted or decided.
func (s *Server) declineReferOnTimeout(legID string) {
	pr, ok := s.pendingRefers.takeIfPending(legID)
	if !ok {
		return
	}
	if pr.tx != nil {
		if err := s.SIPEngine.RespondFromSource(pr.tx, pr.req, 603, "Decline"); err != nil {
			s.Log.Error("REFER timeout respond 603 failed", "leg_id", legID, "error", err)
		}
	}
	s.Log.Info("inbound REFER auto-declined (consult timeout)", "leg_id", legID)
	s.Bus.Publish(events.LegTransferFailed, &events.LegTransferFailedData{
		LegScope:   events.LegScope{LegID: pr.referrer.ID(), AppID: pr.referrer.AppID()},
		StatusCode: 603,
		Reason:     "Decline",
	})
}

// sipReasonPhrase returns a default reason phrase for common SIP status codes
// used in transfer sipfrag NOTIFYs.
func sipReasonPhrase(code int) string {
	switch code {
	case 100:
		return "Trying"
	case 180:
		return "Ringing"
	case 183:
		return "Session Progress"
	case 200:
		return "OK"
	case 486:
		return "Busy Here"
	case 487:
		return "Request Terminated"
	case 500:
		return "Server Internal Error"
	case 603:
		return "Decline"
	default:
		return "Transfer"
	}
}

// ── HTTP handlers for the inbound-transfer decision commands ──

func (s *Server) acceptTransferLeg(w http.ResponseWriter, r *http.Request) {
	if err := s.doAcceptTransfer(chi.URLParam(r, "id")); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepting"})
}

func (s *Server) progressTransferLeg(w http.ResponseWriter, r *http.Request) {
	var req TransferProgressRequest
	if err := decodeJSON(r, &req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.doTransferProgress(chi.URLParam(r, "id"), req); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "progress"})
}

func (s *Server) completeTransferLeg(w http.ResponseWriter, r *http.Request) {
	var req TransferCompleteRequest
	if err := decodeJSON(r, &req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.doCompleteTransfer(chi.URLParam(r, "id"), req); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "completing"})
}

func (s *Server) declineTransferLeg(w http.ResponseWriter, r *http.Request) {
	var req TransferDeclineRequest
	if err := decodeJSON(r, &req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.doDeclineTransfer(chi.URLParam(r, "id"), req); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "declining"})
}
