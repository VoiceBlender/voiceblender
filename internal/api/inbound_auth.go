package api

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ChallengeRequest is the body shared by the leg-challenge and
// registration-attempt-challenge endpoints / VSI commands. It carries the
// digest realm and the expected credential VoiceBlender verifies the retry
// against. Provide either Password or HA1 (= H(username:realm:password)).
type ChallengeRequest struct {
	Realm     string   `json:"realm"`
	Username  string   `json:"username,omitempty"`
	Password  string   `json:"password,omitempty"`
	HA1       string   `json:"ha1,omitempty"`
	Algorithm string   `json:"algorithm,omitempty"`
	QOP       []string `json:"qop,omitempty"`
}

func (r ChallengeRequest) toParams() sipmod.ChallengeParams {
	return sipmod.ChallengeParams{
		Realm:     r.Realm,
		Username:  r.Username,
		Password:  r.Password,
		HA1:       r.HA1,
		Algorithm: r.Algorithm,
		QOP:       r.QOP,
	}
}

func (r ChallengeRequest) validate() error {
	if r.Realm == "" {
		return newAPIError(http.StatusBadRequest, "realm is required")
	}
	if r.Password == "" && r.HA1 == "" {
		return newAPIError(http.StatusBadRequest, "password or ha1 is required")
	}
	return nil
}

// RegistrationRejectRequest is the optional body for rejecting a registration
// attempt. Code defaults to 403 and Reason to "Forbidden".
type RegistrationRejectRequest struct {
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ── Inbound INVITE challenge ─────────────────────────────────────────────

// doChallengeLeg sends a 401 digest challenge on a ringing inbound SIP leg.
// Mirrors the reject path in doDeleteLeg: the leg is claimed, the 401 is sent,
// and a leg.disconnected("challenged") is published. The credentialed
// re-INVITE arrives as a fresh inbound call surfaced as authenticated.
func (s *Server) doChallengeLeg(id string, req ChallengeRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	sl, isSIP := l.(*leg.SIPLeg)
	if !isSIP {
		return newAPIError(http.StatusBadRequest, "only SIP inbound legs can be challenged")
	}
	if st := sl.State(); st != leg.StateRinging && st != leg.StateEarlyMedia {
		return newAPIError(http.StatusConflict, "leg is %s, not ringing", st)
	}
	if _, ok := s.LegMgr.Remove(id); !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	sl.SetDisconnectReason("challenged")
	params := req.toParams()
	go func() {
		if err := sl.Challenge(context.Background(), params); err != nil {
			s.Log.Warn("challenge error", "leg_id", sl.ID(), "error", err)
		}
		s.cleanupLeg(sl)
		s.publishDisconnect(sl, "challenged")
	}()
	return nil
}

func (s *Server) challengeLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req ChallengeRequest
	if err := decodeJSON(r, &req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.doChallengeLeg(id, req); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "challenging"})
}

// ── Inbound REGISTER decision ────────────────────────────────────────────

// registerAttemptStore tracks parked REGISTER attempts awaiting a client
// decision, keyed by attempt id. The decision channel is buffered so a late
// decision never blocks the command handler.
type registerAttemptStore struct {
	mu sync.Mutex
	m  map[string]chan sipmod.RegisterDecision
}

func newRegisterAttemptStore() *registerAttemptStore {
	return &registerAttemptStore{m: make(map[string]chan sipmod.RegisterDecision)}
}

func (s *registerAttemptStore) put(id string, ch chan sipmod.RegisterDecision) {
	s.mu.Lock()
	s.m[id] = ch
	s.mu.Unlock()
}

func (s *registerAttemptStore) get(id string) (chan sipmod.RegisterDecision, bool) {
	s.mu.Lock()
	ch, ok := s.m[id]
	s.mu.Unlock()
	return ch, ok
}

func (s *registerAttemptStore) delete(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// HandleRegisterAttempt is the engine OnRegisterAttempt callback. Mirroring the
// inbound-INVITE path (which always surfaces leg.ringing and waits for the
// client to decide), every inbound REGISTER is surfaced as a
// sip.registration_attempt event and the client is consulted for a
// challenge/accept/reject decision. Because a REGISTER — unlike an INVITE —
// cannot be parked indefinitely, the consult is bounded: if no decision arrives
// within the consult timeout the REGISTER auto-accepts, preserving the
// unauthenticated default.
func (s *Server) HandleRegisterAttempt(a *sipmod.RegisterAttempt) sipmod.RegisterDecision {
	attemptID := uuid.New().String()
	ch := make(chan sipmod.RegisterDecision, 1)
	s.regAttempts.put(attemptID, ch)
	defer s.regAttempts.delete(attemptID)

	s.Bus.Publish(events.SIPRegistrationAttempt, &events.SIPRegistrationAttemptData{
		AttemptID:        attemptID,
		AOR:              a.AOR,
		Contact:          a.Contact,
		SourceAddress:    a.Source,
		Transport:        a.Transport,
		UserAgent:        a.UserAgent,
		CallID:           a.CallID,
		HasAuthorization: a.HasAuth,
	})

	timeout := time.Duration(s.Config.SIPInboundAuthConsultTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	select {
	case d := <-ch:
		return d
	case <-time.After(timeout):
		return sipmod.RegisterDecision{Kind: sipmod.RegisterAccept}
	}
}

func (s *Server) decideRegisterAttempt(id string, d sipmod.RegisterDecision) error {
	ch, ok := s.regAttempts.get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "registration attempt not found or already decided")
	}
	select {
	case ch <- d:
	default:
	}
	return nil
}

func (s *Server) doChallengeRegistration(id string, req ChallengeRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	return s.decideRegisterAttempt(id, sipmod.RegisterDecision{
		Kind:      sipmod.RegisterChallenge,
		Challenge: req.toParams(),
	})
}

func (s *Server) doAcceptRegistration(id string) error {
	return s.decideRegisterAttempt(id, sipmod.RegisterDecision{Kind: sipmod.RegisterAccept})
}

func (s *Server) doRejectRegistration(id string, code int, reason string) error {
	return s.decideRegisterAttempt(id, sipmod.RegisterDecision{
		Kind:         sipmod.RegisterReject,
		RejectCode:   code,
		RejectReason: reason,
	})
}

func (s *Server) challengeRegistrationAttempt(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req ChallengeRequest
	if err := decodeJSON(r, &req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.doChallengeRegistration(id, req); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "challenging"})
}

func (s *Server) acceptRegistrationAttempt(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doAcceptRegistration(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepting"})
}

func (s *Server) rejectRegistrationAttempt(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req RegistrationRejectRequest
	if err := decodeJSON(r, &req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.doRejectRegistration(id, req.Code, req.Reason); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "rejecting"})
}
