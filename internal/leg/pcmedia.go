package leg

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
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

	// AnsweringDTLSRole forces the DTLS role when answering a remote offer
	// whose a=setup is "actpass". Defaults to the pion default (Server /
	// passive), which works for browser peers. Set to DTLSRoleClient
	// (a=setup:active) when answering an ice-lite peer such as WhatsApp
	// Business Calling — otherwise both sides wait for a ClientHello that
	// never arrives and DTLS stalls.
	AnsweringDTLSRole webrtc.DTLSRole

	// EnableTelephoneEvent registers audio/telephone-event (PT 126,
	// clock 8000, events 0-16) in the MediaEngine so the SDP answer
	// advertises RFC 4733 DTMF. Inbound telephone-event packets are
	// decoded in handleTrack and forwarded via the OnDTMF callback.
	EnableTelephoneEvent bool
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

	// Taps receive a copy of decoded inbound PCM (16-bit LE, codec native
	// rate). Guarded by tapMu to allow concurrent set/clear from the API
	// layer while handleTrack is running.
	tapMu       sync.RWMutex
	speakingTap io.Writer

	// DTMF callback invoked on end-of-event for inbound telephone-event
	// packets. Guarded by tapMu for the same reason.
	onDTMF     func(digit rune)
	lastDTMFTS uint32

	started bool
	log     *slog.Logger
}

// SetSpeakingTap installs a writer that receives decoded inbound PCM on
// every packet. Used by the speaking detector. Pass nil via
// ClearSpeakingTap to remove.
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

