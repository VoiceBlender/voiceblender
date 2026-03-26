package api

import (
	"log/slog"
	"net/http"

	"github.com/csiwek/VoiceBlender/internal/config"
	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/leg"
	"github.com/csiwek/VoiceBlender/internal/room"
	sipmod "github.com/csiwek/VoiceBlender/internal/sip"
	"github.com/csiwek/VoiceBlender/internal/storage"
	"github.com/csiwek/VoiceBlender/internal/tts"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	Router    *chi.Mux
	LegMgr   *leg.Manager
	RoomMgr  *room.Manager
	SIPEngine *sipmod.Engine
	Bus      *events.Bus
	Webhooks *events.WebhookRegistry
	TTS      tts.Provider
	S3       storage.Backend
	Config   config.Config
	Log      *slog.Logger
}

func NewServer(
	legMgr *leg.Manager,
	roomMgr *room.Manager,
	engine *sipmod.Engine,
	bus *events.Bus,
	webhooks *events.WebhookRegistry,
	ttsProvider tts.Provider,
	s3Backend storage.Backend,
	cfg config.Config,
	log *slog.Logger,
) *Server {
	s := &Server{
		Router:    chi.NewRouter(),
		LegMgr:   legMgr,
		RoomMgr:  roomMgr,
		SIPEngine: engine,
		Bus:      bus,
		Webhooks: webhooks,
		TTS:      ttsProvider,
		S3:       s3Backend,
		Config:   cfg,
		Log:      log,
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	r := s.Router
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(corsMiddleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			next.ServeHTTP(w, r)
		})
	})

	r.Route("/v1", func(r chi.Router) {
		// Legs
		r.Post("/legs", s.createLeg)
		r.Get("/legs", s.listLegs)
		r.Get("/legs/{id}", s.getLeg)
		r.Post("/legs/{id}/answer", s.answerLeg)
		r.Post("/legs/{id}/mute", s.muteLeg)
		r.Delete("/legs/{id}/mute", s.unmuteLeg)
		r.Delete("/legs/{id}", s.deleteLeg)
		r.Post("/legs/{id}/dtmf", s.sendDTMF)
		r.Post("/legs/{id}/play", s.playLeg)
		r.Delete("/legs/{id}/play/{playbackID}", s.stopPlayLeg)
		r.Post("/legs/{id}/tts", s.ttsLeg)
		r.Post("/legs/{id}/record", s.recordLeg)
		r.Delete("/legs/{id}/record", s.stopRecordLeg)
		r.Post("/legs/{id}/stt", s.sttLeg)
		r.Delete("/legs/{id}/stt", s.stopSTTLeg)
		r.Post("/legs/{id}/agent", s.agentLeg)
		r.Delete("/legs/{id}/agent", s.stopAgentLeg)

		// Rooms
		r.Post("/rooms", s.createRoom)
		r.Get("/rooms", s.listRooms)
		r.Get("/rooms/{id}", s.getRoom)
		r.Delete("/rooms/{id}", s.deleteRoom)
		r.Post("/rooms/{id}/legs", s.addLegToRoom)
		r.Delete("/rooms/{id}/legs/{legID}", s.removeLegFromRoom)
		r.Post("/rooms/{id}/play", s.playRoom)
		r.Delete("/rooms/{id}/play/{playbackID}", s.stopPlayRoom)
		r.Post("/rooms/{id}/tts", s.ttsRoom)
		r.Post("/rooms/{id}/record", s.recordRoom)
		r.Delete("/rooms/{id}/record", s.stopRecordRoom)
		r.Post("/rooms/{id}/stt", s.sttRoom)
		r.Delete("/rooms/{id}/stt", s.stopSTTRoom)
		r.Post("/rooms/{id}/agent", s.agentRoom)
		r.Delete("/rooms/{id}/agent", s.stopAgentRoom)
		r.Get("/rooms/{id}/ws", s.wsRoom)

		// WebRTC
		r.Post("/webrtc/offer", s.webrtcOffer)

		// Webhooks
		r.Post("/webhooks", s.registerWebhook)
		r.Get("/webhooks", s.listWebhooks)
		r.Delete("/webhooks/{id}", s.deleteWebhook)
	})
}

// corsMiddleware allows cross-origin requests from browser clients.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Router.ServeHTTP(w, r)
}
