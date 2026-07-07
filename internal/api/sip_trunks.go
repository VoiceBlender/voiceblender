package api

import (
	"context"
	"net/http"
	"time"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CreateTrunkResponse is the body returned by POST /v1/sip/trunks.
type CreateTrunkResponse struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// TrunksListResponse is the body returned by GET /v1/sip/trunks.
type TrunksListResponse struct {
	Trunks []sipmod.TrunkView `json:"trunks"`
}

// doCreateTrunk validates the request, registers the trunk, and starts the
// async REGISTER loop. Shared by the REST handler and the VSI dispatcher.
func (s *Server) doCreateTrunk(req CreateTrunkRequest) (CreateTrunkResponse, error) {
	switch req.Type {
	case string(sipmod.TrunkTypeSIPRegister):
		return s.doCreateSIPRegisterTrunk(req)
	case string(sipmod.TrunkTypeIPIP):
		return CreateTrunkResponse{}, newAPIError(http.StatusNotImplemented, "trunk type 'ip_ip' not yet implemented")
	case "":
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "type is required")
	default:
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "unknown trunk type: %s", req.Type)
	}
}

func (s *Server) doCreateSIPRegisterTrunk(req CreateTrunkRequest) (CreateTrunkResponse, error) {
	spec := req.SIPRegister
	if spec == nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register block is required when type=sip_register")
	}
	if spec.RegistrarURI == "" {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.registrar_uri is required")
	}
	if spec.AOR == "" {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.aor is required")
	}
	if spec.Password == "" {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.password is required")
	}

	var registrarURI sip.Uri
	if err := sip.ParseUri(spec.RegistrarURI, &registrarURI); err != nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.registrar_uri is invalid: %s", err.Error())
	}
	var aorURI sip.Uri
	if err := sip.ParseUri(spec.AOR, &aorURI); err != nil {
		return CreateTrunkResponse{}, newAPIError(http.StatusBadRequest, "sip_register.aor is invalid: %s", err.Error())
	}

	id := uuid.NewString()
	cfg := sipmod.OutboundRegistrationConfig{
		DefaultExpiresSeconds: s.Config.SIPOutboundRegistrationDefaultExpiresSeconds,
		MinExpiresSeconds:     s.Config.SIPOutboundRegistrationMinExpiresSeconds,
		MaxExpiresSeconds:     s.Config.SIPOutboundRegistrationMaxExpiresSeconds,
		RefreshRatio:          s.Config.SIPOutboundRegistrationRefreshRatio,
		FailureBackoffMax:     time.Duration(s.Config.SIPOutboundRegistrationFailureBackoffMaxMs) * time.Millisecond,
	}
	trunk := sipmod.NewOutboundRegistration(s.SIPEngine, s.Bus, s.Log, cfg, sipmod.OutboundRegistrationParams{
		ID:                      id,
		AppID:                   req.AppID,
		RegistrarURI:            registrarURI,
		AOR:                     aorURI,
		Username:                spec.Username,
		Password:                spec.Password,
		ContactUser:             spec.ContactUser,
		RequestedExpiresSeconds: spec.ExpiresSeconds,
	})
	s.SIPEngine.Trunks().Add(trunk)
	// The trunk lifecycle outlives the request that created it — using a
	// request-scoped context would cancel the REGISTER loop immediately.
	trunk.Start(context.Background())

	return CreateTrunkResponse{
		ID:     id,
		Type:   string(sipmod.TrunkTypeSIPRegister),
		Status: string(sipmod.TrunkStatusRegistering),
	}, nil
}

// doListTrunks returns a snapshot of every configured trunk.
func (s *Server) doListTrunks() TrunksListResponse {
	trunks := s.SIPEngine.Trunks().List()
	views := make([]sipmod.TrunkView, 0, len(trunks))
	for _, t := range trunks {
		views = append(views, t.Snapshot())
	}
	return TrunksListResponse{Trunks: views}
}

// doGetTrunk returns a single trunk snapshot or a 404 apiError.
func (s *Server) doGetTrunk(id string) (sipmod.TrunkView, error) {
	t := s.SIPEngine.Trunks().Get(id)
	if t == nil {
		return sipmod.TrunkView{}, newAPIError(http.StatusNotFound, "trunk not found")
	}
	return t.Snapshot(), nil
}

// doDeleteTrunk unregisters and removes the trunk asynchronously.
func (s *Server) doDeleteTrunk(id string) error {
	t := s.SIPEngine.Trunks().Get(id)
	if t == nil {
		return newAPIError(http.StatusNotFound, "trunk not found")
	}
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = t.Stop(stopCtx)
		s.SIPEngine.Trunks().Remove(id)
	}()
	return nil
}

// createTrunk handles POST /v1/sip/trunks. Returns 202 Accepted.
func (s *Server) createTrunk(w http.ResponseWriter, r *http.Request) {
	var req CreateTrunkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.doCreateTrunk(req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// listTrunks handles GET /v1/sip/trunks.
func (s *Server) listTrunks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.doListTrunks())
}

// getTrunk handles GET /v1/sip/trunks/{id}.
func (s *Server) getTrunk(w http.ResponseWriter, r *http.Request) {
	view, err := s.doGetTrunk(chi.URLParam(r, "id"))
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// deleteTrunk handles DELETE /v1/sip/trunks/{id}. Returns 202 Accepted and
// performs the unregister + cleanup asynchronously.
func (s *Server) deleteTrunk(w http.ResponseWriter, r *http.Request) {
	if err := s.doDeleteTrunk(chi.URLParam(r, "id")); err != nil {
		handleAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
