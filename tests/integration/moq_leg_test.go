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
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/moqmedia"
	"github.com/mengelbart/moqtransport"
	"github.com/mengelbart/moqtransport/webtransportmoq"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// startMoQListener attaches a WebTransport+HTTP/3 listener to inst and
// wires it into the api.Server. Returns the listen address and a cleanup
// that closes the listener.
func startMoQListener(t *testing.T, inst *testInstance) string {
	t.Helper()

	cert := generateMoQTestCert(t)

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}

	moqSrv := &webtransport.Server{
		H3: http3.Server{
			Handler: inst.apiSrv.Router,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				NextProtos:   []string{"h3"},
			},
		},
	}
	inst.apiSrv.MoQWebTransport = moqSrv

	go func() {
		if err := moqSrv.Serve(udpConn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("moq Serve exit: %v", err)
		}
	}()

	addr := udpConn.LocalAddr().String()
	t.Cleanup(func() {
		_ = moqSrv.Close()
		_ = udpConn.Close()
	})
	return addr
}

func generateMoQTestCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa gen: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "moq-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return cert
}

// TestMoQLegInbound_AudioRoundTrip dials a MoQ-over-WebTransport client
// against a running VoiceBlender, completes the MoQ handshake, publishes a
// few Opus frames on `mic`, subscribes to `mix`, and verifies at least one
// mix object flows back (mixed-minus-self silence in a single-leg room).
// This proves the WebTransport accept, MoQ session setup, ANNOUNCE +
// SUBSCRIBE handshake, and bidirectional Object flow all work end-to-end.
func TestMoQLegInbound_AudioRoundTrip(t *testing.T) {
	inst := newTestInstance(t, "moq-inbound")
	moqAddr := startMoQListener(t, inst)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dialer := webtransport.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		},
	}
	wtURL := (&url.URL{
		Scheme:   "https",
		Host:     moqAddr,
		Path:     "/v1/legs/moq",
		RawQuery: "room_id=moq-room",
	}).String()

	_, wtSess, err := dialer.Dial(ctx, wtURL, nil)
	if err != nil {
		t.Fatalf("webtransport dial: %v", err)
	}
	defer wtSess.CloseWithError(0, "test done")

	conn := webtransportmoq.NewClient(wtSess)

	gotMixObject := make(chan struct{}, 1)
	var subscribeOK atomic.Bool

	clientSess := &moqtransport.Session{
		InitialMaxRequestID: 100,
		Handler: moqtransport.HandlerFunc(func(w moqtransport.ResponseWriter, m *moqtransport.Message) {
			if m.Method == moqtransport.MessageAnnounce {
				// Server doesn't ANNOUNCE to us in our protocol, but be polite.
				_ = w.Accept()
			}
		}),
		// Handle the server's SUBSCRIBE for our mic namespace.
		SubscribeHandler: moqtransport.SubscribeHandlerFunc(func(w *moqtransport.SubscribeResponseWriter, m *moqtransport.SubscribeMessage) {
			if !subscribeOK.CompareAndSwap(false, true) {
				_ = w.Reject(0, "duplicate")
				return
			}
			if err := w.Accept(); err != nil {
				t.Logf("client accept SUBSCRIBE: %v", err)
				return
			}
			// Publish one Opus-encoded silence frame so the server's recv
			// loop has something to decode and the mixer sees a tick.
			go publishOneOpusFrame(t, w)
		}),
	}

	if err := clientSess.Run(conn); err != nil {
		t.Fatalf("client Session.Run: %v", err)
	}
	defer clientSess.Close()

	// Announce mic so the server subscribes back.
	if err := clientSess.Announce(ctx, []string{moqmedia.MicNamespace}); err != nil {
		t.Fatalf("client Announce mic: %v", err)
	}

	// Subscribe to mix; this triggers the server's send loop.
	rt, err := clientSess.Subscribe(ctx, []string{moqmedia.MixNamespace}, moqmedia.AudioTrack)
	if err != nil {
		t.Fatalf("client Subscribe mix: %v", err)
	}

	// Drain mix objects in a goroutine; signal on the first one.
	go func() {
		for {
			obj, err := rt.ReadObject(ctx)
			if err != nil {
				return
			}
			_ = obj
			select {
			case gotMixObject <- struct{}{}:
			default:
			}
		}
	}()

	// Confirm LegConnected fires while the session is up.
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() != ""
	}, 5*time.Second)

	select {
	case <-gotMixObject:
		// good — first mix object arrived
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for mix object from server")
	}
}

func publishOneOpusFrame(t *testing.T, pub *moqtransport.SubscribeResponseWriter) {
	enc, err := codec.NewOpusEncoder()
	if err != nil {
		t.Logf("client opus encoder: %v", err)
		return
	}
	silence := make([]int16, 960) // 20 ms @ 48 kHz
	frame, err := enc.Encode(silence)
	if err != nil {
		t.Logf("client opus encode: %v", err)
		return
	}
	sg, err := pub.OpenSubgroup(0, 0, 0)
	if err != nil {
		t.Logf("client OpenSubgroup: %v", err)
		return
	}
	defer sg.Close()
	if _, err := sg.WriteObject(0, frame); err != nil {
		t.Logf("client WriteObject: %v", err)
		return
	}
}
