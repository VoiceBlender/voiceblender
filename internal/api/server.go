//go:generate go run ../../cmd/openapi-gen
//go:generate go run ../../cmd/asyncapi-gen

package api

import (
	"log/slog"
	"net/http"
	"net/netip"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/metrics"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/VoiceBlender/voiceblender/internal/speaking"
	"github.com/VoiceBlender/voiceblender/internal/storage"
	"github.com/VoiceBlender/voiceblender/internal/tts"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/quic-go/webtransport-go"
)

type Server struct {
	Router     *chi.Mux
	LegMgr     *leg.Manager
	RoomMgr    *room.Manager
	SIPEngine  *sipmod.Engine
	Bus        *events.Bus
	Webhooks   *events.WebhookRegistry
	TTS        tts.Provider
	TTSCache   *tts.Cache
	S3         storage.Backend
	Metrics    *metrics.Collector
	Config     config.Config
	AllowedIPs []netip.Prefix
	Log        *slog.Logger

	// MoQWebTransport is set by main.go when MoQ is enabled. The MoQ leg
	// handler (s.moqLeg) returns 503 if this is nil.
	MoQWebTransport *webtransport.Server

	speakMu   sync.Mutex
	speakDets map[string]*speaking.Detector

	speechOverrideMu sync.Mutex
	speechOverride   map[string]*bool

	transfers *transferStore

	// pendingRefers tracks inbound REFERs parked awaiting an app accept/decline.
	pendingRefers *pendingReferStore

	// regAttempts tracks inbound REGISTERs parked awaiting a client decision.
	regAttempts *registerAttemptStore
}

func NewServer(
	legMgr *leg.Manager,
	roomMgr *room.Manager,
	engine *sipmod.Engine,
	bus *events.Bus,
	webhooks *events.WebhookRegistry,
	ttsProvider tts.Provider,
	ttsCache *tts.Cache,
	s3Backend storage.Backend,
	metricsCollector *metrics.Collector,
	cfg config.Config,
	allowedIPs []netip.Prefix,
	log *slog.Logger,
) *Server {
	instanceID = cfg.InstanceID
	s := &Server{
		Router:         chi.NewRouter(),
		LegMgr:         legMgr,
		RoomMgr:        roomMgr,
		SIPEngine:      engine,
		Bus:            bus,
		Webhooks:       webhooks,
		TTS:            ttsProvider,
		TTSCache:       ttsCache,
		S3:             s3Backend,
		Metrics:        metricsCollector,
		Config:         cfg,
		AllowedIPs:     allowedIPs,
		Log:            log,
		speakDets:      make(map[string]*speaking.Detector),
		speechOverride: make(map[string]*bool),
		transfers:      newTransferStore(),
		pendingRefers:  newPendingReferStore(),
		regAttempts:    newRegisterAttemptStore(),
	}
	// A leg whose mixer IO loop panicked is torn down inside the room layer,
	// which cannot finish the job: the CDR, the OTel span, the per-leg webhook
	// and the leg-manager entry are all API-layer state, and room cannot import
	// api. Without this the leg emits leg.left_room and nothing else — no
	// leg.disconnected, an unended span, and it stays in LegMgr, so
	// GET /v1/legs/{id} keeps serving a dead leg forever.
	//
	// tearDownPanickedLeg's RemoveLeg has already cleared the leg's RoomID by
	// the time this runs, so cleanupLeg's room block is skipped. That is what
	// stops a second leg.left_room — but it also skips the room-scoped cleanup,
	// so this path has to run that half itself off the roomID the hook carries.
	// Every other leg-removal path reaches it through cleanupLeg; without this
	// the panic path would be the only one that silently drops it, stranding
	// the room's agent session and its recording on an empty room.
	roomMgr.SetOnLegPanicTeardown(func(l leg.Leg, roomID, reason string) {
		if roomID != "" {
			// Same order as cleanupLeg's room block, minus the RemoveLeg the
			// room layer already did.
			s.onLegLeavingRoomRecording(roomID, l.ID())
			s.stopRoomAgentIfEmpty(roomID)
			s.stopRoomRecordingIfEmpty(roomID)
		}
		s.cleanupLeg(l)
		s.publishDisconnect(l, reason)
	})

	s.routes()
	return s
}

