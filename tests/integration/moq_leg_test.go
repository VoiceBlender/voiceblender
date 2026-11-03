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
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/transport/wt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/moqmedia"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

var (
	moqMicTrack = v22.FullTrackName{Namespace: v22.Namespace{moqmedia.MicNamespace}, Name: moqmedia.AudioTrack}
	moqMixTrack = v22.FullTrackName{Namespace: v22.Namespace{moqmedia.MixNamespace}, Name: moqmedia.AudioTrack}
)

// startMoQListener attaches a WebTransport+HTTP/3 listener to inst and
// wires it into the api.Server. Returns the listen address.
func startMoQListener(t *testing.T, inst *testInstance) string {
	t.Helper()

	cert := generateMoQTestCert(t)

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}

	moqSrv := &webtransport.Server{
		H3: &http3.Server{
			Handler: inst.apiSrv.Router,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				NextProtos:   []string{"h3"},
			},
		},
		ApplicationProtocols: []string{moqmedia.ALPN},
	}
	inst.apiSrv.MoQWebTransport = moqSrv

	go func() {
		if err := moqSrv.Serve(udpConn); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
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

// moqTestClient answers the server's PUBLISH_NAMESPACE for mix and, when
// the server subscribes to mic, publishes one Opus frame of pcm every
// 20 ms until the subscription or session ends.
type moqTestClient struct {
	moqt.RejectAll
	t   *testing.T
	pcm []int16
}

func (c *moqTestClient) HandlePublishNamespace(r *moqt.PublishNamespaceRequest) { _, _ = r.Accept() }

func (c *moqTestClient) HandleSubscribe(r *moqt.SubscribeRequest) {
	if r.Track.Key() != moqMicTrack.Key() {
		_ = r.Reject(v22.ReqErrDoesNotExist, "unknown track")
		return
	}
	tw, err := r.Accept(moqt.AcceptOptions{})
	if err != nil {
		c.t.Logf("client accept SUBSCRIBE mic: %v", err)
		return
	}
	enc, err := codec.NewOpusEncoder()
	if err != nil {
		c.t.Logf("client opus encoder: %v", err)
		return
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for group := uint64(0); ; group++ {
		frame, err := enc.Encode(c.pcm)
		if err != nil {
			c.t.Logf("client opus encode: %v", err)
			return
		}
		sg := tw.OpenSubgroup(group, 0, moqt.SubgroupOptions{EndOfGroup: true, FirstObject: true})
		if err := sg.WriteObject(&v22.Object{ID: 0, Payload: frame}); err != nil {
			return
		}
		_ = sg.Close()
		select {
		case <-tw.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// dialMoQLeg opens a MoQ leg into roomID, publishes the mic namespace so
// the server subscribes to it, and subscribes to the room mix.
func dialMoQLeg(t *testing.T, ctx context.Context, addr, roomID string, micPCM []int16) (*moqt.Session, *moqt.Subscription) {
	t.Helper()
	tr := &webtransport.Transport{
		ApplicationProtocols: []string{moqmedia.ALPN},
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		},
	}
	t.Cleanup(func() { _ = tr.Close() })
	wtURL := (&url.URL{
		Scheme:   "https",
		Host:     addr,
		Path:     "/v1/legs/moq",
		RawQuery: "room_id=" + url.QueryEscape(roomID),
	}).String()

	_, wtSess, err := tr.Dial(ctx, wtURL, nil)
	if err != nil {
		t.Fatalf("webtransport dial: %v", err)
	}
	if p := wtSess.SessionState().ApplicationProtocol; p != moqmedia.ALPN {
		t.Fatalf("negotiated WebTransport protocol %q, want %q", p, moqmedia.ALPN)
	}

	sess, err := moqt.NewSession(ctx, wt.New(wtSess), moqt.Client, moqt.Config{
		Handler: &moqTestClient{t: t, pcm: micPCM},
	})
	if err != nil {
		t.Fatalf("moqt client session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if _, err := sess.PublishNamespace(ctx, moqMicTrack.Namespace, nil); err != nil {
		t.Fatalf("publish mic namespace: %v", err)
	}
	sub, err := sess.Subscribe(ctx, moqMixTrack, nil)
	if err != nil {
		t.Fatalf("subscribe mix: %v", err)
	}
	return sess, sub
}

// awaitMixAudio decodes mix Objects until one has an RMS above minRMS.
// minRMS 0 accepts the first decodable Object.
func awaitMixAudio(t *testing.T, ctx context.Context, sub *moqt.Subscription, minRMS float64) {
	t.Helper()
	dec, err := codec.NewOpusDecoder()
	if err != nil {
		t.Fatalf("opus decoder: %v", err)
	}
	for {
		g, err := sub.AcceptSubgroup(ctx)
		if err != nil {
			t.Fatalf("waiting for mix audio (rms > %.0f): %v", minRMS, err)
		}
		for {
			o, err := g.Next()
			if err != nil {
				break
			}
			samples, err := dec.Decode(o.Payload)
			if err != nil {
				t.Fatalf("decode mix object: %v", err)
			}
			if minRMS == 0 || moqRMS(samples) > minRMS {
				return
			}
		}
	}
}

func moqRMS(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

// TestMoQLegInbound_AudioRoundTrip dials a draft-22 MoQ client against a
// running VoiceBlender, publishes the mic namespace, answers the server's
// SUBSCRIBE with Opus frames, subscribes to the mix and verifies a mix
// Object flows back.
func TestMoQLegInbound_AudioRoundTrip(t *testing.T) {
	inst := newTestInstance(t, "moq-inbound")
	moqAddr := startMoQListener(t, inst)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, sub := dialMoQLeg(t, ctx, moqAddr, "moq-room", make([]int16, 960))

	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() != ""
	}, 5*time.Second)

	awaitMixAudio(t, ctx, sub, 0)
}

// TestMoQLeg_TwoLegsExchangeAudio puts two MoQ legs in one room: a tone
// published on leg A's mic must be audible in leg B's mix.
func TestMoQLeg_TwoLegsExchangeAudio(t *testing.T) {
	inst := newTestInstance(t, "moq-pair")
	moqAddr := startMoQListener(t, inst)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tone := generateSineSamples(48000, 440, 20*time.Millisecond, 0.3)
	dialMoQLeg(t, ctx, moqAddr, "moq-pair-room", tone)
	_, subB := dialMoQLeg(t, ctx, moqAddr, "moq-pair-room", make([]int16, 960))

	awaitMixAudio(t, ctx, subB, 1000)
}

// TestMoQLeg_ClientCloseDisconnects closes the client session and expects
// leg.disconnected with reason "hangup".
func TestMoQLeg_ClientCloseDisconnects(t *testing.T) {
	inst := newTestInstance(t, "moq-close")
	moqAddr := startMoQListener(t, inst)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess, sub := dialMoQLeg(t, ctx, moqAddr, "moq-close-room", make([]int16, 960))
	connected := inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() != ""
	}, 5*time.Second)
	legID := connected.Data.GetLegID()
	awaitMixAudio(t, ctx, sub, 0)

	_ = sess.Close()

	disc := inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 5*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "hangup" {
		t.Fatalf("disconnect reason = %q, want hangup", got)
	}
}
