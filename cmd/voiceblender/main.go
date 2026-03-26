package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/VoiceBlender/voiceblender/internal/storage"
	"github.com/VoiceBlender/voiceblender/internal/tts"
	"golang.org/x/sync/errgroup"
)

func main() {
	cfg := config.Load()

	// Logger
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Root context with signal handling
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Event bus + webhooks
	bus := events.NewBus()
	bus.Subscribe(func(e events.Event) {
		log.Info("event", "type", string(e.Type), "data", e.Data)
	})
	webhookReg := events.NewWebhookRegistry(bus, log)
	defer webhookReg.Stop()

	// Leg and room managers
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)

	// Parse SIP port
	sipPort, err := strconv.Atoi(cfg.SIPPort)
	if err != nil {
		log.Error("invalid SIP_PORT", "error", err)
		os.Exit(1)
	}

	// SIP engine (replaces diago)
	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:   cfg.SIPBindIP,
		ListenIP: cfg.SIPListenIP,
		BindPort: sipPort,
		SIPHost:  cfg.SIPHost,
		Codecs:   []codec.CodecType{codec.CodecOpus, codec.CodecG722, codec.CodecPCMU, codec.CodecPCMA},
		Log:      log,
	})
	if err != nil {
		log.Error("failed to create SIP engine", "error", err)
		os.Exit(1)
	}

	// TTS provider (always created; per-request API key can override env var)
	ttsProvider := tts.NewElevenLabs(cfg.ElevenLabsAPIKey, log)
	if cfg.ElevenLabsAPIKey != "" {
		log.Info("ElevenLabs TTS enabled (default API key set)")
	} else {
		log.Info("ElevenLabs TTS enabled (no default API key; per-request key required)")
	}

	// S3 storage backend (optional)
	var s3Backend storage.Backend
	if cfg.S3Bucket != "" {
		b, err := storage.NewS3Backend(ctx, storage.S3Config{
			Bucket:   cfg.S3Bucket,
			Region:   cfg.S3Region,
			Endpoint: cfg.S3Endpoint,
			Prefix:   cfg.S3Prefix,
		})
		if err != nil {
			log.Error("failed to create S3 backend", "error", err)
			os.Exit(1)
		}
		log.Info("S3 storage enabled", "bucket", cfg.S3Bucket, "region", cfg.S3Region)
		s3Backend = b
	}

	// HTTP API server
	apiSrv := api.NewServer(legMgr, roomMgr, engine, bus, webhookReg, ttsProvider, s3Backend, cfg, log)
	httpSrv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: apiSrv,
	}

	// Register inbound call handler
	engine.OnInvite(apiSrv.HandleInboundCall)

	// Register re-INVITE handler for hold/unhold detection
	engine.OnReInvite(apiSrv.HandleReInvite)

	// Run SIP and HTTP servers
	g, gCtx := errgroup.WithContext(ctx)

	// SIP server
	g.Go(func() error {
		log.Info("starting SIP server", "bind", cfg.SIPBindIP, "port", sipPort)
		return engine.Serve(gCtx)
	})

	// HTTP server
	g.Go(func() error {
		log.Info("starting HTTP server", "addr", cfg.HTTPAddr)
		return httpSrv.ListenAndServe()
	})

	// Graceful shutdown
	g.Go(func() error {
		<-gCtx.Done()
		log.Info("shutting down")

		// Shutdown HTTP
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5000000000) // 5 seconds
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)

		// Hangup all active legs
		for _, l := range legMgr.List() {
			l.Hangup(shutdownCtx)
		}
		return nil
	})

	if err := g.Wait(); err != nil && err != http.ErrServerClosed {
		log.Error("server error", "error", err)
		os.Exit(1)
	}

	log.Info("shutdown complete")
}
