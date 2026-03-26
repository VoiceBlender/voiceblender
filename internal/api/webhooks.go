package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) registerWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url required")
		return
	}
	wh := s.Webhooks.Register(req.URL, req.Secret)
	writeJSON(w, http.StatusCreated, wh)
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks := s.Webhooks.List()
	writeJSON(w, http.StatusOK, hooks)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.Webhooks.Unregister(id) {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
