package leg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// PCMediaConfig configures a pion PeerConnection + audio pipeline shared by
// WebRTC and WhatsApp legs. The peer connection is created by NewPCMedia; the
// caller drives SDP negotiation via PC().
type PCMediaConfig struct {
	Codec      codec.CodecType
	ICEServers []string
	RTPPortMin uint16
	RTPPortMax uint16
	Log        *slog.Logger

	// ExternalIPs are public addresses substituted into local host ICE
	// candidates via pion's SetNAT1To1IPs. Required when VB runs behind
	// NAT/Docker and the gathered host interface IPs aren't routable
	// from the remote peer.
	ExternalIPs []string

	// OnDisconnect fires when the media path is lost: with the ICE state
	// name ("failed", "disconnected"), or with PCPeerClosed / PCDTLSFailed.
	OnDisconnect func(reason string)
	// OnConnected fires once when the peer connection reaches the
	// Connected state. Subsequent state transitions don't re-fire.
	OnConnected func()

	// TolerateICEDisconnect keeps the peer connection alive through the ICE
	// disconnected state so the remote peer can restart ICE: OnInterrupted
	// fires instead of OnDisconnect, and OnRestored once connectivity is
	// back. ICE failed still fires OnDisconnect.
	TolerateICEDisconnect bool
	OnInterrupted         func()
	OnRestored            func()

	// Zero keeps pion's default (5s disconnected, then 25s more to failed).
	ICEDisconnectedTimeout time.Duration
	ICEFailedTimeout       time.Duration

	// AnsweringDTLSRole forces the DTLS role on actpass offers. Use
	// DTLSRoleClient against ice-lite peers (e.g. WhatsApp).
	AnsweringDTLSRole webrtc.DTLSRole

	// EnableTelephoneEvent advertises RFC 4733 DTMF (PT 126) in the
	// answer and routes inbound telephone-event packets to OnDTMF.
	EnableTelephoneEvent bool

	// LoopbackICE gathers host candidates on loopback only. Intended for
	// same-process tests: gathering on every interface of a developer or
	// CI host (docker bridges, VPN tunnels, VM adapters) yields hundreds
	// of candidate pairs and lets ICE nominate a virtual interface that
	// carries STUN but drops the DTLS handshake.
	LoopbackICE bool
}

// OnDisconnect reasons that are not ICE connection state names. Both reach
// the wire as cdr.reason on leg.disconnected.
const (
	PCPeerClosed = "peer_closed"
	PCDTLSFailed = "dtls_failed"
)

var (
	// ErrNotICERestart is returned by RestartICE when the offer keeps the
	// current ICE credentials.
	ErrNotICERestart = errors.New("offer does not change ICE credentials")
	// ErrInvalidOffer is returned by RestartICE when the offer cannot be
	// parsed or applied.
	ErrInvalidOffer = errors.New("invalid SDP offer")
)

const (
	defaultICEDisconnectedTimeout = 5 * time.Second
	defaultICEFailedTimeout       = 25 * time.Second
	defaultICEKeepaliveInterval   = 2 * time.Second
)

// PCMedia wraps a pion PeerConnection and exposes PCM16 io.Reader/io.Writer
// at the codec's native sample rate. Inbound RTP is decoded to PCM on a
// per-packet goroutine; outbound PCM is chunked into 20 ms frames, encoded,
// and written to the local RTP track.
type PCMedia struct {
	codec   codec.CodecType
	ptimeMs int
	frameSz int // PCM samples per 20ms frame

	pc         *webrtc.PeerConnection
	localTrack *webrtc.TrackLocalStaticRTP

	encoder codec.Encoder
	ssrc    uint32

	ctx    context.Context
	cancel context.CancelFunc

	inFrames  chan []byte
	outFrames chan []byte

	mu            sync.Mutex
	iceCandidates []webrtc.ICECandidateInit
	iceDone       bool

	negMu       sync.Mutex
	interrupted atomic.Bool

	tapMu       sync.RWMutex
	speakingTap io.Writer
	onDTMF      func(digit rune)
	lastDTMFTS  uint32

	started bool
	closing atomic.Bool
	log     *slog.Logger
}

func (m *PCMedia) SetSpeakingTap(w io.Writer) {
	m.tapMu.Lock()
	m.speakingTap = w
	m.tapMu.Unlock()
}

// ClearSpeakingTap removes the installed tap.
func (m *PCMedia) ClearSpeakingTap() {
	m.tapMu.Lock()
	m.speakingTap = nil
	m.tapMu.Unlock()
}

