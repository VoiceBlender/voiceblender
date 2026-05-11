package api

import (
	"context"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/go-chi/chi/v5"
)

// rttPreviewMaxRunes caps how many runes of an RTT chunk are emitted in
// debug logs. Keeps log lines bounded for very long sends; full content
// is still available via SIP_DEBUG wire dumps when needed.
const rttPreviewMaxRunes = 64

// RTT send source tags surfaced in debug logs.
const (
	rttSourceREST = "rest"
	rttSourceVSI  = "vsi"
)

// rttPreview returns a UTF-8-safe preview suitable for slog values.
func rttPreview(text string) string {
	if utf8.RuneCountInString(text) <= rttPreviewMaxRunes {
		return text
	}
	count := 0
	for i := range text {
		if count == rttPreviewMaxRunes {
			return text[:i] + "…"
		}
		count++
	}
	return text
}

func (s *Server) doSendLegRTT(ctx context.Context, source, id, text string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	if text == "" {
		return newAPIError(http.StatusBadRequest, "text required")
	}
	if !l.RTTNegotiated() {
		return newAPIError(http.StatusConflict, "RTT not negotiated for this leg")
	}
	s.Log.Debug("rtt send", "leg_id", id, "source", source, "len", len(text), "runes", utf8.RuneCountInString(text), "preview", rttPreview(text))
	if err := l.SendText(ctx, text); err != nil {
		if errors.Is(err, leg.ErrRTTNotNegotiated) {
			return newAPIError(http.StatusConflict, "%s", err.Error())
		}
		return newAPIError(http.StatusInternalServerError, "%s", err.Error())
	}
	return nil
}

func (s *Server) sendRTT(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req RTTRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.doSendLegRTT(r.Context(), rttSourceREST, id, req.Text); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) doAcceptLegRTT(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	l.SetAcceptText(true)
	s.Log.Debug("rtt accept", "leg_id", id)
	return nil
}

func (s *Server) acceptRTTLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doAcceptLegRTT(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rtt_accepting"})
}

func (s *Server) doRejectLegRTT(id string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	l.SetAcceptText(false)
	s.Log.Debug("rtt reject", "leg_id", id)
	return nil
}

func (s *Server) rejectRTTLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.doRejectLegRTT(id); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rtt_rejecting"})
}

// broadcastRTT forwards a chunk of received text to every other RTT-capable
// leg in the same room that has accept_text enabled. Mirrors broadcastDTMF.
// Runs inline on the source leg's dispatcher; the short per-peer deadline
// caps the worst-case stall when a peer's outbound text queue is full.
func (s *Server) broadcastRTT(fromLegID, text string) {
	roomID, ok := s.RoomMgr.FindLegRoom(fromLegID)
	if !ok {
		return
	}
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}
	for _, p := range rm.Participants() {
		if p.ID() == fromLegID || !p.AcceptText() || !p.RTTNegotiated() {
			continue
		}
		s.Log.Debug("rtt broadcast", "from_leg", fromLegID, "to_leg", p.ID(), "len", len(text))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		err := p.SendText(ctx, text)
		cancel()
		if err != nil {
			s.Log.Warn("rtt forward failed", "from_leg", fromLegID, "to_leg", p.ID(), "error", err)
		}
	}
}
