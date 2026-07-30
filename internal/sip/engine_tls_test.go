package sip

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "voiceblender-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	certOut.Close()
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyOut.Close()
	return certPath, keyPath
}

func pickFreePort(t *testing.T, network string) int {
	t.Helper()
	switch network {
	case "tcp":
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pick tcp port: %v", err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		return p
	case "udp":
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			t.Fatalf("pick udp port: %v", err)
		}
		p := c.LocalAddr().(*net.UDPAddr).Port
		c.Close()
		return p
	}
	t.Fatalf("unknown network %q", network)
	return 0
}

func TestEngine_NewEngine_TLSRejectsMissingCerts(t *testing.T) {
	_, err := NewEngine(EngineConfig{
		BindIP:      "127.0.0.1",
		BindPort:    pickFreePort(t, "udp"),
		TLSBindPort: pickFreePort(t, "tcp"),
		SIPHost:     "test",
		Codecs:      []codec.CodecType{codec.CodecPCMU},
		Log:         slog.Default(),
	})
	if err == nil {
		t.Fatalf("expected error when TLS port set but cert paths missing")
	}
}

func TestEngine_NewEngine_TLSRejectsBadCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte("not a cert"), 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a key"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, err := NewEngine(EngineConfig{
		BindIP:      "127.0.0.1",
		BindPort:    pickFreePort(t, "udp"),
		TLSBindPort: pickFreePort(t, "tcp"),
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
		SIPHost:     "test",
		Codecs:      []codec.CodecType{codec.CodecPCMU},
		Log:         slog.Default(),
	})
	if err == nil {
		t.Fatalf("expected error loading invalid cert")
	}
}

func TestEngine_Serve_AcceptsTLSHandshake(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())
	udpPort := pickFreePort(t, "udp")
	tlsPort := pickFreePort(t, "tcp")

	engine, err := NewEngine(EngineConfig{
		BindIP:      "127.0.0.1",
		BindPort:    udpPort,
		TLSBindPort: tlsPort,
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
		SIPHost:     "test",
		Codecs:      []codec.CodecType{codec.CodecPCMU},
		Log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if engine.TLSPort() != tlsPort {
		t.Errorf("TLSPort() = %d, want %d", engine.TLSPort(), tlsPort)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- engine.Serve(ctx) }()

	// Poll the TLS port until it accepts a handshake (listener startup is async).
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tlsPort), &tls.Config{InsecureSkipVerify: true})
		if err == nil {
			conn.Close()
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("TLS handshake never succeeded: %v", lastErr)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve did not return after ctx cancel")
	}
}

func TestEngine_Serve_UDPOnlyWhenTLSDisabled(t *testing.T) {
	udpPort := pickFreePort(t, "udp")
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: udpPort,
		SIPHost:  "test",
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if engine.TLSPort() != 0 {
		t.Errorf("TLSPort() = %d, want 0", engine.TLSPort())
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- engine.Serve(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve did not return after ctx cancel")
	}
}
