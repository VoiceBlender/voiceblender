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

// createTrunk handles POST /v1/sip/trunks. Synchronously validates and
// registers the trunk in the manager, then kicks off the async REGISTER
// loop. Returns 202 Accepted.
func (s *Server) createTrunk(w http.ResponseWriter, r *http.Request) {
	var req CreateTrunkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	switch req.Type {
	case string(sipmod.TrunkTypeSIPRegister):
		s.createSIPRegisterTrunk(w, r, req)
	case string(sipmod.TrunkTypeIPIP):
		writeError(w, http.StatusNotImplemented, "trunk type 'ip_ip' not yet implemented")
	case "":
		writeError(w, http.StatusBadRequest, "type is required")
	default:
		writeError(w, http.StatusBadRequest, "unknown trunk type: "+req.Type)
	}
}

func (s *Server) createSIPRegisterTrunk(w http.ResponseWriter, r *http.Request, req CreateTrunkRequest) {
	spec := req.SIPRegister
	if spec == nil {
		writeError(w, http.StatusBadRequest, "sip_register block is required when type=sip_register")
		return
	}
	if spec.RegistrarURI == "" {
		writeError(w, http.StatusBadRequest, "sip_register.registrar_uri is required")
		return
	}
	if spec.AOR == "" {
		writeError(w, http.StatusBadRequest, "sip_register.aor is required")
		return
	}
	if spec.Password == "" {
		writeError(w, http.StatusBadRequest, "sip_register.password is required")
		return
	}

	var registrarURI sip.Uri
	if err := sip.ParseUri(spec.RegistrarURI, &registrarURI); err != nil {
		writeError(w, http.StatusBadRequest, "sip_register.registrar_uri is invalid: "+err.Error())
		return
	}
	var aorURI sip.Uri
	if err := sip.ParseUri(spec.AOR, &aorURI); err != nil {
		writeError(w, http.StatusBadRequest, "sip_register.aor is invalid: "+err.Error())
		return
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
	// The trunk lifecycle outlives the HTTP request — using r.Context()
	// would cancel the REGISTER loop as soon as the 202 is returned.
	trunk.Start(context.Background())

	writeJSON(w, http.StatusAccepted, CreateTrunkResponse{
		ID:     id,
		Type:   string(sipmod.TrunkTypeSIPRegister),
		Status: string(sipmod.TrunkStatusRegistering),
	})
}

// listTrunks handles GET /v1/sip/trunks.
func (s *Server) listTrunks(w http.ResponseWriter, r *http.Request) {
	trunks := s.SIPEngine.Trunks().List()
	views := make([]sipmod.TrunkView, 0, len(trunks))
	for _, t := range trunks {
		views = append(views, t.Snapshot())
	}
	writeJSON(w, http.StatusOK, TrunksListResponse{Trunks: views})
}

// getTrunk handles GET /v1/sip/trunks/{id}.
func (s *Server) getTrunk(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t := s.SIPEngine.Trunks().Get(id)
	if t == nil {
		writeError(w, http.StatusNotFound, "trunk not found")
		return
	}
	writeJSON(w, http.StatusOK, t.Snapshot())
}

// deleteTrunk handles DELETE /v1/sip/trunks/{id}. Returns 202 Accepted and
// performs the unregister + cleanup asynchronously.
func (s *Server) deleteTrunk(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t := s.SIPEngine.Trunks().Get(id)
	if t == nil {
		writeError(w, http.StatusNotFound, "trunk not found")
		return
	}
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = t.Stop(stopCtx)
		s.SIPEngine.Trunks().Remove(id)
	}()
	w.WriteHeader(http.StatusAccepted)
}
