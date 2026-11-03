package moqmedia

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/MoQ-it/moqt"
	"github.com/MoQ-it/moqt/transport/wt"
	v22 "github.com/MoQ-it/moqt/wire/v22"
	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// ALPN is the WebTransport application protocol the MoQ endpoint speaks.
const ALPN = v22.ALPN

// micObjectQueue bounds Opus payloads waiting for the decoder; at 20 ms
// per Object this is about one second of uplink audio.
const micObjectQueue = 50

var (
	micTrack     = v22.FullTrackName{Namespace: v22.Namespace{MicNamespace}, Name: AudioTrack}
	mixNamespace = v22.Namespace{MixNamespace}
)

// Transport carries one MoQ session for a single VoiceBlender leg. The
// server publishes the "mix" namespace (downlink: mixer -> client) and
// subscribes to the client's "mic" namespace (uplink: client -> mixer).
type Transport struct {
	moqt.RejectAll

	cfg Config

	wtSession *webtransport.Session
	session   *moqt.Session

	encoder *codec.OpusEncoder
	decoder *codec.OpusDecoder

	audioIn  *streamBuffer
	audioOut io.Reader

	pubCh      chan *moqt.TrackWriter
	subscribed atomic.Bool
	micOnce    sync.Once

	ctx    context.Context
	cancel context.CancelFunc

	startOnce sync.Once
	closeOnce sync.Once
	closed    atomic.Bool
	done      chan struct{}
	wg        sync.WaitGroup
	err       atomic.Value // error
}

// UpgradeServer accepts a WebTransport CONNECT, sets up a MoQ session
// (server perspective), publishes the mix namespace, and returns a
// Transport whose AudioReader yields decoded 48 kHz PCM16-LE from the
// client and whose Start consumes 48 kHz PCM16-LE for delivery to the
// client.
func UpgradeServer(w http.ResponseWriter, r *http.Request, wtServer *webtransport.Server, cfg Config) (*Transport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	wtSession, err := wt.Upgrade(wtServer, w, r)
	if err != nil {
		return nil, fmt.Errorf("webtransport upgrade: %w", err)
	}
	if p := wtSession.SessionState().ApplicationProtocol; p != "" && p != ALPN {
		_ = wtSession.CloseWithError(0, "unsupported protocol "+p)
		return nil, fmt.Errorf("webtransport: client negotiated %q, want %q", p, ALPN)
	}

	enc, err := codec.NewOpusEncoder()
	if err != nil {
		_ = wtSession.CloseWithError(0, "encoder init failed")
		return nil, err
	}
	if err := enc.SetBitrate(cfg.OpusBitrate); err != nil {
		cfg.Log.Warn("moqmedia: SetBitrate failed", "bps", cfg.OpusBitrate, "error", err)
	}

	dec, err := codec.NewOpusDecoder()
	if err != nil {
		_ = wtSession.CloseWithError(0, "decoder init failed")
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	t := &Transport{
		cfg:       cfg,
		wtSession: wtSession,
		encoder:   enc,
		decoder:   dec,
		audioIn:   newStreamBuffer(cfg.IngressBufferBytes(), cfg.FrameMs),
		pubCh:     make(chan *moqt.TrackWriter, 1),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}

	sess, err := moqt.NewSession(ctx, wt.New(wtSession), moqt.Server, moqt.Config{
		Handler: t,
		Logger:  cfg.Log.With("component", "moqt"),
	})
	if err != nil {
		cancel()
		_ = wtSession.CloseWithError(0, "moq setup failed")
		return nil, fmt.Errorf("moq setup: %w", err)
	}
	t.session = sess

	go func() {
		if _, err := sess.PublishNamespace(ctx, mixNamespace, nil); err != nil && !t.sessionEnded() {
			cfg.Log.Warn("moqmedia: publish mix namespace failed", "error", err)
		}
	}()

	return t, nil
}

// AudioReader returns the inbound PCM stream (decoded Opus from client).
func (t *Transport) AudioReader() io.Reader { return t.audioIn }

// Done is closed once all goroutines started by Start have exited.
func (t *Transport) Done() <-chan struct{} { return t.done }

// Err returns the terminal error, if any. Stable after Done is closed.
func (t *Transport) Err() error {
	if v := t.err.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// AudioDropsBytes reports cumulative inbound bytes dropped on overflow.
func (t *Transport) AudioDropsBytes() int64 { return t.audioIn.Dropped() }

// PeerAddr returns the WebTransport peer's remote address as a string,
// or empty if unavailable.
func (t *Transport) PeerAddr() string {
	if t.wtSession == nil {
		return ""
	}
	return t.wtSession.RemoteAddr().String()
}

// Start begins the send loop (reading PCM from audioOut, encoding Opus,
// publishing as MoQ Objects) and the session watcher. Idempotent.
func (t *Transport) Start(audioOut io.Reader) {
	t.startOnce.Do(func() {
		t.audioOut = audioOut
		t.wg.Add(2)
		go t.sendLoop()
		go t.sessionLoop()
		go func() {
			t.wg.Wait()
			close(t.done)
		}()
	})
}

// Close shuts the transport down: cancels the context, closes the
// inbound buffer, closes the MoQ session, and closes audioOut if it
// implements io.Closer so the send loop unblocks.
func (t *Transport) Close() error {
	var ret error
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.cancel()
		t.audioIn.Close()
		switch {
		case t.session != nil:
			ret = t.session.Close()
		case t.wtSession != nil:
			ret = t.wtSession.CloseWithError(0, "")
		}
		if c, ok := t.audioOut.(io.Closer); ok {
			_ = c.Close()
		}
	})
	return ret
}

