//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// tlsRegistrar is a SIP-over-TLS UAS presenting a self-signed certificate whose
// name lives only in the Common Name field — no SAN — exactly what carriers
// with legacy certificates present and what Go rejects out of the box.
type tlsRegistrar struct {
	port      int
	caPath    string
	registers atomic.Int32
	invites   atomic.Int32
}

func newLegacyCNTLSRegistrar(t *testing.T) *tlsRegistrar {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(11),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	caPath := filepath.Join(t.TempDir(), "registrar-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("tls-registrar"), sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("new UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	r := &tlsRegistrar{port: port, caPath: caPath}
	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		r.registers.Add(1)
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		if c := req.GetHeader("Contact"); c != nil {
			res.AppendHeader(sip.NewHeader("Contact", strings.TrimSuffix(c.Value(), ">")+">;expires=120"))
		}
		res.AppendHeader(sip.NewHeader("Expires", "120"))
		_ = tx.Respond(res)
	})

	// Answering INVITEs too makes this fake usable as a TLS-only call target.
	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		r.invites.Add(1)
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusServiceUnavailable, "Service Unavailable", nil))
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = srv.ListenAndServeTLS(ctx, "tls", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
			MinVersion:   tls.VersionTLS12,
		})
	}()
	time.Sleep(150 * time.Millisecond)
	return r
}

// createTLSTrunk registers against the TLS registrar over "sips:" — the URI
// host is "localhost" so the dial exercises name verification, not an IP SAN.
func createTLSTrunk(t *testing.T, baseURL string, port int, insecure bool) string {
	t.Helper()
	return createTLSTrunkHost(t, baseURL, "localhost", port, insecure)
}

func createTLSTrunkHost(t *testing.T, baseURL, host string, port int, insecure bool) string {
	t.Helper()
	spec := map[string]interface{}{
		"registrar_uri":   fmt.Sprintf("sips:%s:%d", host, port),
		"aor":             "sip:alice@" + host,
		"password":        "secret",
		"expires_seconds": 600,
	}
	if insecure {
		spec["tls_insecure_skip_verify"] = true
	}
	resp, body := createTrunkRequest(t, baseURL, map[string]interface{}{
		"type":         "sip_register",
		"sip_register": spec,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST trunk = %d, body=%s", resp.StatusCode, body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("missing trunk id")
	}
	return id
}

// TestTLSTrust_LegacyCNRejectedByDefault pins the out-of-the-box behaviour the
// trust options exist to relax.
func TestTLSTrust_LegacyCNRejectedByDefault(t *testing.T) {
	reg := newLegacyCNTLSRegistrar(t)
	inst := newTestInstance(t, "tls-trust-default")

	id := createTLSTrunk(t, inst.baseURL(), reg.port, false)
	snap := waitForTrunkStatus(t, inst.baseURL(), id, "failed", 5*time.Second)

	lastErr, _ := snap["last_error"].(string)
	if !strings.Contains(lastErr, "x509") {
		t.Errorf("last_error = %q, want a certificate verification failure", lastErr)
	}
	if reg.registers.Load() != 0 {
		t.Error("REGISTER reached the registrar despite the failed handshake")
	}
}

// TestTLSTrust_PerTrunkSkipVerify is the narrow fix: one trunk accepts its own
// peer's certificate, and the snapshot reports that it does.
func TestTLSTrust_PerTrunkSkipVerify(t *testing.T) {
	reg := newLegacyCNTLSRegistrar(t)
	inst := newTestInstance(t, "tls-trust-per-trunk")

	id := createTLSTrunk(t, inst.baseURL(), reg.port, true)
	snap := waitForTrunkStatus(t, inst.baseURL(), id, "active", 5*time.Second)

	if reg.registers.Load() < 1 {
		t.Error("registrar received no REGISTER")
	}
	sub, _ := snap["sip_register"].(map[string]interface{})
	if skip, _ := sub["tls_insecure_skip_verify"].(bool); !skip {
		t.Error("snapshot does not report the per-trunk exemption")
	}
}

// TestTLSTrust_PooledConnectionSharedAcrossTrunks pins a consequence of the
// exemption being applied at handshake time: sipgo pools TLS connections by
// remote address, so a second trunk to the same socket rides the connection the
// exempted trunk opened and never gets verified either. The exemption is
// revoked when the trunk goes away (unit-tested in internal/sip), but an open
// connection to that peer outlives it.
func TestTLSTrust_PooledConnectionSharedAcrossTrunks(t *testing.T) {
	reg := newLegacyCNTLSRegistrar(t)
	inst := newTestInstance(t, "tls-trust-pooled")

	exempt := createTLSTrunk(t, inst.baseURL(), reg.port, true)
	waitForTrunkStatus(t, inst.baseURL(), exempt, "active", 5*time.Second)

	second := createTLSTrunk(t, inst.baseURL(), reg.port, false)
	waitForTrunkStatus(t, inst.baseURL(), second, "active", 5*time.Second)
}

// TestTLSTrust_CAFileStillChecksName — pinning the peer's CA is the secure
// route, and it still enforces the name in the certificate: this registrar has
// none, so the trunk fails even with its cert trusted as a root.
func TestTLSTrust_CAFileStillChecksName(t *testing.T) {
	reg := newLegacyCNTLSRegistrar(t)
	inst := newTestInstanceWithOpts(t, "tls-trust-cafile", func(c *config.Config) {
		c.SIPTLSCAFile = reg.caPath
	})

	id := createTLSTrunk(t, inst.baseURL(), reg.port, false)
	snap := waitForTrunkStatus(t, inst.baseURL(), id, "failed", 5*time.Second)

	lastErr, _ := snap["last_error"].(string)
	if !strings.Contains(lastErr, "SANs") {
		t.Errorf("last_error = %q, want the legacy Common Name failure", lastErr)
	}
}

// TestTLSTrust_InsecureSkipVerify is the server-wide escape hatch.
func TestTLSTrust_InsecureSkipVerify(t *testing.T) {
	reg := newLegacyCNTLSRegistrar(t)
	inst := newTestInstanceWithOpts(t, "tls-trust-insecure", func(c *config.Config) {
		c.SIPTLSInsecure = true
	})

	id := createTLSTrunk(t, inst.baseURL(), reg.port, false)
	waitForTrunkStatus(t, inst.baseURL(), id, "active", 5*time.Second)

	if reg.registers.Load() < 1 {
		t.Error("registrar received no REGISTER")
	}
}

// TestTLSTrust_SipsRecipientDialsTLS — a "sips:" target must be dialled over
// TLS. sipgo's own resolution leaves it on UDP (its sips: upgrade only fires
// from TCP), so the INVITE used to go to a UDP socket nothing listens on; this
// fake speaks TLS only, so receiving the INVITE is the proof.
func TestTLSTrust_SipsRecipientDialsTLS(t *testing.T) {
	peer := newLegacyCNTLSRegistrar(t)
	inst := newTestInstanceWithOpts(t, "tls-sips-leg", func(c *config.Config) {
		c.SIPTLSInsecure = true // the fake's certificate is self-signed
	})

	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type": "sip",
		"to":   fmt.Sprintf("sips:bob@localhost:%d", peer.port),
		"from": "alice",
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if peer.invites.Load() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("sips: INVITE never reached the TLS-only peer")
}
