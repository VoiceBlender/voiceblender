package leg

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"runtime"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// PCMediaConfig configures a pion PeerConnection + audio pipeline shared by
// WebRTC and WhatsApp legs. The peer connection is created by NewPCMedia; the
// caller drives SDP negotiation via PC().
type PCMediaConfig struct {
	Codec      codec.CodecType
	ICEServers []string
	RTPPortMin uint16 // 0 = OS-assigned
	RTPPortMax uint16
	Log        *slog.Logger

	// OnDisconnect fires when ICE enters Failed or Disconnected. May be nil.
	OnDisconnect func(reason string)
}

// PCMedia wraps a pion PeerConnection and exposes PCM16 io.Reader/io.Writer
// at the codec's native sample rate. Inbound RTP is decoded to PCM on a
// per-packet goroutine; outbound PCM is chunked into 20 ms frames, encoded,
// and written to the local RTP track.
type PCMedia struct {
	codec   codec.CodecType
	ptimeMs int
	frameSz int // PCM samples per frame (e.g. 160 @ 8kHz/20ms, 960 @ 48kHz/20ms)

	pc         *webrtc.PeerConnection
	localTrack *webrtc.TrackLocalStaticRTP

	encoder codec.Encoder
	ssrc    uint32

	ctx    context.Context
	cancel context.CancelFunc

	inFrames  chan []byte // decoded PCM (byte-framed, 2 bytes per sample)
	outFrames chan []byte // outbound PCM chunks from mixer

	mu            sync.Mutex
	iceCandidates []webrtc.ICECandidateInit
	iceDone       bool

	started bool
	log     *slog.Logger
}

// NewPCMedia creates a PeerConnection configured for cfg.Codec, wires
// OnTrack/OnICECandidate/OnICEConnectionStateChange, and returns the media
// object. The caller is responsible for SDP negotiation via PC().
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

	var pc *webrtc.PeerConnection
	if cfg.RTPPortMin > 0 && cfg.RTPPortMax > 0 {
		se := webrtc.SettingEngine{}
		se.SetEphemeralUDPPortRange(cfg.RTPPortMin, cfg.RTPPortMax)
		api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
		pc, err = api.NewPeerConnection(pcCfg)
	} else {
		pc, err = webrtc.NewPeerConnection(pcCfg)
	}
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	mime := mimeTypeFor(cfg.Codec)
	// Channel count here is SDP metadata only — it must match pion's default
	// MediaEngine registration, otherwise SetLocalDescription fails with
	// "codec is not supported by remote". Pion registers Opus as /48000/2
	// and the G.711 family as /8000/1. The actual RTP payload is format-
	// agnostic (Opus carries its own stereo/mono flag), so sending a
	// mono-encoded stream under Channels=2 is fine.
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
			m.log.Debug("pcmedia: ICE gathering complete")
			m.mu.Lock()
			m.iceDone = true
			m.mu.Unlock()
			return
		}
		init := c.ToJSON()
		m.log.Debug("pcmedia: local ICE candidate", "candidate", init.Candidate)
		m.mu.Lock()
		m.iceCandidates = append(m.iceCandidates, init)
		m.mu.Unlock()
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		m.log.Info("pcmedia: ICE connection state", "state", state.String())
		if cfg.OnDisconnect != nil &&
			(state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected) {
			cfg.OnDisconnect(state.String())
		}
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		m.log.Info("pcmedia: ICE gathering state", "state", state.String())
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		m.log.Info("pcmedia: peer connection state", "state", state.String())
	})
	pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		m.log.Debug("pcmedia: signaling state", "state", state.String())
	})

	// DTLS state — catches DTLS handshake failures that don't cascade to
	// PeerConnectionStateFailed (pion sometimes holds the PC in Connecting
	// when the DTLS ClientHello times out).
	if dtls := sender.Transport(); dtls != nil {
		dtls.OnStateChange(func(state webrtc.DTLSTransportState) {
			m.log.Info("pcmedia: DTLS state", "state", state.String())
		})
		if ice := dtls.ICETransport(); ice != nil {
			ice.OnSelectedCandidatePairChange(func(pair *webrtc.ICECandidatePair) {
				if pair == nil {
					return
				}
				l := pair.Local
				r := pair.Remote
				m.log.Info("pcmedia: ICE pair selected",
					"local", fmt.Sprintf("%s:%d typ=%s", l.Address, l.Port, l.Typ.String()),
					"remote", fmt.Sprintf("%s:%d typ=%s", r.Address, r.Port, r.Typ.String()),
				)
			})
		}
	}

	return m, nil
}

// PC exposes the underlying peer connection for SDP negotiation.
func (m *PCMedia) PC() *webrtc.PeerConnection { return m.pc }