func (m *PCMedia) SetOnDTMF(fn func(digit rune)) {
	m.tapMu.Lock()
	m.onDTMF = fn
	m.tapMu.Unlock()
}

func NewPCMedia(cfg PCMediaConfig) (*PCMedia, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	rate := cfg.Codec.ClockRate()
	if rate == 0 {
		return nil, fmt.Errorf("codec %s has no clock rate", cfg.Codec)
	}

	enc, err := codec.NewEncoder(cfg.Codec)
	if err != nil {
		return nil, fmt.Errorf("new encoder: %w", err)
	}

	iceServers := make([]webrtc.ICEServer, 0, len(cfg.ICEServers))
	for _, url := range cfg.ICEServers {
		if url != "" {
			iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{url}})
		}
	}
	pcCfg := webrtc.Configuration{ICEServers: iceServers}

	se := webrtc.SettingEngine{}
	se.LoggerFactory = &pionLogFactory{log: cfg.Log}
	if cfg.RTPPortMin > 0 && cfg.RTPPortMax > 0 {
		se.SetEphemeralUDPPortRange(cfg.RTPPortMin, cfg.RTPPortMax)
	}
	if len(cfg.ExternalIPs) > 0 {
		se.SetNAT1To1IPs(cfg.ExternalIPs, webrtc.ICECandidateTypeHost)
	}
	if cfg.ICEDisconnectedTimeout > 0 || cfg.ICEFailedTimeout > 0 {
		disconnected, failed := cfg.ICEDisconnectedTimeout, cfg.ICEFailedTimeout
		if disconnected <= 0 {
			disconnected = defaultICEDisconnectedTimeout
		}
		if failed <= 0 {
			failed = defaultICEFailedTimeout
		}
		se.SetICETimeouts(disconnected, failed, defaultICEKeepaliveInterval)
	}
	if cfg.LoopbackICE {
		se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
		se.SetIncludeLoopbackCandidate(true)
	}
	if cfg.AnsweringDTLSRole != 0 {
		if err := se.SetAnsweringDTLSRole(cfg.AnsweringDTLSRole); err != nil {
			return nil, fmt.Errorf("set DTLS role: %w", err)
		}
	}

	// Custom MediaEngine when telephone-event is needed. Opus is
	// registered with an empty SDPFmtpLine so pion's fuzzy matcher treats
	// any remote Opus fmtp as exact — otherwise it drops Opus when
	// telephone-event is present alongside.
	var api *webrtc.API
	if cfg.EnableTelephoneEvent {
		me := &webrtc.MediaEngine{}
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeOpus,
				ClockRate:    48000,
				Channels:     2,
				RTCPFeedback: []webrtc.RTCPFeedback{{Type: "transport-cc"}},
			},
			PayloadType: 111,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, fmt.Errorf("register opus: %w", err)
		}
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:  "audio/telephone-event",
				ClockRate: 8000,
			},
			PayloadType: 126,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, fmt.Errorf("register telephone-event: %w", err)
		}
		ir := &interceptor.Registry{}
		if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
			return nil, fmt.Errorf("register default interceptors: %w", err)
		}
		api = webrtc.NewAPI(
			webrtc.WithSettingEngine(se),
			webrtc.WithMediaEngine(me),
			webrtc.WithInterceptorRegistry(ir),
		)
	} else {
		api = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	}
	pc, err := api.NewPeerConnection(pcCfg)
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	mime := mimeTypeFor(cfg.Codec)
	// Channels must match pion's MediaEngine entry (Opus=2, G.711=1) or
	// SetLocalDescription fails. The on-wire RTP is unaffected.
	channels := uint16(1)
	if cfg.Codec == codec.CodecOpus {
		channels = 2
	}
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: mime, ClockRate: uint32(rate), Channels: channels},
		"audio", "voiceblender",
	)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("new track: %w", err)
	}
	sender, err := pc.AddTrack(localTrack)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("add track: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ptime := 20
	frameSz := rate * ptime / 1000 // samples per frame

	m := &PCMedia{
		codec:      cfg.Codec,
		ptimeMs:    ptime,
		frameSz:    frameSz,
		pc:         pc,
		localTrack: localTrack,
		encoder:    enc,
		ssrc:       rand.Uint32(),
		ctx:        ctx,
		cancel:     cancel,
		inFrames:   make(chan []byte, 8),
		outFrames:  make(chan []byte, 8),
		log:        cfg.Log,
	}

	pc.OnTrack(m.handleTrack)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			m.mu.Lock()
			m.iceDone = true
			m.mu.Unlock()
			return
		}
		init := c.ToJSON()
		m.mu.Lock()
		m.iceCandidates = append(m.iceCandidates, init)
		m.mu.Unlock()
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		m.log.Debug("pcmedia: ICE connection state", "state", state.String())
		switch state {
		case webrtc.ICEConnectionStateDisconnected:
			if !cfg.TolerateICEDisconnect {
				if cfg.OnDisconnect != nil {
					cfg.OnDisconnect(state.String())
				}
				return
			}
			if !m.interrupted.Swap(true) && cfg.OnInterrupted != nil {
				cfg.OnInterrupted()
			}
		case webrtc.ICEConnectionStateFailed:
			if cfg.OnDisconnect != nil {
				cfg.OnDisconnect(state.String())
			}
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			if m.interrupted.Swap(false) && cfg.OnRestored != nil {
				cfg.OnRestored()
			}
		}
	})
	var connectedOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		m.log.Debug("pcmedia: peer connection state", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			if cfg.OnConnected != nil {
				connectedOnce.Do(cfg.OnConnected)
			}
		case webrtc.PeerConnectionStateClosed:
			// pion closes the peer connection itself on the peer's DTLS
			// close_notify, which skips the ICE failed/disconnected states.
			if cfg.OnDisconnect != nil && !m.closing.Load() {
				cfg.OnDisconnect(PCPeerClosed)
			}
		}
	})

	if dtls := sender.Transport(); dtls != nil {
		dtls.OnStateChange(func(state webrtc.DTLSTransportState) {
			m.log.Debug("pcmedia: DTLS state", "state", state.String())
			if state == webrtc.DTLSTransportStateFailed && cfg.OnDisconnect != nil && !m.closing.Load() {
				// pion holds the transport lock here; tearing down inline deadlocks.
				go cfg.OnDisconnect(PCDTLSFailed)
			}
		})
	}

	return m, nil
}

