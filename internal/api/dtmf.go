package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) sendDTMF(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	var req struct {
		Digits string `json:"digits"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.Digits == "" {
		writeError(w, http.StatusBadRequest, "digits required")
		return
	}

	if err := l.SendDTMF(r.Context(), req.Digits); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
