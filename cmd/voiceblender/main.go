package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/metrics"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/VoiceBlender/voiceblender/internal/storage"
	"github.com/VoiceBlender/voiceblender/internal/tts"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"golang.org/x/sync/errgroup"
)

var version = "dev"

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
	bus := events.NewBus(cfg.InstanceID)
	_ = bus.Subscribe(func(e events.Event) {
		log.Info("event", "type", string(e.Type), "data", e.Data)
	})
	webhookReg := events.NewWebhookRegistry(bus, log, cfg.WebhookURL, cfg.WebhookSecret)
	defer webhookReg.Stop()

	log.Info("starting voiceblender", "version", version)
	log.Info("config loaded", "default_sample_rate", cfg.DefaultSampleRate)

	// Leg and room managers
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)

	// Parse SIP port
	sipPort, err := strconv.Atoi(cfg.SIPPort)
	if err != nil {
		log.Error("invalid SIP_PORT", "error", err)
		os.Exit(1)
	}

	// Parse optional SIP TLS port
	var sipTLSPort int
	if cfg.SIPTLSPort != "" {
		sipTLSPort, err = strconv.Atoi(cfg.SIPTLSPort)
		if err != nil {
			log.Error("invalid SIP_TLS_PORT", "error", err)
			os.Exit(1)
		}
	}

	// RTP port allocator (nil when range not configured)
	portAlloc, err := sipmod.NewPortAllocator(cfg.RTPPortMin, cfg.RTPPortMax)
	if err != nil {
		log.Error("invalid RTP port range", "error", err)
		os.Exit(1)
	}
	if portAlloc != nil {
		log.Info("RTP port range configured", "min", cfg.RTPPortMin, "max", cfg.RTPPortMax)
	}

	// AOR registrar (in-memory).
	registrar := sipmod.NewRegistrar(bus, log, sipmod.RegistrarConfig{
		DefaultExpiresSeconds: cfg.SIPRegistrationDefaultExpiresSeconds,
		MaxExpiresSeconds:     cfg.SIPRegistrationMaxExpiresSeconds,
		SweepInterval:         time.Duration(cfg.SIPRegistrationSweepIntervalMs) * time.Millisecond,
		AllowMultipleContacts: cfg.SIPRegistrationAllowMultipleContacts,
	})
	registrar.Start(ctx)

	// SIP engine (replaces diago)
	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:            cfg.SIPBindIP,
		BindIPV6:          cfg.SIPBindIPV6,
		ListenIP:          cfg.SIPListenIP,
		ListenIPV6:        cfg.SIPListenIPV6,
		ExternalIP:        cfg.SIPExternalIP,
		PublicHost:        cfg.SIPDomain,
		BindPort:          sipPort,
		TLSBindPort:       sipTLSPort,
		TLSCertPath:       cfg.SIPTLSCert,
		TLSKeyPath:        cfg.SIPTLSKey,
		SIPDebug:          cfg.SIPDebug,
		SIPHost:           cfg.SIPHost,
		UseSourceSocket:   cfg.SIPUseSourceSocket,
		Codecs:            cfg.Codecs,
		AMRWBMode:         cfg.AMRWBMode,
		AMRWBOctetAligned: cfg.AMRWBOctetAligned,
		AMRNBMode:         cfg.AMRNBMode,
		AMRNBOctetAligned: cfg.AMRNBOctetAligned,
		Log:               log,
		PortAllocator:     portAlloc,
		Registrar:         registrar,
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

	// TTS cache (optional)
	var ttsCache *tts.Cache
	if cfg.TTSCacheEnabled {
		c, err := tts.NewCache(cfg.TTSCacheDir, cfg.TTSCacheIncludeAPIKey, log)
		if err != nil {
			log.Error("failed to create TTS cache", "error", err)
			os.Exit(1)
		}
		ttsCache = c
		log.Info("TTS cache enabled", "dir", cfg.TTSCacheDir)
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

	// Prometheus metrics collector
	metricsCollector := metrics.New(bus)

	// HTTP API server
	allowedIPs, err := api.ParseAllowedIPs(cfg.AllowedIPs)
	if err != nil {
		log.Error("invalid ALLOWED_IPS", "error", err)
		os.Exit(1)
	}
	if len(allowedIPs) > 0 {
		log.Info("HTTP IP allowlist enabled", "prefixes", len(allowedIPs), "trust_proxy_headers", cfg.TrustProxyHeaders)
	}
	apiSrv := api.NewServer(legMgr, roomMgr, engine, bus, webhookReg, ttsProvider, ttsCache, s3Backend, metricsCollector, cfg, allowedIPs, log)
	httpSrv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: apiSrv,
	}

	// Optional MoQ-over-WebTransport listener. Reuses chi (apiSrv.Router)
	// as the HTTP/3 handler so /v1/legs/moq routes the same way the rest
	// of the API does; the CONNECT method gates the route to extended-
	// CONNECT requests only.
	var moqSrv *webtransport.Server
	if cfg.MoQEnabled {
		if cfg.MoQTLSCertFile == "" || cfg.MoQTLSKeyFile == "" {
			log.Error("MOQ_ENABLED=true requires MOQ_TLS_CERT_FILE and MOQ_TLS_KEY_FILE")
			os.Exit(1)
		}
		cert, err := tls.LoadX509KeyPair(cfg.MoQTLSCertFile, cfg.MoQTLSKeyFile)
		if err != nil {
			log.Error("load MoQ TLS cert/key", "error", err)
			os.Exit(1)
		}
		moqSrv = &webtransport.Server{
			H3: http3.Server{
				Addr:    cfg.MoQListenAddr,
				Handler: apiSrv.Router,
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{cert},
					NextProtos:   []string{"h3"},
				},
			},
			// PoC: accept any Origin. Browser demos served from a different
			// host/port than the MoQ endpoint would otherwise fail the
			// same-origin check. Tighten before any production use.
			CheckOrigin: func(*http.Request) bool { return true },
		}
		apiSrv.MoQWebTransport = moqSrv
	}

	// Register inbound call handler
	engine.OnInvite(apiSrv.HandleInboundCall)

	// Register re-INVITE handler for hold/unhold detection
	engine.OnReInvite(apiSrv.HandleReInvite)

	// Register UPDATE handler for session-timer refresh and in-dialog media renegotiation (RFC 3311, RFC 4028).
	engine.OnUpdate(apiSrv.HandleUpdate)

	// Register REFER + NOTIFY handlers for SIP transfer (RFC 3515).
	engine.OnRefer(apiSrv.HandleIncomingRefer)
	engine.OnNotify(apiSrv.HandleReferNotify)

	// Run SIP and HTTP servers
	g, gCtx := errgroup.WithContext(ctx)

	log.Info("instance", "id", cfg.InstanceID)

	// SIP server
	g.Go(func() error {
		args := []any{"bind", cfg.SIPBindIP, "port", sipPort}
		if cfg.SIPBindIPV6 != "" {
			args = append(args, "bind_v6", cfg.SIPBindIPV6)
		}
		if sipTLSPort > 0 {
			args = append(args, "tls_port", sipTLSPort)
		}
		log.Info("starting SIP server", args...)
		return engine.Serve(gCtx)
	})

	// HTTP server
	g.Go(func() error {
		log.Info("starting HTTP server", "addr", cfg.HTTPAddr)
		return httpSrv.ListenAndServe()
	})

	// MoQ-over-WebTransport server (HTTP/3 over UDP). Listens on
	// MoQ_LISTEN_ADDR (UDP) with the same chi router as the TCP HTTP
	// server above.
	if moqSrv != nil {
		g.Go(func() error {
			log.Info("starting MoQ (WebTransport/H3) server", "addr", cfg.MoQListenAddr)
			err := moqSrv.ListenAndServe()
			if err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		})
	}

	// Graceful shutdown
	g.Go(func() error {
		<-gCtx.Done()
		log.Info("shutting down")

		// Shutdown HTTP
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
		if moqSrv != nil {
			_ = moqSrv.Close()
		}

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