func (m *PCMedia) PC() *webrtc.PeerConnection { return m.pc }
func (m *PCMedia) Codec() codec.CodecType     { return m.codec }
func (m *PCMedia) SampleRate() int            { return m.codec.ClockRate() }

// Start begins the outbound write loop. Idempotent.
func (m *PCMedia) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()
	go m.writeLoop()
}

func (m *PCMedia) Close() error {
	m.closing.Store(true)
	m.cancel()
	return m.pc.Close()
}

func (m *PCMedia) Context() context.Context { return m.ctx }

func (m *PCMedia) AddICECandidate(c webrtc.ICECandidateInit) error {
	return m.pc.AddICECandidate(c)
}

// RestartICE applies a remote offer carrying new ICE credentials to the live
// peer connection and returns the answer. Local candidates of the new
// generation are trickled through DrainLocalCandidates again.
func (m *PCMedia) RestartICE(offerSDP string) (string, error) {
	m.negMu.Lock()
	defer m.negMu.Unlock()

	ufrag, pwd, err := sdpICECredentials(offerSDP)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidOffer, err)
	}
	cur := m.pc.RemoteDescription()
	if cur == nil {
		return "", errors.New("no remote description")
	}
	curUfrag, curPwd, err := sdpICECredentials(cur.SDP)
	if err != nil {
		return "", fmt.Errorf("current remote description: %w", err)
	}
	if ufrag == curUfrag && pwd == curPwd {
		return "", ErrNotICERestart
	}

	m.mu.Lock()
	prevCandidates, prevDone := m.iceCandidates, m.iceDone
	m.iceCandidates, m.iceDone = nil, false
	m.mu.Unlock()

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}
	if err := m.pc.SetRemoteDescription(offer); err != nil {
		m.mu.Lock()
		m.iceCandidates, m.iceDone = prevCandidates, prevDone
		m.mu.Unlock()
		return "", fmt.Errorf("%w: %v", ErrInvalidOffer, err)
	}
	answer, err := m.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("create answer: %w", err)
	}
	if err := m.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("set local description: %w", err)
	}
	return answer.SDP, nil
}

// sdpICECredentials returns the ice-ufrag/ice-pwd of an SDP, preferring the
// session level and falling back to the first media section that has them.
func sdpICECredentials(raw string) (ufrag, pwd string, err error) {
	var parsed sdp.SessionDescription
	if err := parsed.UnmarshalString(raw); err != nil {
		return "", "", err
	}
	ufrag, _ = parsed.Attribute("ice-ufrag")
	pwd, _ = parsed.Attribute("ice-pwd")
	for _, md := range parsed.MediaDescriptions {
		if ufrag == "" {
			ufrag, _ = md.Attribute("ice-ufrag")
		}
		if pwd == "" {
			pwd, _ = md.Attribute("ice-pwd")
		}
	}
	if ufrag == "" || pwd == "" {
		return "", "", errors.New("missing ICE credentials")
	}
	return ufrag, pwd, nil
}