// HandlePublishNamespace accepts the client's PUBLISH_NAMESPACE for `mic`,
// which is the cue to subscribe to its audio track.
func (t *Transport) HandlePublishNamespace(r *moqt.PublishNamespaceRequest) {
	if !namespaceEqual(r.Prefix, micTrack.Namespace) {
		_ = r.Reject(v22.ReqErrUninterested, "unexpected namespace")
		return
	}
	if _, err := r.Accept(); err != nil {
		t.cfg.Log.Warn("moqmedia: accept publish namespace failed", "error", err)
		return
	}
	t.micOnce.Do(t.subscribeMic)
}

// HandleSubscribe accepts the client's SUBSCRIBE for the mix track and
// hands the writer to the send loop. Only one subscription is served.
func (t *Transport) HandleSubscribe(r *moqt.SubscribeRequest) {
	if !namespaceEqual(r.Track.Namespace, mixNamespace) || r.Track.Name != AudioTrack {
		_ = r.Reject(v22.ReqErrDoesNotExist, "unknown track")
		return
	}
	if !t.subscribed.CompareAndSwap(false, true) {
		_ = r.Reject(v22.ReqErrInternal, "already subscribed")
		return
	}
	tw, err := r.Accept(moqt.AcceptOptions{})
	if err != nil {
		t.cfg.Log.Warn("moqmedia: accept subscribe failed", "error", err)
		return
	}
	t.pubCh <- tw
}

func (t *Transport) subscribeMic() {
	sub, err := t.session.Subscribe(t.ctx, micTrack, nil)
	if err != nil {
		if !t.sessionEnded() {
			t.cfg.Log.Warn("moqmedia: subscribe mic failed", "error", err)
		}
		return
	}
	t.wg.Add(1)
	go t.recvLoop(sub)
}

// recvLoop merges Objects from every subgroup stream and from datagrams,
// in arrival order, into a single decoder.
func (t *Transport) recvLoop(sub *moqt.Subscription) {
	defer t.wg.Done()
	defer sub.Unsubscribe()

	ctx, cancel := context.WithCancel(t.ctx)
	defer cancel()

	payloads := make(chan []byte, micObjectQueue)
	push := func(p []byte) {
		select {
		case payloads <- p:
		default:
		}
	}

	go func() {
		defer cancel()
		for {
			g, err := sub.AcceptSubgroup(ctx)
			if err != nil {
				t.logRecvErr(err)
				return
			}
			go func() {
				for {
					o, err := g.Next()
					if err != nil {
						return
					}
					if o.Status == v22.StatusNormal && len(o.Payload) > 0 {
						push(o.Payload)
					}
				}
			}()
		}
	}()
	go func() {
		for {
			d, err := sub.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if d.Status == v22.StatusNormal && len(d.Payload) > 0 {
				push(d.Payload)
			}
		}
	}()

	for {
		var p []byte
		select {
		case p = <-payloads:
		case <-ctx.Done():
			return
		}
		samples, err := t.decoder.Decode(p)
		if err != nil {
			t.cfg.Log.Warn("moqmedia: opus decode failed", "error", err)
			continue
		}
		buf := make([]byte, len(samples)*2)
		for i, s := range samples {
			binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
		}
		_, _ = t.audioIn.Write(buf)
	}
}

