package sip

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLegacyCNCert writes a self-signed cert whose name lives only in the
// Common Name field — no SAN — which is what carriers like sip.vpbx.pl present
// and what Go rejects outright on hostname verification.
func writeLegacyCNCert(t *testing.T, dir, commonName string) (certPath string, cert tls.Certificate) {
	t.Helper()
	return writeTestCert(t, dir, commonName, nil)
}

func writeTestCert(t *testing.T, dir, commonName string, sans []string) (certPath string, cert tls.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPath = filepath.Join(dir, commonName+".pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return certPath, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: parsed}
}

// dialTLS runs a one-shot TLS server presenting srvCert and dials it with
// clientCfg, using serverName the way sipgo does — it fills ServerName in from
// the dialed URI host.
func dialTLS(t *testing.T, srvCert tls.Certificate, clientCfg *tls.Config, serverName string) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{srvCert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.(*tls.Conn).Handshake()
		conn.Close()
	}()

	cfg := clientCfg.Clone()
	cfg.ServerName = serverName
	conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func TestBuildClientTLSConfig_Defaults(t *testing.T) {
	cfg, err := buildClientTLSConfig(ClientTLSConfig{}, newPeerTrust())
	if err != nil {
		t.Fatalf("buildClientTLSConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want %x", cfg.MinVersion, tls.VersionTLS12)
	}
	if cfg.RootCAs != nil {
		t.Error("no CA file configured must leave the system trust store in place")
	}
	// Verification is not skipped — it moves into VerifyConnection, which is
	// the only way per-peer exemptions are reachable.
	if cfg.VerifyConnection == nil {
		t.Fatal("VerifyConnection hook missing")
	}
	if (ClientTLSConfig{}).Enabled() {
		t.Error("zero ClientTLSConfig must not report Enabled")
	}
}

func TestBuildClientTLSConfig_CAFileErrors(t *testing.T) {
	dir := t.TempDir()

	if _, err := buildClientTLSConfig(ClientTLSConfig{CAFile: filepath.Join(dir, "nope.pem")}, nil); err == nil {
		t.Error("missing CA file must fail")
	}

	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildClientTLSConfig(ClientTLSConfig{CAFile: junk}, nil)
	if err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("junk CA file error = %v, want 'no PEM certificate'", err)
	}
}

func TestClientTLS_UntrustedPeerRejectedByDefault(t *testing.T) {
	_, srvCert := writeLegacyCNCert(t, t.TempDir(), "sbc.example.net")

	cfg, err := buildClientTLSConfig(ClientTLSConfig{}, newPeerTrust())
	if err != nil {
		t.Fatalf("buildClientTLSConfig: %v", err)
	}
	if err := dialTLS(t, srvCert, cfg, "sbc.example.net"); err == nil {
		t.Fatal("handshake with an untrusted self-signed peer must fail")
	}
}

// TestClientTLS_CAFileVerifiesName pins that trusting a CA does not also stop
// the name in the certificate being checked.
func TestClientTLS_CAFileVerifiesName(t *testing.T) {
	dir := t.TempDir()
	caPath, srvCert := writeTestCert(t, dir, "sbc.example.net", []string{"sbc.example.net"})

	cfg, err := buildClientTLSConfig(ClientTLSConfig{CAFile: caPath}, newPeerTrust())
	if err != nil {
		t.Fatalf("buildClientTLSConfig: %v", err)
	}
	if err := dialTLS(t, srvCert, cfg, "sbc.example.net"); err != nil {
		t.Fatalf("pinned cert with a matching SAN: %v", err)
	}
	if err := dialTLS(t, srvCert, cfg, "other.example.net"); err == nil {
		t.Fatal("a pinned cert must still fail for a host it does not name")
	}
}

// TestClientTLS_CAFileAloneStillFailsOnLegacyCN is why the per-trunk exemption
// exists: a SAN-less certificate cannot be rescued by trusting it as a root.
func TestClientTLS_CAFileAloneStillFailsOnLegacyCN(t *testing.T) {
	caPath, srvCert := writeLegacyCNCert(t, t.TempDir(), "sbc.example.net")

	cfg, err := buildClientTLSConfig(ClientTLSConfig{CAFile: caPath}, newPeerTrust())
	if err != nil {
		t.Fatalf("buildClientTLSConfig: %v", err)
	}
	err = dialTLS(t, srvCert, cfg, "sbc.example.net")
	if err == nil {
		t.Fatal("a pinned but SAN-less cert must still fail name verification")
	}
	if !strings.Contains(err.Error(), "SANs") {
		t.Errorf("error = %v, want legacy Common Name failure", err)
	}
}