// DrainLocalCandidates returns buffered local ICE candidates and the
// gathering-complete flag, clearing the buffer.
func (m *PCMedia) DrainLocalCandidates() ([]webrtc.ICECandidateInit, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.iceCandidates
	m.iceCandidates = nil
	return cs, m.iceDone
}

// AudioReader yields decoded PCM (16-bit LE) at the codec's native rate.
func (m *PCMedia) AudioReader() io.Reader {
	return &pcmReader{frames: m.inFrames, ctx: m.ctx}
}

// AudioWriter accepts PCM (16-bit LE) at the codec's native rate.
func (m *PCMedia) AudioWriter() io.Writer {
	return &pcmWriter{frames: m.outFrames, ctx: m.ctx}
}

func (m *PCMedia) handleTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	mime := track.Codec().MimeType
	// Fallback if pion splits telephone-event onto its own TrackRemote;
	// WhatsApp's single-SSRC offer keeps both PTs on one track today.
	if strings.EqualFold(mime, "audio/telephone-event") {
		m.handleDTMFTrack(track)
		return
	}

	dec, err := codec.NewDecoder(m.codec)
	if err != nil {
		m.log.Error("pcmedia: new decoder", "error", err, "codec", m.codec)
		return
	}
	// pion v4 mutates TrackRemote.PayloadType() per packet, so capture
	// the negotiated audio PT once to distinguish DTMF.
	audioPT := uint8(track.PayloadType())
	buf := make([]byte, 1500)
	for {
		if m.ctx.Err() != nil {
			return
		}
		n, _, err := track.Read(buf)
		if err != nil {
			return
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		if pkt.PayloadType != audioPT {
			if len(pkt.Payload) < 4 {
				continue
			}
			ev, derr := sipmod.DecodeDTMFEvent(pkt.Payload[:4])
			if derr != nil || pkt.Timestamp == m.lastDTMFTS {
				continue
			}
			m.lastDTMFTS = pkt.Timestamp
			digit, ok := sipmod.DTMFEventToDigit(ev.Event)
			if !ok {
				continue
			}
			m.tapMu.RLock()
			cb := m.onDTMF
			m.tapMu.RUnlock()
			if cb != nil {
				cb(digit)
			}
			continue
		}

		samples, err := dec.Decode(pkt.Payload)
		if err != nil || len(samples) == 0 {
			continue
		}
		pcm := int16ToBytes(samples)
		// Tap runs on every frame so VAD works without any AudioReader.
		m.tapMu.RLock()
		tap := m.speakingTap
		m.tapMu.RUnlock()
		if tap != nil {
			tap.Write(pcm)
		}
		select {
		case m.inFrames <- pcm:
		default:
			select {
			case <-m.inFrames:
			default:
			}
			m.inFrames <- pcm
		}
	}
}

// handleDTMFTrack handles the case where pion delivers telephone-event on
// a separate TrackRemote (rare; WhatsApp interleaves on the audio track).
func (m *PCMedia) handleDTMFTrack(track *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		if m.ctx.Err() != nil {
			return
		}
		n, _, err := track.Read(buf)
		if err != nil {
			return
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if len(pkt.Payload) < 4 {
			continue
		}
		ev, derr := sipmod.DecodeDTMFEvent(pkt.Payload[:4])
		if derr != nil || pkt.Timestamp == m.lastDTMFTS {
			continue
		}
		m.lastDTMFTS = pkt.Timestamp
		digit, ok := sipmod.DTMFEventToDigit(ev.Event)
		if !ok {
			continue
		}
		m.tapMu.RLock()
		cb := m.onDTMF
		m.tapMu.RUnlock()
		if cb != nil {
			cb(digit)
		}
	}
}

