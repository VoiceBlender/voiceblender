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

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/mengelbart/moqtransport"
	"github.com/mengelbart/moqtransport/webtransportmoq"
	"github.com/quic-go/webtransport-go"
)

// Transport carries one MoQ session for a single VoiceBlender leg. The
// server publishes the "mix" namespace (downlink: mixer -> client) and
// subscribes to the client's "mic" namespace (uplink: client -> mixer).
type Transport struct {
	cfg Config

	wtSession *webtransport.Session
	session   *moqtransport.Session

	encoder *codec.OpusEncoder
	decoder *codec.OpusDecoder

	audioIn  *streamBuffer
	audioOut io.Reader

	pubCh chan moqtransport.Publisher

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
// (server perspective), announces the mix namespace, and returns a
// Transport whose AudioReader yields decoded 48 kHz PCM16-LE from the
// client and whose Start consumes 48 kHz PCM16-LE for delivery to the
// client.
func UpgradeServer(w http.ResponseWriter, r *http.Request, wtServer *webtransport.Server, cfg Config) (*Transport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	wtSession, err := wtServer.Upgrade(w, r)
	if err != nil {
		return nil, fmt.Errorf("webtransport upgrade: %w", err)
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
		pubCh:     make(chan moqtransport.Publisher, 1),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}

	conn := webtransportmoq.NewServer(wtSession)
	sess := &moqtransport.Session{
		Handler:             moqtransport.HandlerFunc(t.handleControl),
		SubscribeHandler:    moqtransport.SubscribeHandlerFunc(t.handleSubscribe),
		InitialMaxRequestID: 100,
	}
	if err := sess.Run(conn); err != nil {
		cancel()
		_ = wtSession.CloseWithError(0, "moq session.Run failed")
		return nil, fmt.Errorf("moq session.Run: %w", err)
	}
	t.session = sess

	if err := sess.Announce(ctx, []string{MixNamespace}); err != nil {
		cfg.Log.Warn("moqmedia: announce mix failed", "error", err)
	}

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
// inbound buffer, closes the WebTransport session, and closes audioOut
// if it implements io.Closer so the send loop unblocks.
func (t *Transport) Close() error {
	var ret error
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.cancel()
		t.audioIn.Close()
		if t.wtSession != nil {
			ret = t.wtSession.CloseWithError(0, "")
		}
		if c, ok := t.audioOut.(io.Closer); ok {
			_ = c.Close()
		}
	})
	return ret
}

// handleControl handles ANNOUNCE (and other control) messages from the
// client. We only care about an ANNOUNCE for the `mic` namespace; that
// is our cue to SUBSCRIBE so the recv loop can start reading.
func (t *Transport) handleControl(w moqtransport.ResponseWriter, m *moqtransport.Message) {
	if m.Method != moqtransport.MessageAnnounce {
		return
	}
	if !namespaceEqual(m.Namespace, []string{MicNamespace}) {
		_ = w.Reject(0, "unexpected namespace")
		return
	}
	if err := w.Accept(); err != nil {
		t.cfg.Log.Warn("moqmedia: accept announce failed", "error", err)
		return
	}
	go t.subscribeMic()
}

// handleSubscribe handles a client SUBSCRIBE for our mix track. We
// accept and hand the resulting publisher to the send loop via pubCh.
func (t *Transport) handleSubscribe(w *moqtransport.SubscribeResponseWriter, m *moqtransport.SubscribeMessage) {
	if !namespaceEqual(m.Namespace, []string{MixNamespace}) || m.Track != AudioTrack {
		_ = w.Reject(moqtransport.ErrorCodeSubscribeTrackDoesNotExist, "unknown track")
		return
	}
	if err := w.Accept(); err != nil {
		t.cfg.Log.Warn("moqmedia: accept subscribe failed", "error", err)
		return
	}
	select {
	case t.pubCh <- w:
	default:
		t.cfg.Log.Warn("moqmedia: duplicate subscribe to mix track")
	}
}

// subscribeMic subscribes to the client's mic track and spawns a recv
// loop that decodes incoming Opus objects into the inbound PCM buffer.
func (t *Transport) subscribeMic() {
	rt, err := t.session.Subscribe(t.ctx, []string{MicNamespace}, AudioTrack)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			t.cfg.Log.Warn("moqmedia: subscribe mic failed", "error", err)
		}
		return
	}
	t.wg.Add(1)
	go t.recvLoop(rt)
}

func (t *Transport) recvLoop(rt *moqtransport.RemoteTrack) {
	defer t.wg.Done()
	defer rt.Close()
	for {
		if t.ctx.Err() != nil {
			return
		}
		obj, err := rt.ReadObject(t.ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return
			}
			t.err.Store(err)
			t.cfg.Log.Warn("moqmedia: ReadObject failed", "error", err)
			return
		}
		samples, err := t.decoder.Decode(obj.Payload)
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

func (t *Transport) sendLoop() {
	defer t.wg.Done()

	var pub moqtransport.Publisher
	select {
	case pub = <-t.pubCh:
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
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(pcmBuf[i*2:]))
		}
		encoded, err := t.encoder.Encode(samples)
		if err != nil {
			t.cfg.Log.Warn("moqmedia: opus encode failed", "error", err)
			continue
		}
		sg, err := pub.OpenSubgroup(groupID, 0, 0)
		if err != nil {
			t.cfg.Log.Warn("moqmedia: OpenSubgroup failed", "error", err)
			return
		}
		_, werr := sg.WriteObject(0, encoded)
		_ = sg.Close()
		if werr != nil {
			t.cfg.Log.Warn("moqmedia: WriteObject failed", "error", werr)
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