// SetOnDTMF installs a callback invoked once per inbound DTMF digit
// (end-of-event, deduplicated against RFC 4733 retransmits). Only
// effective when PCMediaConfig.EnableTelephoneEvent was set.
func (m *PCMedia) SetOnDTMF(fn func(digit rune)) {
	m.tapMu.Lock()
	m.onDTMF = fn
	m.tapMu.Unlock()
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

	// Always build a custom SettingEngine so pion's internal transport
	// traces (ICE, DTLS, SRTP decrypt errors) flow into our slog handler.
	se := webrtc.SettingEngine{}
	se.LoggerFactory = &pionLogFactory{log: cfg.Log}
	if cfg.RTPPortMin > 0 && cfg.RTPPortMax > 0 {
		se.SetEphemeralUDPPortRange(cfg.RTPPortMin, cfg.RTPPortMax)
	}
	if cfg.AnsweringDTLSRole != 0 {
		if err := se.SetAnsweringDTLSRole(cfg.AnsweringDTLSRole); err != nil {
			return nil, fmt.Errorf("set DTLS role: %w", err)
		}
	}

	// Build the API. When telephone-event support is requested we need a
	// custom MediaEngine — pion's registry is frozen once NewPeerConnection
	// runs, so RFC 4733 can't be added after the fact. Crucially we do NOT
	// call RegisterDefaultCodecs here: its Opus entry carries
	// SDPFmtpLine="minptime=10;useinbandfec=1", which conflicts with
	// Meta's "minptime=20;...". pion's matcher classifies that as a
	// partial-only match for Opus while telephone-event matches exactly
	// (empty-params fuzzy), and then mediaengine.updateFromRemoteDescription
	// drops the partial set — so the generated answer ends up with PT 126
	// only and our Opus track fails to Bind. Registering Opus with an
	// empty SDPFmtpLine sidesteps the fmtp comparison so Opus is an exact
	// match too; pion echoes the remote's fmtp in the answer regardless.
	var api *webrtc.API
	if cfg.EnableTelephoneEvent {
		me := &webrtc.MediaEngine{}
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeOpus,
				ClockRate:   48000,
				Channels:    2,
				SDPFmtpLine: "", // empty → match any remote Opus fmtp
				RTCPFeedback: []webrtc.RTCPFeedback{
					{Type: "transport-cc"},
				},
			},
			PayloadType: 111,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, fmt.Errorf("register opus: %w", err)
		}
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    "audio/telephone-event",
				ClockRate:   8000,
				Channels:    0,
				SDPFmtpLine: "",
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
	mime := track.Codec().MimeType
	m.log.Info("pcmedia: remote track established",
		"ssrc", track.SSRC(),
		"payload_type", track.PayloadType(),
		"mime", mime,
		"clock_rate", track.Codec().ClockRate,
		"channels", track.Codec().Channels,
	)

	// Pion sometimes fires OnTrack separately for telephone-event even
	// though in practice with WhatsApp's single-SSRC offer it doesn't.
	// Keep the dedicated handler as a defensive fallback for the case
	// where a future pion version (or another ice-lite peer) splits them.
	if strings.EqualFold(mime, "audio/telephone-event") {
		m.handleDTMFTrack(track)
		return
	}

	dec, err := codec.NewDecoder(m.codec)
	if err != nil {
		m.log.Error("pcmedia: new decoder", "error", err, "codec", m.codec)
		return
	}
	// CAPTURE ONCE: pion v4's TrackRemote.PayloadType() mutates on every
	// incoming packet (checkAndUpdateTrack). Reading it inside the loop
	// makes audioPT track the last-seen PT, so PT 126 DTMF packets would
	// be classified as "audio" and fed to the Opus decoder. Pin the
	// negotiated audio PT at track-establishment time so DTMF packets
	// can be correctly routed to the RFC 4733 parser.
	audioPT := uint8(track.PayloadType())
	buf := make([]byte, 1500)
	var (
		firstPacketLogged bool
		pktCount          uint64
		pktBytes          uint64
		droppedFull       uint64
		lastReport        = time.Now()
		// Debug: log first N packets per unique PT, and the running PT
		// distribution. Decisive for DTMF debugging — we can prove
		// whether PT 126 packets arrive at all, and see their payload
		// bytes to verify RFC 4733 parsing.
		ptFirstLogs = map[uint8]int{}
		ptCounts    = map[uint8]uint64{}
	)
	const ptLogFirstN = 3
	for {
		if m.ctx.Err() != nil {
			m.log.Info("pcmedia: handleTrack exiting (ctx done)", "total_pkts", pktCount, "total_bytes", pktBytes)
			return
		}
		n, _, err := track.Read(buf)
		if err != nil {
			m.log.Info("pcmedia: handleTrack exiting (track read error)", "error", err, "total_pkts", pktCount, "total_bytes", pktBytes)
			return
		}
		pktCount++
		pktBytes += uint64(n)
		if !firstPacketLogged {
			firstPacketLogged = true
			m.log.Info("pcmedia: first inbound RTP packet received", "bytes", n)
		}
		// Heartbeat: every 2 s summarise inbound traffic + drops. Silent
		// when no packets arrived — the gap itself is the signal.
		if now := time.Now(); now.Sub(lastReport) >= 2*time.Second {
			m.log.Info("pcmedia: inbound RTP stats (last 2s)",
				"pkts", pktCount, "bytes", pktBytes, "dropped_channel_full", droppedFull)
			lastReport = now
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		// Per-PT packet logging for debugging. Emits a hex dump of the
		// first N packets seen for each unique PT, and the running
		// distribution every 500 packets. Tight bound so it can't flood.
		ptCounts[pkt.PayloadType]++
		if ptFirstLogs[pkt.PayloadType] < ptLogFirstN {
			ptFirstLogs[pkt.PayloadType]++
			head := pkt.Payload
			if len(head) > 32 {
				head = head[:32]
			}
			m.log.Info("pcmedia: RTP packet (sample)",
				"pt", pkt.PayloadType,
				"seq", pkt.SequenceNumber,
				"ts", pkt.Timestamp,
				"payload_len", len(pkt.Payload),
				"payload_hex", fmt.Sprintf("%x", head),
			)
		}
		if pktCount%500 == 0 && pktCount > 0 {
			m.log.Info("pcmedia: PT distribution", "counts", fmt.Sprintf("%v", ptCounts))
		}

		// Interleaved DTMF: telephone-event on the same TrackRemote as
		// Opus. RFC 4733 payload is 4 bytes; if it's longer, the first 4
		// bytes are still the primary event (RFC 2198 redundancy appends
		// past events). We dedupe on RTP timestamp — every packet of a
		// single digit shares the same timestamp, so the first one wins
		// and retransmits with the same ts are ignored.
		if pkt.PayloadType != audioPT {
			if len(pkt.Payload) >= 4 {
				ev, derr := sipmod.DecodeDTMFEvent(pkt.Payload[:4])
				if derr == nil {
					digit, ok := sipmod.DTMFEventToDigit(ev.Event)
					newEvent := pkt.Timestamp != m.lastDTMFTS
					// Always log the parsed packet so we can see what
					// Meta sends and verify dedup behaviour.
					m.log.Info("pcmedia: DTMF packet",
						"pt", pkt.PayloadType,
						"event", ev.Event,
						"digit_ok", ok,
						"digit", string(digit),
						"end_of_event", ev.EndOfEvent,
						"volume", ev.Volume,
						"duration", ev.Duration,
						"rtp_ts", pkt.Timestamp,
						"new_event", newEvent,
						"payload_len", len(pkt.Payload),
					)
					if newEvent && ok {
						m.lastDTMFTS = pkt.Timestamp
						m.tapMu.RLock()
						cb := m.onDTMF
						m.tapMu.RUnlock()
						if cb != nil {
							cb(digit)
						}
					}
				} else {
					m.log.Info("pcmedia: non-audio RTP (decode failed)",
						"pt", pkt.PayloadType,
						"error", derr,
						"payload_len", len(pkt.Payload),
						"payload_hex", fmt.Sprintf("%x", pkt.Payload),
					)
				}
			} else {
				m.log.Info("pcmedia: non-audio RTP (too short)",
					"pt", pkt.PayloadType,
					"payload_len", len(pkt.Payload),
					"payload_hex", fmt.Sprintf("%x", pkt.Payload),
				)
			}
			continue
		}

		samples, err := dec.Decode(pkt.Payload)
		if err != nil || len(samples) == 0 {
			continue
		}
		pcm := int16ToBytes(samples)
		// Write to the speaking tap before the channel push so VAD runs
		// whether or not an AudioReader consumer exists.
		m.tapMu.RLock()
		tap := m.speakingTap
		m.tapMu.RUnlock()
		if tap != nil {
			tap.Write(pcm)
		}
		select {
		case m.inFrames <- pcm:
		default:
			droppedFull++
			// Drop oldest to avoid blocking. Happens when nothing reads the
			// leg's AudioReader (no room join / no tap); Meta's audio is
			// being discarded into the void.
			select {
			case <-m.inFrames:
			default:
			}
			m.inFrames <- pcm
		}
	}
}