func TestClientTLS_PeerTrustExemptsOnlyThatHost(t *testing.T) {
	dir := t.TempDir()
	_, legacyCert := writeLegacyCNCert(t, dir, "sbc.example.net")
	_, otherCert := writeLegacyCNCert(t, dir, "other.example.net")

	trust := newPeerTrust()
	trust.add("SBC.Example.NET.") // case and trailing dot must not matter
	cfg, err := buildClientTLSConfig(ClientTLSConfig{}, trust)
	if err != nil {
		t.Fatalf("buildClientTLSConfig: %v", err)
	}

	if err := dialTLS(t, legacyCert, cfg, "sbc.example.net"); err != nil {
		t.Fatalf("exempted peer: %v", err)
	}
	if err := dialTLS(t, otherCert, cfg, "other.example.net"); err == nil {
		t.Fatal("a peer that was never exempted must still be verified")
	}

	trust.remove("sbc.example.net")
	if err := dialTLS(t, legacyCert, cfg, "sbc.example.net"); err == nil {
		t.Fatal("removing the exemption must restore verification")
	}
}

func TestPeerTrust_Refcounted(t *testing.T) {
	trust := newPeerTrust()
	trust.add("sbc.example.net")
	trust.add("sbc.example.net")
	trust.remove("sbc.example.net")
	if !trust.trusted("sbc.example.net") {
		t.Error("a second trunk's exemption must survive the first being removed")
	}
	trust.remove("sbc.example.net")
	if trust.trusted("sbc.example.net") {
		t.Error("exemption outlived the last holder")
	}
	trust.remove("sbc.example.net") // underflow must not resurrect it
	if trust.trusted("sbc.example.net") {
		t.Error("extra remove resurrected the exemption")
	}
	if trust.trusted("") {
		t.Error("an empty SNI must never match an exemption")
	}
}

// TestClientTLS_IPPeerHasNoSNI documents the limit of per-trunk exemptions:
// a peer dialed by IP literal sends no SNI, so the hook cannot tell it apart
// and it is chain-verified without a name check.
func TestClientTLS_IPPeerHasNoSNI(t *testing.T) {
	dir := t.TempDir()
	caPath, srvCert := writeLegacyCNCert(t, dir, "sbc.example.net")

	trust := newPeerTrust()
	trust.add("127.0.0.1")
	cfg, err := buildClientTLSConfig(ClientTLSConfig{CAFile: caPath}, trust)
	if err != nil {
		t.Fatalf("buildClientTLSConfig: %v", err)
	}
	// Trusted chain, no name to check → accepted; the exemption played no part.
	if err := dialTLS(t, srvCert, cfg, "127.0.0.1"); err != nil {
		t.Fatalf("IP-dialed peer with a pinned chain: %v", err)
	}

	_, unpinnedCert := writeLegacyCNCert(t, dir, "impostor.example.net")
	if err := dialTLS(t, unpinnedCert, cfg, "127.0.0.1"); err == nil {
		t.Fatal("an IP-dialed peer outside the roots must still be rejected")
	}
}

func TestClientTLS_InsecureSkipVerifyAcceptsAnything(t *testing.T) {
	_, srvCert := writeLegacyCNCert(t, t.TempDir(), "whatever.example.net")

	cfg, err := buildClientTLSConfig(ClientTLSConfig{InsecureSkipVerify: true}, newPeerTrust())
	if err != nil {
		t.Fatalf("buildClientTLSConfig: %v", err)
	}
	if err := dialTLS(t, srvCert, cfg, "unrelated.example.org"); err != nil {
		t.Fatalf("handshake with verification disabled: %v", err)
	}
	if err := dialTLS(t, srvCert, cfg, "127.0.0.1"); err != nil {
		t.Fatalf("IP-dialed handshake with verification disabled: %v", err)
	}
}

func TestVerifyPeerChain_NoCertificate(t *testing.T) {
	if err := verifyPeerChain(tls.ConnectionState{}, nil); err == nil {
		t.Error("empty certificate list must be rejected")
	}
}