func (s *Server) routes() {
	r := s.Router
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	r.Use(corsMiddleware)
	r.Use(ipAllowlistMiddleware(s.AllowedIPs, s.Config.TrustProxyHeaders, s.Log))
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			next.ServeHTTP(w, r)
		})
	})

	// Prometheus metrics — outside /v1, plain text response.
	if s.Metrics != nil {
		r.Get("/metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			s.Metrics.Handler().ServeHTTP(w, r)
		})
	}

	// pprof — compiled in only with -tags pprof; no-op otherwise.
	registerPprof(r, s.Log)

	r.Route("/v1", func(r chi.Router) {
		// Legs
		r.Post("/legs", s.createLeg)
		r.Get("/legs", s.listLegs)
		r.Get("/legs/websocket", s.wsLeg)
		r.Connect("/legs/moq", s.moqLeg)
		r.Get("/legs/{id}", s.getLeg)
		r.Post("/legs/{id}/answer", s.answerLeg)
		r.Post("/legs/{id}/early-media", s.earlyMediaLeg)
		r.Post("/legs/{id}/ring", s.ringLeg)
		r.Post("/legs/{id}/challenge", s.challengeLeg)
		r.Post("/legs/{id}/mute", s.muteLeg)
		r.Delete("/legs/{id}/mute", s.unmuteLeg)
		r.Post("/legs/{id}/deaf", s.deafLeg)
		r.Delete("/legs/{id}/deaf", s.undeafLeg)
		r.Post("/legs/{id}/hold", s.holdLeg)
		r.Delete("/legs/{id}/hold", s.unholdLeg)
		r.Post("/legs/{id}/transfer", s.transferLeg)
		r.Post("/legs/{id}/transfer/accept", s.acceptTransferLeg)
		r.Post("/legs/{id}/transfer/progress", s.progressTransferLeg)
		r.Post("/legs/{id}/transfer/complete", s.completeTransferLeg)
		r.Post("/legs/{id}/transfer/decline", s.declineTransferLeg)
		r.Delete("/legs/{id}", s.deleteLeg)
		r.Post("/legs/{id}/dtmf", s.sendDTMF)
		r.Post("/legs/{id}/dtmf/accept", s.acceptDTMFLeg)
		r.Post("/legs/{id}/dtmf/reject", s.rejectDTMFLeg)
		r.Post("/legs/{id}/rtt", s.sendRTT)
		r.Post("/legs/{id}/rtt/accept", s.acceptRTTLeg)
		r.Post("/legs/{id}/rtt/reject", s.rejectRTTLeg)
		r.Post("/legs/{id}/play", s.playLeg)
		r.Delete("/legs/{id}/play/{playbackID}", s.stopPlayLeg)
		r.Patch("/legs/{id}/play/{playbackID}", s.volumePlayLeg)
		r.Post("/legs/{id}/tts", s.ttsLeg)
		r.Post("/legs/{id}/record", s.recordLeg)
		r.Delete("/legs/{id}/record", s.stopRecordLeg)
		r.Post("/legs/{id}/record/pause", s.pauseRecordLeg)
		r.Post("/legs/{id}/record/resume", s.resumeRecordLeg)
		r.Post("/legs/{id}/stt", s.sttLeg)
		r.Delete("/legs/{id}/stt", s.stopSTTLeg)
		r.Post("/legs/{id}/agent/elevenlabs", s.agentLegElevenLabs)
		r.Post("/legs/{id}/agent/vapi", s.agentLegVAPI)
		r.Post("/legs/{id}/agent/pipecat", s.agentLegPipecat)
		r.Post("/legs/{id}/agent/deepgram", s.agentLegDeepgram)
		r.Post("/legs/{id}/agent/message", s.agentLegMessage)
		r.Delete("/legs/{id}/agent", s.stopAgentLeg)
		r.Post("/legs/{id}/amd", s.startAMDLeg)
		r.Post("/legs/{id}/ice-candidates", s.webrtcAddCandidate)
		r.Get("/legs/{id}/ice-candidates", s.webrtcGetCandidates)

		// Rooms
		r.Post("/rooms", s.createRoom)
		r.Get("/rooms", s.listRooms)
		r.Get("/rooms/{id}", s.getRoom)
		r.Delete("/rooms/{id}", s.deleteRoom)
		r.Post("/rooms/{id}/legs", s.addLegToRoom)
		r.Delete("/rooms/{id}/legs/{legID}", s.removeLegFromRoom)
		r.Post("/rooms/{id}/bridges", s.createRoomBridge)
		r.Get("/rooms/{id}/bridges", s.listRoomBridges)
		r.Get("/rooms/{id}/bridges/{bridgeID}", s.getRoomBridge)
		r.Patch("/rooms/{id}/bridges/{bridgeID}", s.updateRoomBridge)
		r.Delete("/rooms/{id}/bridges/{bridgeID}", s.deleteRoomBridge)
		r.Get("/rooms/{id}/routing", s.getRoomRouting)
		r.Put("/rooms/{id}/routing", s.setRoomRouting)
		r.Patch("/rooms/{id}/routing", s.updateRoomRouting)
		r.Patch("/legs/{id}/role", s.setLegRole)
		r.Post("/rooms/{id}/play", s.playRoom)
		r.Delete("/rooms/{id}/play/{playbackID}", s.stopPlayRoom)
		r.Patch("/rooms/{id}/play/{playbackID}", s.volumePlayRoom)
		r.Post("/rooms/{id}/tts", s.ttsRoom)
		r.Post("/rooms/{id}/record", s.recordRoom)
		r.Delete("/rooms/{id}/record", s.stopRecordRoom)
		r.Post("/rooms/{id}/record/pause", s.pauseRecordRoom)
		r.Post("/rooms/{id}/record/resume", s.resumeRecordRoom)
		r.Post("/rooms/{id}/stt", s.sttRoom)
		r.Delete("/rooms/{id}/stt", s.stopSTTRoom)
		r.Post("/rooms/{id}/agent/elevenlabs", s.agentRoomElevenLabs)
		r.Post("/rooms/{id}/agent/vapi", s.agentRoomVAPI)
		r.Post("/rooms/{id}/agent/pipecat", s.agentRoomPipecat)
		r.Post("/rooms/{id}/agent/deepgram", s.agentRoomDeepgram)
		r.Post("/rooms/{id}/agent/message", s.agentRoomMessage)
		r.Delete("/rooms/{id}/agent", s.stopAgentRoom)
		r.Get("/rooms/{id}/ws", s.wsRoom)

		// SIP registrations (AOR registry)
		r.Get("/sip/registrations", s.listRegistrations)
		r.Delete("/sip/registrations/{aor}", s.deleteRegistration)

		// Inbound REGISTER auth decisions (parked attempts awaiting a decision)
		r.Post("/sip/registrations/attempts/{id}/challenge", s.challengeRegistrationAttempt)
		r.Post("/sip/registrations/attempts/{id}/accept", s.acceptRegistrationAttempt)
		r.Post("/sip/registrations/attempts/{id}/reject", s.rejectRegistrationAttempt)

		// SIP trunks (outbound registrations / static peering)
		r.Post("/sip/trunks", s.createTrunk)
		r.Get("/sip/trunks", s.listTrunks)
		r.Get("/sip/trunks/{id}", s.getTrunk)
		r.Delete("/sip/trunks/{id}", s.deleteTrunk)

		// Event stream
		r.Get("/vsi", s.vsi)

		// WebRTC
		r.Post("/webrtc/offer", s.webrtcOffer)

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