// handleDTMFTrack reads a dedicated telephone-event TrackRemote and fires
// the onDTMF callback once per digit (end-of-event, deduplicated against
// RFC 4733 retransmits). Separate from the Opus path because pion v4
// delivers each negotiated PT on its own TrackRemote.
func (m *PCMedia) handleDTMFTrack(track *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	var digitCount uint64
	for {
		if m.ctx.Err() != nil {
			m.log.Info("pcmedia: DTMF track exiting (ctx done)", "digits", digitCount)
			return
		}
		n, _, err := track.Read(buf)
		if err != nil {
			m.log.Info("pcmedia: DTMF track exiting (read error)", "error", err, "digits", digitCount)
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
		if derr != nil {
			continue
		}
		// Every packet of one DTMF event shares the same RTP timestamp;
		// fire on the first with a new ts. Works whether the sender
		// transmits an end-of-event marker or not (Meta does not).
		if pkt.Timestamp == m.lastDTMFTS {
			continue
		}
		m.lastDTMFTS = pkt.Timestamp
		digit, ok := sipmod.DTMFEventToDigit(ev.Event)
		if !ok {
			continue
		}
		digitCount++
		m.log.Info("pcmedia: DTMF digit received", "digit", string(digit), "ssrc", track.SSRC())
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

// pionLogAdapter bridges pion's LeveledLogger to slog so we see pion's
// internal transport-layer traces (DTLS, SRTP decrypt errors).
// pion's ICE scope spams ping/keepalive/response traces at Debug/Trace;
// we drop those entirely to keep the log readable. Warn and above still
// pass through so failures are visible.
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
