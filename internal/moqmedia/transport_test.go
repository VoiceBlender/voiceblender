package moqmedia

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

const toneRMSThreshold = 1000

// startTestServer serves UpgradeServer on a loopback HTTP/3 listener. Each
// accepted Transport is started with audioOut and sent on the returned channel.
func startTestServer(t *testing.T, audioOut io.Reader) (string, <-chan *Transport) {
	t.Helper()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	trCh := make(chan *Transport, 4)
	srv := &webtransport.Server{
		H3: &http3.Server{
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{testCert(t)}, NextProtos: []string{http3.NextProtoH3}},
		},
		ApplicationProtocols: []string{ALPN},
	}
	srv.H3.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr, err := UpgradeServer(w, r, srv, Config{Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Logf("UpgradeServer: %v", err)
			return
		}
		tr.Start(audioOut)
		trCh <- tr
		<-tr.Done()
	})
	go func() { _ = srv.Serve(udp) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = udp.Close()
	})
	return "https://" + udp.LocalAddr().String() + "/moq", trCh
}

func dialTestClient(t *testing.T, rawURL string, h moqt.Handler) *moqt.Session {
	t.Helper()
	sess, _ := dialTestClientConn(t, rawURL, h)
	return sess
}

// dialTestClientConn also returns the client's QUIC connection. It is
// dialled directly so closing the MoQ session leaves the connection open,
// letting the session close reach the server on its own.
func dialTestClientConn(t *testing.T, rawURL string, h moqt.Handler) (*moqt.Session, *quic.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	qconn, err := quic.DialAddrEarly(ctx, u.Host,
		&tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}},
		&quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true})
	if err != nil {
		t.Fatalf("quic dial: %v", err)
	}
	t.Cleanup(func() { _ = qconn.CloseWithError(0, "") })
	cc, err := (&webtransport.Transport{ApplicationProtocols: []string{ALPN}}).NewClientConn(qconn)
	if err != nil {
		t.Fatalf("webtransport client conn: %v", err)
	}
	_, ws, err := cc.Dial(ctx, rawURL, nil)
	if err != nil {
		t.Fatalf("webtransport dial: %v", err)
	}
	sess, err := moqt.NewSession(ctx, wt.New(ws), moqt.Client, moqt.Config{Handler: h})
	if err != nil {
		t.Fatalf("moqt client session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, qconn
}

func awaitTransport(t *testing.T, ch <-chan *Transport) *Transport {
	t.Helper()
	select {
	case tr := <-ch:
		return tr
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the MoQ session")
		return nil
	}
}

// acceptMixClient answers the server's PUBLISH_NAMESPACE for mix and
// serves a tone on the mic track when the server subscribes to it.
type acceptMixClient struct {
	moqt.RejectAll
	micFrames [][]byte
}

func (acceptMixClient) HandlePublishNamespace(r *moqt.PublishNamespaceRequest) { _, _ = r.Accept() }

func (c acceptMixClient) HandleSubscribe(r *moqt.SubscribeRequest) {
	if r.Track.Key() != micTrack.Key() || c.micFrames == nil {
		_ = r.Reject(v22.ReqErrDoesNotExist, "")
		return
	}
	tw, err := r.Accept(moqt.AcceptOptions{})
	if err != nil {
		return
	}
	for g, frame := range c.micFrames {
		if tw.Context().Err() != nil {
			return
		}
		sg := tw.OpenSubgroup(uint64(g), 0, moqt.SubgroupOptions{EndOfGroup: true, FirstObject: true})
		if err := sg.WriteObject(&v22.Object{ID: 0, Payload: frame}); err != nil {
			return
		}
		_ = sg.Close()
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTransport_MixDownlinkCarriesAudio(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		frame := pcmBytes(tone(DefaultSampleRate * DefaultFrameMs / 1000))
		for {
			if _, err := pw.Write(frame); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	endpoint, trCh := startTestServer(t, pr)
	client := dialTestClient(t, endpoint, acceptMixClient{})
	awaitTransport(t, trCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := client.Subscribe(ctx, v22.FullTrackName{Namespace: mixNamespace, Name: AudioTrack}, nil)
	if err != nil {
		t.Fatalf("subscribe mix: %v", err)
	}
	dec, err := codec.NewOpusDecoder()
	if err != nil {
		t.Fatal(err)
	}
	for {
		g, err := sub.AcceptSubgroup(ctx)
		if err != nil {
			t.Fatalf("no audible mix object before %v", err)
		}
		o, err := g.Next()
		if err != nil {
			continue
		}
		samples, err := dec.Decode(o.Payload)
		if err != nil {
			t.Fatalf("decode mix object: %v", err)
		}
		if rms(samples) > toneRMSThreshold {
			return
		}
	}
}

func TestTransport_MicUplinkReachesAudioReader(t *testing.T) {
	pr, _ := io.Pipe()
	endpoint, trCh := startTestServer(t, pr)

	enc, err := codec.NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	pcm := tone(DefaultSampleRate * DefaultFrameMs / 1000)
	var frames [][]byte
	for range 50 {
		f, err := enc.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, append([]byte(nil), f...))
	}
	client := dialTestClient(t, endpoint, acceptMixClient{micFrames: frames})
	tr := awaitTransport(t, trCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.PublishNamespace(ctx, micTrack.Namespace, nil); err != nil {
		t.Fatalf("publish mic namespace: %v", err)
	}

	got := make(chan bool, 1)
	go func() {
		buf := make([]byte, DefaultSampleRate*DefaultFrameMs/1000*2)
		for {
			if _, err := io.ReadFull(tr.AudioReader(), buf); err != nil {
				got <- false
				return
			}
			samples := make([]int16, len(buf)/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
			}
			if rms(samples) > toneRMSThreshold {
				got <- true
				return
			}
		}
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("audio reader closed before any audible mic audio")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for mic audio on the server")
	}
}

func TestTransport_RejectsUnknownTrack(t *testing.T) {
	pr, _ := io.Pipe()
	endpoint, trCh := startTestServer(t, pr)
	client := dialTestClient(t, endpoint, acceptMixClient{})
	awaitTransport(t, trCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Subscribe(ctx, v22.FullTrackName{Namespace: mixNamespace, Name: "video"}, nil)
	var re *moqt.RequestError
	if !errors.As(err, &re) || re.Code != v22.ReqErrDoesNotExist {
		t.Fatalf("subscribe mix/video: got %v, want DOES_NOT_EXIST", err)
	}
}

func TestTransport_RejectsSecondMixSubscription(t *testing.T) {
	pr, _ := io.Pipe()
	endpoint, trCh := startTestServer(t, pr)
	client := dialTestClient(t, endpoint, acceptMixClient{})
	awaitTransport(t, trCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mix := v22.FullTrackName{Namespace: mixNamespace, Name: AudioTrack}
	if _, err := client.Subscribe(ctx, mix, nil); err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	var re *moqt.RequestError
	if _, err := client.Subscribe(ctx, mix, nil); !errors.As(err, &re) {
		t.Fatalf("second subscribe: got %v, want REQUEST_ERROR", err)
	}
}

func TestTransport_ClientCloseEndsTransport(t *testing.T) {
	pr, _ := io.Pipe()
	endpoint, trCh := startTestServer(t, pr)
	client := dialTestClient(t, endpoint, acceptMixClient{})
	tr := awaitTransport(t, trCh)

	_ = client.Close()
	select {
	case <-tr.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not finish after the client closed")
	}
	if err := tr.Err(); err != nil {
		t.Fatalf("Err after clean client close: %v", err)
	}
}

func TestTransport_ClientConnectionCloseIsHangup(t *testing.T) {
	tr, _, clientConn := subscribedMicTransport(t)
	_ = clientConn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "")
	awaitDone(t, tr)
	if err := tr.Err(); err != nil {
		t.Fatalf("Err after the client closed its QUIC connection: %v", err)
	}
}

func TestTransport_ClientErrorCloseIsReported(t *testing.T) {
	tr, client, _ := subscribedMicTransport(t)
	_ = client.CloseWithError(v22.ErrProtocolViolation, "test")
	awaitDone(t, tr)
	if tr.Err() == nil {
		t.Fatal("Err is nil after the client closed with PROTOCOL_VIOLATION")
	}
}

// subscribedMicTransport returns a server Transport whose mic subscription
// to the client is established and receiving.
func subscribedMicTransport(t *testing.T) (*Transport, *moqt.Session, *quic.Conn) {
	t.Helper()
	pr, _ := io.Pipe()
	endpoint, trCh := startTestServer(t, pr)
	enc, err := codec.NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := enc.Encode(tone(DefaultSampleRate * DefaultFrameMs / 1000))
	if err != nil {
		t.Fatal(err)
	}
	frames := make([][]byte, 500)
	for i := range frames {
		frames[i] = frame
	}
	client, conn := dialTestClientConn(t, endpoint, acceptMixClient{micFrames: frames})
	tr := awaitTransport(t, trCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.PublishNamespace(ctx, micTrack.Namespace, nil); err != nil {
		t.Fatalf("publish mic namespace: %v", err)
	}
	buf := make([]byte, DefaultSampleRate*DefaultFrameMs/1000*2)
	if _, err := io.ReadFull(tr.AudioReader(), buf); err != nil {
		t.Fatalf("read mic audio: %v", err)
	}
	return tr, client, conn
}

func awaitDone(t *testing.T, tr *Transport) {
	t.Helper()
	select {
	case <-tr.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not finish after the client went away")
	}
}

func TestIsCleanClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"moq no error", &v22.SessionError{Code: v22.ErrNoError}, true},
		{"moq protocol violation", &v22.SessionError{Code: v22.ErrProtocolViolation}, false},
		{"webtransport code 0", &webtransport.SessionError{Remote: true}, true},
		{"webtransport code 3", &webtransport.SessionError{Remote: true, ErrorCode: 3}, false},
		{"quic H3_NO_ERROR", &quic.ApplicationError{Remote: true, ErrorCode: quic.ApplicationErrorCode(http3.ErrCodeNoError)}, true},
		{"quic code 0", &quic.ApplicationError{Remote: true}, true},
		{"quic H3_INTERNAL_ERROR", &quic.ApplicationError{Remote: true, ErrorCode: quic.ApplicationErrorCode(http3.ErrCodeInternalError)}, false},
		{"wrapped no error", fmt.Errorf("read: %w", &v22.SessionError{Code: v22.ErrNoError}), true},
		{"other", errors.New("boom"), false},
	} {
		if got := isCleanClose(tc.err); got != tc.want {
			t.Errorf("%s: isCleanClose = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func tone(n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/DefaultSampleRate))
	}
	return s
}

func pcmBytes(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func rms(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "moqmedia-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