func (t *Transport) logRecvErr(err error) {
	if errors.Is(err, io.EOF) || errors.Is(err, moqt.ErrUnsubscribed) || isCleanClose(err) || t.sessionEnded() {
		return
	}
	t.cfg.Log.Warn("moqmedia: mic subscription failed", "error", err)
}

// closeError returns the error code the session was closed with, by moqt
// or by the client; nil for our own Close, a clean hangup or a dropped
// connection.
func (t *Transport) closeError() error {
	if t.closed.Load() {
		return nil
	}
	var moqErr *v22.SessionError
	if t.session != nil && errors.As(t.session.Err(), &moqErr) {
		if moqErr.Code == v22.ErrNoError {
			return nil
		}
		return moqErr
	}
	// webtransport-go exposes the session's close error only through stream operations.
	str, err := t.wtSession.OpenUniStream()
	if err == nil {
		str.CancelWrite(0)
		return nil
	}
	var wtErr *webtransport.SessionError
	if errors.As(err, &wtErr) && wtErr.ErrorCode != 0 {
		return wtErr
	}
	return nil
}

// isCleanClose reports whether err is the peer ending the MoQ session,
// WebTransport session or QUIC connection without an error code.
func isCleanClose(err error) bool {
	if err == nil {
		return false
	}
	var moqErr *v22.SessionError
	if errors.As(err, &moqErr) {
		return moqErr.Code == v22.ErrNoError
	}
	var wtErr *webtransport.SessionError
	if errors.As(err, &wtErr) {
		return wtErr.ErrorCode == 0
	}
	var quicErr *quic.ApplicationError
	if errors.As(err, &quicErr) {
		return quicErr.ErrorCode == 0 || quicErr.ErrorCode == quic.ApplicationErrorCode(http3.ErrCodeNoError)
	}
	return false
}

// sessionEnded reports whether the leg or the MoQ session is shutting
// down, in which case request and stream errors are expected.
func (t *Transport) sessionEnded() bool {
	if t.ctx.Err() != nil || t.closed.Load() {
		return true
	}
	return t.session != nil && t.session.Err() != nil
}

func (t *Transport) sendLoop() {
	defer t.wg.Done()

	var tw *moqt.TrackWriter
	select {
	case tw = <-t.pubCh:
	case <-t.ctx.Done():
		return
	}

	frameBytes := t.cfg.FrameBytesPCM()
	frameSamples := t.cfg.FrameSamples()
	pcmBuf := make([]byte, frameBytes)
	samples := make([]int16, frameSamples)
	var groupID uint64

	for {
		if t.ctx.Err() != nil {
			return
		}
		if _, err := io.ReadFull(t.audioOut, pcmBuf); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.cfg.Log.Warn("moqmedia: read mixer audio failed", "error", err)
			}
			return
		}
		if tw.Context().Err() != nil {
			return
		}
		if !tw.Forwarding() {
			continue
		}
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(pcmBuf[i*2:]))
		}
		encoded, err := t.encoder.Encode(samples)
		if err != nil {
			t.cfg.Log.Warn("moqmedia: opus encode failed", "error", err)
			continue
		}
		// One Opus frame per Group, so a late or lost frame never holds up the next.
		sg := tw.OpenSubgroup(groupID, 0, moqt.SubgroupOptions{EndOfGroup: true, FirstObject: true})
		werr := sg.WriteObject(&v22.Object{ID: 0, Payload: encoded})
		_ = sg.Close()
		if werr != nil {
			if !t.sessionEnded() && tw.Context().Err() == nil {
				t.cfg.Log.Warn("moqmedia: WriteObject failed", "error", werr)
			}
			return
		}
		groupID++
	}
}

func (t *Transport) sessionLoop() {
	defer t.wg.Done()
	if t.wtSession == nil {
		return
	}
	<-t.wtSession.Context().Done()
	if err := t.closeError(); err != nil {
		t.err.Store(err)
	}
	_ = t.Close()
}

func namespaceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, s := range a {
		if s != b[i] {
			return false
		}
	}
	return true
}
