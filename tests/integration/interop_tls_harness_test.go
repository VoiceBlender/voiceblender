//go:build integration && interop

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// writeSelfSignedCert writes a cert/key pair for 127.0.0.1 into dir.
func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// newInteropVBTLS is newInteropVB plus a SIP TLS listener, the setup a
// deployment placing or taking TLS calls runs with (SIP_TLS_PORT). Wiring
// follows newTestInstanceFull, which has no TLS listener.
func newInteropVBTLS(t *testing.T, name string) (*testInstance, int) {
	t.Helper()
	codecs := []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA, codec.CodecG722, codec.CodecOpus}
	sipPort := reserveSIPPort(t)
	tlsPort := reserveSIPPort(t, "tcp4")
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	log := slog.Default().With("instance", name)

	cfg := config.Config{
		SIPBindIP:                      "127.0.0.1",
		SIPListenIP:                    "127.0.0.1",
		SIPPort:                        fmt.Sprint(sipPort),
		SIPTLSPort:                     fmt.Sprint(tlsPort),
		SIPTLSCert:                     certPath,
		SIPTLSKey:                      keyPath,
		SIPTLSInsecure:                 true,
		SIPHost:                        name,
		HTTPAddr:                       "127.0.0.1:0",
		RecordingDir:                   t.TempDir(),
		DefaultSampleRate:              16000,
		SIPRTCP:                        true,
		RTPPortMin:                     pionPortMin,
		RTPPortMax:                     pionPortMax,
		SIPInboundAuthConsultTimeoutMs: 150,
		SIPInboundRegisterDefault:      "accept",
	}

	bus := events.NewBus("test")
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)
	registrar := sipmod.NewRegistrar(bus, log, sipmod.RegistrarConfig{})

	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:        "127.0.0.1",
		ListenIP:      "127.0.0.1",
		PortAllocator: testRTPAllocator(t),
		BindPort:      sipPort,
		TLSBindPort:   tlsPort,
		TLSCertPath:   certPath,
		TLSKeyPath:    keyPath,
		SIPHost:       name,
		Codecs:        codecs,
		Log:           log,
		Registrar:     registrar,
		RTCPInterval:  harnessRTCPInterval(cfg),
		ClientTLS:     sipmod.ClientTLSConfig{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatalf("[%s] new engine: %v", name, err)
	}
	apiSrv := api.NewServer(legMgr, roomMgr, engine, bus, webhooks, nil, nil, nil, nil, cfg, nil, log)
	engine.OnInvite(apiSrv.HandleInboundCall)
	engine.OnReInvite(apiSrv.HandleReInvite)
	engine.OnUpdate(apiSrv.HandleUpdate)
	engine.OnRefer(apiSrv.HandleIncomingRefer)
	engine.OnNotify(apiSrv.HandleReferNotify)
	engine.OnRegisterAttempt(apiSrv.HandleRegisterAttempt)

	ctx, cancel := context.WithCancel(context.Background())
	registrar.Start(ctx)
	go func() {
		if err := engine.Serve(ctx); err != nil && ctx.Err() == nil {
			log.Error("SIP engine error", "error", err)
		}
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("[%s] listen HTTP: %v", name, err)
	}
	httpSrv := &http.Server{Handler: apiSrv}
	go httpSrv.Serve(ln)
	time.Sleep(200 * time.Millisecond)

	ec := newEventCollector()
	_ = bus.Subscribe(ec.handle)
	inst := &testInstance{
		name: name, cfg: cfg, bus: bus, webhooks: webhooks, legMgr: legMgr, roomMgr: roomMgr,
		engine: engine, apiSrv: apiSrv, httpSrv: httpSrv, httpAddr: ln.Addr().String(),
		sipPort: sipPort, collector: ec, cancel: cancel,
	}
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		httpSrv.Shutdown(sctx)
		engine.Trunks().Shutdown(sctx)
		webhooks.Stop()
		for _, l := range legMgr.List() {
			l.Hangup(context.Background())
		}
	})
	return inst, tlsPort
}
