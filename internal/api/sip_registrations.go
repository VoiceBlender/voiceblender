package api

import (
	"net/http"
	"net/url"
	"time"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/go-chi/chi/v5"
)

// RegistrationView is the JSON shape returned by GET /v1/sip/registrations
// and the list_sip_registrations VSI command for a single AOR binding.
type RegistrationView struct {
	AOR                   string `json:"aor"`
	Contact               string `json:"contact"`
	Socket                string `json:"socket"`
	Transport             string `json:"transport"`
	UserAgent             string `json:"user_agent,omitempty"`
	CallID                string `json:"call_id,omitempty"`
	AppID                 string `json:"app_id,omitempty"`
	CreatedAt             string `json:"created_at"`
	LastRefresh           string `json:"last_refresh"`
	ExpiresAt             string `json:"expires_at"`
	GrantedExpiresSeconds int    `json:"granted_expires_seconds"`
}

// RegistrationsResponse is the success body shape for the list endpoint.
type RegistrationsResponse struct {
	Bindings []RegistrationView `json:"bindings"`
}

func toRegistrationView(b sipmod.Binding) RegistrationView {
	return RegistrationView{
		AOR:                   b.AOR,
		Contact:               b.Contact,
		Socket:                b.Socket,
		Transport:             b.Transport,
		UserAgent:             b.UserAgent,
		CallID:                b.CallID,
		AppID:                 b.AppID,
		CreatedAt:             b.CreatedAt.UTC().Format(time.RFC3339),
		LastRefresh:           b.LastRefresh.UTC().Format(time.RFC3339),
		ExpiresAt:             b.ExpiresAt.UTC().Format(time.RFC3339),
		GrantedExpiresSeconds: b.GrantedExpires,
	}
}

// listRegistrations handles GET /v1/sip/registrations.
func (s *Server) listRegistrations(w http.ResponseWriter, r *http.Request) {
	reg := s.SIPEngine.Registrar()
	if reg == nil {
		writeJSON(w, http.StatusOK, RegistrationsResponse{Bindings: []RegistrationView{}})
		return
	}
	bindings := reg.List()
	views := make([]RegistrationView, 0, len(bindings))
	for _, b := range bindings {
		views = append(views, toRegistrationView(b))
	}
	writeJSON(w, http.StatusOK, RegistrationsResponse{Bindings: views})
}

// deleteRegistration handles DELETE /v1/sip/registrations/{aor}. AOR is
// URL-encoded in the path (callers should encode "sip:alice@host" as
// "sip%3Aalice%40host"). When the query string carries `?contact=<uri>`,
// only that one Contact under the AOR is removed; otherwise every Contact
// is force-unbound.
func (s *Server) deleteRegistration(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "aor")
	aor, err := url.PathUnescape(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid AOR encoding")
		return
	}
	if err := s.doDeleteRegistration(aor, r.URL.Query().Get("contact")); err != nil {
		handleAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// doDeleteRegistration force-unbinds an AOR (or a single contact under it).
// Shared by the REST handler and the VSI delete_sip_registration command.
func (s *Server) doDeleteRegistration(aor, contact string) error {
	reg := s.SIPEngine.Registrar()
	if reg == nil {
		return newAPIError(http.StatusNotFound, "registrar not enabled")
	}
	if contact != "" {
		if ok := reg.UnbindContact(aor, contact, "forced"); !ok {
			return newAPIError(http.StatusNotFound, "contact not found")
		}
		return nil
	}
	if n := reg.UnbindAll(aor, "forced"); n == 0 {
		return newAPIError(http.StatusNotFound, "AOR not found")
	}
	return nil
}