func (m *PCMedia) writeLoop() {
	var seq uint16
	var ts uint32
	var writeErrCount int
	silencePCM := make([]byte, m.frameSz*2)
	ticker := time.NewTicker(time.Duration(m.ptimeMs) * time.Millisecond)
	defer ticker.Stop()

	pending := make([]byte, 0, m.frameSz*2*2)
	frameBytes := m.frameSz * 2

	for {
		select {
		case <-m.ctx.Done():
			return
		case chunk := <-m.outFrames:
			pending = append(pending, chunk...)
			continue
		case <-ticker.C:
		}

		// Drain any further queued chunks without blocking.
		for {
			select {
			case chunk := <-m.outFrames:
				pending = append(pending, chunk...)
				continue
			default:
			}
			break
		}

		var frame []byte
		if len(pending) >= frameBytes {
			frame = pending[:frameBytes]
			pending = pending[frameBytes:]
		} else {
			frame = silencePCM
		}

		samples := bytesToInt16(frame)
		encoded, err := m.encoder.Encode(samples)
		if err != nil {
			m.log.Warn("pcmedia: encode", "error", err)
			continue
		}

		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    m.codec.PayloadType(),
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           m.ssrc,
			},
			Payload: encoded,
		}
		raw, err := pkt.Marshal()
		if err != nil {
			m.log.Warn("pcmedia: marshal RTP", "error", err)
			continue
		}
		if _, err := m.localTrack.Write(raw); err != nil {
			writeErrCount++
			if writeErrCount > 50 {
				return
			}
			continue
		}
		writeErrCount = 0
		seq++
		ts += uint32(m.frameSz)
	}
}

// pionLogAdapter forwards pion's LeveledLogger to slog. ICE scope is
// silenced below Info — its ping/keepalive trace is far too noisy.
type pionLogAdapter struct {
	log   *slog.Logger
	scope string
}

func (a *pionLogAdapter) quiet() bool { return a.scope == "ice" }

func (a *pionLogAdapter) Trace(msg string) {
	if a.quiet() {
		return
	}
	a.log.Debug("pion: "+a.scope, "msg", msg)
}
func (a *pionLogAdapter) Tracef(f string, args ...interface{}) {
	if a.quiet() {
		return
	}
	a.log.Debug("pion: "+a.scope, "msg", fmt.Sprintf(f, args...))
}
func (a *pionLogAdapter) Debug(msg string) {
	if a.quiet() {
		return
	}
	a.log.Debug("pion: "+a.scope, "msg", msg)
}
func (a *pionLogAdapter) Debugf(f string, args ...interface{}) {
	if a.quiet() {
		return
	}
	a.log.Debug("pion: "+a.scope, "msg", fmt.Sprintf(f, args...))
}
func (a *pionLogAdapter) Info(msg string) { a.log.Info("pion: "+a.scope, "msg", msg) }
func (a *pionLogAdapter) Infof(f string, args ...interface{}) {
	a.log.Info("pion: "+a.scope, "msg", fmt.Sprintf(f, args...))
}
func (a *pionLogAdapter) Warn(msg string) { a.log.Warn("pion: "+a.scope, "msg", msg) }
func (a *pionLogAdapter) Warnf(f string, args ...interface{}) {
	a.log.Warn("pion: "+a.scope, "msg", fmt.Sprintf(f, args...))
}
func (a *pionLogAdapter) Error(msg string) { a.log.Error("pion: "+a.scope, "msg", msg) }
func (a *pionLogAdapter) Errorf(f string, args ...interface{}) {
	a.log.Error("pion: "+a.scope, "msg", fmt.Sprintf(f, args...))
}

type pionLogFactory struct{ log *slog.Logger }

func (f *pionLogFactory) NewLogger(scope string) logging.LeveledLogger {
	return &pionLogAdapter{log: f.log, scope: scope}
}

func mimeTypeFor(c codec.CodecType) string {
	switch c {
	case codec.CodecOpus:
		return webrtc.MimeTypeOpus
	case codec.CodecPCMU:
		return webrtc.MimeTypePCMU
	case codec.CodecPCMA:
		return webrtc.MimeTypePCMA
	case codec.CodecG722:
		return webrtc.MimeTypeG722
	}
	return ""
}

type pcmReader struct {
	frames <-chan []byte
	ctx    context.Context
	buf    []byte
}

func (r *pcmReader) Read(p []byte) (int, error) {
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	select {
	case frame := <-r.frames:
		n := copy(p, frame)
		if n < len(frame) {
			r.buf = frame[n:]
		}
		return n, nil
	case <-r.ctx.Done():
		return 0, io.EOF
	}
}

type pcmWriter struct {
	frames chan<- []byte
	ctx    context.Context
}

func (w *pcmWriter) Write(p []byte) (int, error) {
	frame := make([]byte, len(p))
	copy(frame, p)
	select {
	case w.frames <- frame:
		return len(p), nil
	case <-w.ctx.Done():
		return 0, io.ErrClosedPipe
	}
}

func int16ToBytes(s []int16) []byte {
	out := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

func bytesToInt16(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}