// Codec returns the negotiated audio codec.
func (m *PCMedia) Codec() codec.CodecType { return m.codec }

// SampleRate returns the codec's native sample rate.
func (m *PCMedia) SampleRate() int { return m.codec.ClockRate() }

// Start begins the outbound write loop. Safe to call once after
// SetLocalDescription; subsequent calls are no-ops.
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

// Close cancels the media context and closes the peer connection.
func (m *PCMedia) Close() error {
	// Log a short stack trace so we can identify which caller (Hangup,
	// cleanupLeg, an error path) triggered a premature close. This runs
	// once per leg; the allocation is irrelevant.
	buf := make([]byte, 2048)
	n := runtime.Stack(buf, false)
	m.log.Info("pcmedia: Close() called", "caller_stack", string(buf[:n]))
	m.cancel()
	return m.pc.Close()
}

// Context returns the media's lifecycle context; cancelled on Close.
func (m *PCMedia) Context() context.Context { return m.ctx }

// AddICECandidate applies a remote trickle ICE candidate.
func (m *PCMedia) AddICECandidate(c webrtc.ICECandidateInit) error {
	return m.pc.AddICECandidate(c)
}

// DrainLocalCandidates returns and clears buffered local ICE candidates along
// with a flag indicating whether gathering is complete.
func (m *PCMedia) DrainLocalCandidates() ([]webrtc.ICECandidateInit, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.iceCandidates
	m.iceCandidates = nil
	return cs, m.iceDone
}

// AudioReader yields decoded PCM (16-bit LE, codec native sample rate).
func (m *PCMedia) AudioReader() io.Reader {
	return &pcmReader{frames: m.inFrames, ctx: m.ctx}
}

// AudioWriter accepts PCM (16-bit LE, codec native sample rate). Chunks are
// re-framed internally into codec ptime-sized packets.
func (m *PCMedia) AudioWriter() io.Writer {
	return &pcmWriter{frames: m.outFrames, ctx: m.ctx}
}

func (m *PCMedia) handleTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	m.log.Info("pcmedia: remote track established",
		"ssrc", track.SSRC(),
		"payload_type", track.PayloadType(),
		"mime", track.Codec().MimeType,
		"clock_rate", track.Codec().ClockRate,
		"channels", track.Codec().Channels,
	)
	dec, err := codec.NewDecoder(m.codec)
	if err != nil {
		m.log.Error("pcmedia: new decoder", "error", err, "codec", m.codec)
		return
	}
	buf := make([]byte, 1500)
	var firstPacketLogged bool
	for {
		if m.ctx.Err() != nil {
			m.log.Info("pcmedia: handleTrack exiting (ctx done)")
			return
		}
		n, _, err := track.Read(buf)
		if err != nil {
			m.log.Info("pcmedia: handleTrack exiting (track read error)", "error", err)
			return
		}
		if !firstPacketLogged {
			firstPacketLogged = true
			m.log.Info("pcmedia: first inbound RTP packet received", "bytes", n)
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		samples, err := dec.Decode(pkt.Payload)
		if err != nil || len(samples) == 0 {
			continue
		}
		pcm := int16ToBytes(samples)
		select {
		case m.inFrames <- pcm:
		default:
			// Drop oldest to avoid blocking.
			select {
			case <-m.inFrames:
			default:
			}
			m.inFrames <- pcm
		}
	}
}

func (m *PCMedia) writeLoop() {
	var seq uint16
	var ts uint32
	var firstWriteLogged bool
	var writeErrCount int
	silencePCM := make([]byte, m.frameSz*2)
	ticker := time.NewTicker(time.Duration(m.ptimeMs) * time.Millisecond)
	defer ticker.Stop()

	pending := make([]byte, 0, m.frameSz*2*2)
	frameBytes := m.frameSz * 2

	m.log.Info("pcmedia: writeLoop started", "codec", m.codec, "ptime_ms", m.ptimeMs, "samples_per_frame", m.frameSz)

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
			if writeErrCount == 1 || writeErrCount%250 == 0 {
				m.log.Warn("pcmedia: localTrack.Write failed", "error", err, "count", writeErrCount, "seq", seq)
			}
			// pion returns io.ErrClosedPipe once the track is done;
			// stop if we're getting persistent errors.
			if writeErrCount > 50 {
				m.log.Error("pcmedia: writeLoop exiting after persistent write errors", "count", writeErrCount)
				return
			}
			continue
		}
		if !firstWriteLogged {
			firstWriteLogged = true
			m.log.Info("pcmedia: first RTP packet written to localTrack", "seq", seq, "payload_bytes", len(encoded))
		}
		seq++
		ts += uint32(m.frameSz)
	}
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
