package leg

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"errors"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/pion/rtp"
)

const rtpTimeout = 30 * time.Second

// SIPLeg wraps a SIP dialog (inbound or outbound) with RTP media handling.
type SIPLeg struct {
	id      string
	legType LegType
	state   LegState
	mu      sync.RWMutex

	// Exactly one of these is set.
	inbound  *sipmod.InboundCall
	outbound *sipmod.OutboundCall

	ctx           context.Context
	cancel        context.CancelFunc
	roomID        string
	muted         atomic.Bool
	deaf          atomic.Bool
	createdAt     time.Time
	answeredAt    time.Time     // zero if never answered
	answerCh      chan struct{} // signaled by REST answer endpoint (inbound only)
	connectedCh   chan struct{} // closed when leg reaches connected state
	connectedOnce sync.Once     // ensures connectedCh is closed exactly once
	onDTMF        func(digit rune)
	lastDTMFTS    uint32 // timestamp of last fired end-of-event (dedup RFC 4733 retransmits)
	onRTPTimeout  func() // called when no RTP received within timeout
	onHold        func() // called when leg is put on hold
	onUnhold      func() // called when leg is taken off hold

	callID    string      // SIP Call-ID for re-INVITE matching
	held      bool        // true when call is on hold
	holdTimer *time.Timer // 2-hour auto-hangup timer

	// Session timer (RFC 4028)
	sessionInterval  uint32      // negotiated interval in seconds (0 = no session timer)
	sessionRefresher string      // "uac" or "uas"
	sessionTimer     *time.Timer // refresh or expiry timer
	onSessionExpired func()      // called when session expires without refresh

	// Media
	rtpSess   *sipmod.RTPSession
	codecType codec.CodecType
	rtpPT     uint8 // negotiated RTP payload type (may differ from codec default for dynamic PTs)
	encoder   codec.Encoder
	decoder   codec.Decoder
	inFrames  chan []byte // decoded native-rate PCM from readLoop
	outFrames chan []byte // native-rate PCM to encode in writeLoop
	dtmfCh    chan string // DTMF digits to send in writeLoop

	earlyMediaSDP []byte            // SDP sent in 183, reused in 200 OK on Answer
	sipHeaders    map[string]string // X-* headers from inbound INVITE or outbound request

	engine          *sipmod.Engine    // for sending re-INVITEs
	localIP         string            // for SDP answer generation
	supportedCodecs []codec.CodecType // from engine config

	// Optional taps for recording on standalone legs.
	inTap  io.Writer // copy of decoded incoming PCM (before inFrames)
	outTap io.Writer // copy of outgoing PCM (from writeLoop, including silence)

	// AMD tap — separate from inTap so recording and AMD can coexist.
	amdTap io.Writer

	// Speaking detection tap — receives decoded incoming PCM for voice
	// activity detection. Separate from other taps so all can coexist.
	speakingTap io.Writer

	// Inbound RTP stream statistics for MOS calculation (protected by rtpStatsMu).
	rtpStatsMu     sync.Mutex
	rtpReceived    uint32
	rtpFirstSeq    uint16
	rtpLastSeq     uint16
	rtpHasFirst    bool
	rtpJitter      float64 // running jitter in RTP clock units (RFC 3550 §A.8)
	rtpLastTransit int64   // last transit time in RTP clock units

	log *slog.Logger
}

func NewSIPInboundLeg(call *sipmod.InboundCall, engine *sipmod.Engine, log *slog.Logger) *SIPLeg {
	ctx, cancel := context.WithCancel(call.Dialog.Context())

	// Extract X-* headers from the inbound INVITE request.
	var hdrs map[string]string
	if call.Request != nil {
		for _, h := range call.Request.Headers() {
			name := h.Name()
			if strings.HasPrefix(name, "X-") {
				if hdrs == nil {
					hdrs = make(map[string]string)
				}
				hdrs[name] = h.Value()
			}
		}
	}

	// Extract Call-ID for re-INVITE matching.
	var callID string
	if cid := call.Request.CallID(); cid != nil {
		callID = cid.Value()
	}

	l := &SIPLeg{
		id:              uuid.New().String(),
		legType:         TypeSIPInbound,
		state:           StateRinging,
		createdAt:       time.Now(),
		inbound:         call,
		sipHeaders:      hdrs,
		ctx:             ctx,
		cancel:          cancel,
		answerCh:        make(chan struct{}),
		connectedCh:     make(chan struct{}),
		callID:          callID,
		engine:          engine,
		localIP:         engine.BindIP(),
		supportedCodecs: engine.Codecs(),
		log:             log,
	}

	// Copy session timer params from inbound call.
	if call.SessionTimer != nil {
		l.sessionInterval = call.SessionTimer.Interval
		l.sessionRefresher = call.SessionTimer.Refresher
	}

	return l
}

func NewSIPOutboundLeg(call *sipmod.OutboundCall, engine *sipmod.Engine, log *slog.Logger) *SIPLeg {
	now := time.Now()
	ctx, cancel := context.WithCancel(call.Dialog.Context())
	l := &SIPLeg{
		id:         uuid.New().String(),
		legType:    TypeSIPOutbound,
		state:      StateConnected,
		createdAt:  now,
		answeredAt: now,
		outbound:   call,
		rtpSess:    call.RTPSess,
		ctx:        ctx,
		cancel:     cancel,
		log:        log,
	}

	// Negotiate codec from the remote answer SDP
	negotiated, _, ok := sipmod.NegotiateCodec(call.RemoteSDP, engine.Codecs())
	if !ok {
		log.Error("no common codec with remote for outbound leg")
		return l
	}
	l.codecType = negotiated
	// As the offerer we send with OUR payload type (from the offer SDP),
	// not the answerer's PT which may differ for dynamic codecs.
	l.rtpPT = negotiated.PayloadType()
	l.setupMedia()
	return l
}

// NewSIPOutboundPendingLeg creates an outbound leg in ringing state with its
// own context. Call ConnectOutbound after the INVITE succeeds.
// If codecs is non-empty it overrides the engine's default codec list.
func NewSIPOutboundPendingLeg(engine *sipmod.Engine, codecs []codec.CodecType, log *slog.Logger) *SIPLeg {
	supported := engine.Codecs()
	if len(codecs) > 0 {
		supported = codecs
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SIPLeg{
		id:              uuid.New().String(),
		legType:         TypeSIPOutbound,
		state:           StateRinging,
		createdAt:       time.Now(),
		ctx:             ctx,
		cancel:          cancel,
		engine:          engine,
		localIP:         engine.BindIP(),
		supportedCodecs: supported,
		log:             log,
	}
}

// SetupEarlyMediaOutbound configures the media pipeline from a 183 response's
// SDP before the call is answered. Only valid in StateRinging.
func (l *SIPLeg) SetupEarlyMediaOutbound(remoteSDP *sipmod.SDPMedia, rtpSess *sipmod.RTPSession) error {
	l.mu.Lock()
	if l.state != StateRinging {
		st := l.state
		l.mu.Unlock()
		return fmt.Errorf("leg is %s, not ringing", st)
	}
	l.mu.Unlock()

	negotiated, _, ok := sipmod.NegotiateCodec(remoteSDP, l.supportedCodecs)
	if !ok {
		return fmt.Errorf("no common codec with remote")
	}

	l.mu.Lock()
	l.rtpSess = rtpSess
	l.codecType = negotiated
	l.rtpPT = negotiated.PayloadType()
	l.mu.Unlock()

	l.setupMedia()
	l.setState(StateEarlyMedia)
	return nil
}

// ConnectOutbound sets the answered outbound call on the leg, negotiates the
// codec, starts media, and transitions to connected.
func (l *SIPLeg) ConnectOutbound(call *sipmod.OutboundCall) error {
	l.mu.Lock()
	st := l.state
	if st != StateRinging && st != StateEarlyMedia {
		l.mu.Unlock()
		return fmt.Errorf("leg is %s, expected ringing or early_media", st)
	}
	l.outbound = call
	if st == StateRinging {
		l.rtpSess = call.RTPSess
	}
	// Extract Call-ID for re-INVITE matching.
	if call.Dialog.InviteRequest != nil {
		if cid := call.Dialog.InviteRequest.CallID(); cid != nil {
			l.callID = cid.Value()
		}
	}
	l.mu.Unlock()

	if st == StateRinging {
		negotiated, _, ok := sipmod.NegotiateCodec(call.RemoteSDP, l.supportedCodecs)
		if !ok {
			return fmt.Errorf("no common codec with remote")
		}
		l.codecType = negotiated
		l.rtpPT = negotiated.PayloadType()
		l.setupMedia()
	}

	// Pick up session timer params from the outbound call's 200 OK.
	if call.SessionTimer != nil {
		l.mu.Lock()
		l.sessionInterval = call.SessionTimer.Interval
		l.sessionRefresher = call.SessionTimer.Refresher
		l.mu.Unlock()
	}

	l.mu.Lock()
	l.answeredAt = time.Now()
	l.mu.Unlock()
	l.setState(StateConnected)
	l.startSessionTimer()
	return nil
}

func (l *SIPLeg) ID() string      { return l.id }
func (l *SIPLeg) Type() LegType   { return l.legType }
func (l *SIPLeg) SampleRate() int { return l.codecType.SampleRate() }

func (l *SIPLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *SIPLeg) setState(s LegState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = s
	if s == StateConnected && l.connectedCh != nil {
		l.connectedOnce.Do(func() { close(l.connectedCh) })
	}
}

func (l *SIPLeg) Context() context.Context { return l.ctx }

func (l *SIPLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *SIPLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *SIPLeg) IsMuted() bool   { return l.muted.Load() }
func (l *SIPLeg) SetMuted(m bool) { l.muted.Store(m) }
func (l *SIPLeg) IsDeaf() bool    { return l.deaf.Load() }
func (l *SIPLeg) SetDeaf(d bool)  { l.deaf.Store(d) }

func (l *SIPLeg) CreatedAt() time.Time { return l.createdAt }

func (l *SIPLeg) AnsweredAt() time.Time {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.answeredAt
}

func (l *SIPLeg) SIPHeaders() map[string]string { return l.sipHeaders }

// AnswerCh returns the channel that is closed when the REST answer endpoint is called.
func (l *SIPLeg) AnswerCh() <-chan struct{} {
	return l.answerCh
}

// WaitConnected blocks until the leg reaches connected state or the context
// is cancelled. Returns nil if connected, or the context error otherwise.
func (l *SIPLeg) WaitConnected(ctx context.Context) error {
	if l.connectedCh == nil {
		return nil
	}
	select {
	case <-l.connectedCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SignalAnswer signals the leg to answer (called from REST API).
func (l *SIPLeg) SignalAnswer() {
	select {
	case l.answerCh <- struct{}{}:
	default:
	}
}

// EnableEarlyMedia sends 183 Session Progress with SDP and sets up the media
// pipeline so audio can flow before the call is answered. Only valid for
// inbound legs in StateRinging.
func (l *SIPLeg) EnableEarlyMedia(ctx context.Context) error {
	if l.inbound == nil {
		return fmt.Errorf("cannot enable early media on outbound leg")
	}

	l.mu.RLock()
	st := l.state
	l.mu.RUnlock()
	if st != StateRinging {
		return fmt.Errorf("leg is %s, not ringing", st)
	}

	// Negotiate codec from remote offer
	negotiated, pt, ok := sipmod.NegotiateCodec(l.inbound.RemoteSDP, l.supportedCodecs)
	if !ok {
		return fmt.Errorf("no common codec negotiated")
	}
	l.codecType = negotiated
	l.rtpPT = pt

	// Create RTP session
	rtpSess, err := sipmod.NewRTPSessionFromAllocator(l.engine.PortAllocator())
	if err != nil {
		return fmt.Errorf("create RTP session: %w", err)
	}
	l.rtpSess = rtpSess

	// Set remote RTP address from the offer SDP
	if err := rtpSess.SetRemote(l.inbound.RemoteSDP.RemoteIP, l.inbound.RemoteSDP.RemotePort); err != nil {
		rtpSess.Close()
		return fmt.Errorf("set remote: %w", err)
	}

	// Generate answer SDP — echo the remote's PT for dynamic codecs
	answerSDP := sipmod.GenerateAnswer(sipmod.SDPConfig{
		LocalIP: l.localIP,
		RTPPort: rtpSess.LocalPort(),
		Codecs:  l.supportedCodecs,
	}, negotiated, pt)

	// Store SDP for reuse in Answer()
	l.mu.Lock()
	l.earlyMediaSDP = answerSDP
	l.mu.Unlock()

	// Send 183 Session Progress with SDP
	if err := l.inbound.Dialog.Respond(
		sip.StatusSessionInProgress, "Session Progress",
		answerSDP,
		sip.NewHeader("Content-Type", "application/sdp"),
	); err != nil {
		rtpSess.Close()
		return fmt.Errorf("send 183: %w", err)
	}

	l.setupMedia()
	l.setState(StateEarlyMedia)
	return nil
}

func (l *SIPLeg) Answer(ctx context.Context) error {
	if l.inbound == nil {
		return fmt.Errorf("cannot answer outbound leg")
	}

	// If early media is active, reuse the existing SDP and RTP session.
	l.mu.RLock()
	sdp := l.earlyMediaSDP
	st := l.state
	l.mu.RUnlock()

	if st == StateEarlyMedia && sdp != nil {
		if l.sessionInterval > 0 {
			// Use Respond to include session timer headers.
			if err := l.inbound.Dialog.Respond(sip.StatusOK, "OK", sdp,
				sip.NewHeader("Content-Type", "application/sdp"),
				sip.NewHeader("Supported", "timer"),
				sip.NewHeader("Session-Expires", sipmod.FormatSessionExpires(l.sessionInterval, l.sessionRefresher)),
			); err != nil {
				return fmt.Errorf("respond SDP: %w", err)
			}
		} else {
			if err := l.inbound.Dialog.RespondSDP(sdp); err != nil {
				return fmt.Errorf("respond SDP: %w", err)
			}
		}
		l.mu.Lock()
		l.answeredAt = time.Now()
		l.mu.Unlock()
		l.setState(StateConnected)
		l.startSessionTimer()
		return nil
	}

	// Normal answer path from ringing state.

	// Negotiate codec from remote offer
	negotiated, pt, ok := sipmod.NegotiateCodec(l.inbound.RemoteSDP, l.supportedCodecs)
	if !ok {
		return fmt.Errorf("no common codec negotiated")
	}
	l.codecType = negotiated
	l.rtpPT = pt

	// Create RTP session
	rtpSess, err := sipmod.NewRTPSessionFromAllocator(l.engine.PortAllocator())
	if err != nil {
		return fmt.Errorf("create RTP session: %w", err)
	}
	l.rtpSess = rtpSess

	// Set remote RTP address from the offer SDP
	if err := rtpSess.SetRemote(l.inbound.RemoteSDP.RemoteIP, l.inbound.RemoteSDP.RemotePort); err != nil {
		rtpSess.Close()
		return fmt.Errorf("set remote: %w", err)
	}

	// Generate answer SDP — echo the remote's PT for dynamic codecs
	answerSDP := sipmod.GenerateAnswer(sipmod.SDPConfig{
		LocalIP: l.localIP,
		RTPPort: rtpSess.LocalPort(),
		Codecs:  l.supportedCodecs,
	}, negotiated, pt)

	// Send 200 OK with SDP answer
	if l.sessionInterval > 0 {
		// Include session timer headers in 200 OK.
		if err := l.inbound.Dialog.Respond(sip.StatusOK, "OK", answerSDP,
			sip.NewHeader("Content-Type", "application/sdp"),
			sip.NewHeader("Supported", "timer"),
			sip.NewHeader("Session-Expires", sipmod.FormatSessionExpires(l.sessionInterval, l.sessionRefresher)),
		); err != nil {
			rtpSess.Close()
			return fmt.Errorf("respond SDP: %w", err)
		}
	} else {
		if err := l.inbound.Dialog.RespondSDP(answerSDP); err != nil {
			rtpSess.Close()
			return fmt.Errorf("respond SDP: %w", err)
		}
	}

	l.setupMedia()
	l.mu.Lock()
	l.answeredAt = time.Now()
	l.mu.Unlock()
	l.setState(StateConnected)
	l.startSessionTimer()
	return nil
}

func (l *SIPLeg) setupMedia() {
	var err error
	l.encoder, err = codec.NewEncoder(l.codecType)
	if err != nil {
		l.log.Error("create encoder failed", "codec", l.codecType, "error", err)
		return
	}
	l.decoder, err = codec.NewDecoder(l.codecType)
	if err != nil {
		l.log.Error("create decoder failed", "codec", l.codecType, "error", err)
		return
	}

	l.inFrames = make(chan []byte, 5)
	l.outFrames = make(chan []byte, 5)
	l.dtmfCh = make(chan string, 5)

	l.log.Info("SIP leg media setup",
		"leg_id", l.id,
		"codec", l.codecType.String(),
		"payload_type", l.rtpPT,
		"sample_rate", l.codecType.SampleRate(),
		"clock_rate", l.codecType.ClockRate(),
	)

	go l.readLoop()
	go l.writeLoop()
}

// readLoop reads RTP packets from the UDP socket, decodes audio, and pushes
// native-rate PCM frames into inFrames.
func (l *SIPLeg) readLoop() {
	for {
		// Set read deadline for RTP timeout detection.
		// When held, use a very long deadline (beyond hold timer) to avoid
		// false RTP timeouts since no RTP is expected while on hold.
		l.mu.RLock()
		isHeld := l.held
		l.mu.RUnlock()
		if isHeld {
			l.rtpSess.SetReadDeadline(time.Now().Add(2*time.Hour + time.Minute))
		} else {
			l.rtpSess.SetReadDeadline(time.Now().Add(rtpTimeout))
		}

		pkt, err := l.rtpSess.ReadRTP()
		if err != nil {
			select {
			case <-l.ctx.Done():
				return
			default:
			}
			// Non-RTP packets (RTCP, STUN) fail unmarshal — skip them.
			if errors.Is(err, sipmod.ErrNotRTP) {
				continue
			}
			// Check for read deadline timeout (no RTP received).
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				l.log.Info("RTP timeout", "leg_id", l.id, "timeout", rtpTimeout)
				l.mu.RLock()
				cb := l.onRTPTimeout
				l.mu.RUnlock()
				if cb != nil {
					cb()
				}
				return
			}
			l.log.Debug("readLoop: ReadRTP error", "error", err)
			return
		}

		// Track sequence number on ALL valid RTP packets (including DTMF,
		// comfort noise, etc.) since they share the same sequence number space.
		l.trackRTPSeq(pkt.SequenceNumber)

		// Handle DTMF telephone-event (PT 101 = 8kHz, PT 100 = 48kHz for Opus)
		if pkt.PayloadType == 100 || pkt.PayloadType == 101 {
			ev, err := sipmod.DecodeDTMFEvent(pkt.Payload)
			if err != nil {
				continue
			}
			// Only fire callback on end-of-event. RFC 4733 senders retransmit
			// end-of-event 3 times with the same timestamp — deduplicate on it.
			if ev.EndOfEvent && pkt.Timestamp != l.lastDTMFTS {
				l.lastDTMFTS = pkt.Timestamp
				digit, ok := sipmod.DTMFEventToDigit(ev.Event)
				if ok {
					l.mu.RLock()
					cb := l.onDTMF
					l.mu.RUnlock()
					if cb != nil {
						cb(digit)
					}
				}
			}
			continue
		}

		// Skip packets that don't match the negotiated codec PT (e.g.
		// comfort noise, keep-alive, or other non-audio PTs).
		if pkt.PayloadType != l.rtpPT {
			continue
		}

		// Skip empty payloads (some endpoints send marker-only packets).
		if len(pkt.Payload) == 0 {
			continue
		}

		// Update jitter on audio packets only (DTMF retransmits share
		// timestamps, which would spike the jitter estimate).
		l.updateRTPJitter(pkt)

		// Decode audio payload
		samples, err := l.decoder.Decode(pkt.Payload)
		if err != nil {
			head := pkt.Payload
			if len(head) > 8 {
				head = head[:8]
			}
			l.log.Debug("readLoop: decode error", "error", err,
				"pt", pkt.PayloadType, "expected_pt", l.rtpPT,
				"payload_len", len(pkt.Payload), "payload_head", fmt.Sprintf("%x", head),
				"seq", pkt.SequenceNumber)
			continue
		}

		// Convert decoded samples at native rate to PCM bytes
		pcm := samplesToBytes(samples)

		// Write to incoming taps before pushing to inFrames.
		l.mu.RLock()
		tap := l.inTap
		at := l.amdTap
		st := l.speakingTap
		l.mu.RUnlock()
		if tap != nil {
			tap.Write(pcm)
		}
		if at != nil {
			at.Write(pcm)
		}
		if st != nil {
			st.Write(pcm)
		}

		// Push to inFrames, drop oldest on overflow
		select {
		case l.inFrames <- pcm:
		default:
			select {
			case <-l.inFrames:
			default:
			}
			l.inFrames <- pcm
		}
	}
}

// trackRTPSeq updates sequence number and packet count for ALL received RTP
// packets (audio, DTMF, comfort noise) since they share one sequence space.
func (l *SIPLeg) trackRTPSeq(seq uint16) {
	l.rtpStatsMu.Lock()
	l.rtpReceived++
	if !l.rtpHasFirst {
		l.rtpFirstSeq = seq
		l.rtpLastSeq = seq
		l.rtpHasFirst = true
	} else {
		l.rtpLastSeq = seq
	}
	l.rtpStatsMu.Unlock()
}

// updateRTPJitter updates inter-arrival jitter (RFC 3550 §A.8) on audio
// packets only. DTMF end-of-event retransmits share the same RTP timestamp,
// which would spike the jitter estimate if included.
func (l *SIPLeg) updateRTPJitter(pkt *rtp.Packet) {
	clockRate := int64(l.codecType.ClockRate())
	arrival := time.Now().UnixNano() * clockRate / 1e9
	transit := arrival - int64(pkt.Timestamp)

	l.rtpStatsMu.Lock()
	if l.rtpLastTransit != 0 {
		d := transit - l.rtpLastTransit
		if d < 0 {
			d = -d
		}
		l.rtpJitter += (float64(d) - l.rtpJitter) / 16
	}
	l.rtpLastTransit = transit
	l.rtpStatsMu.Unlock()
}

// RTPStats returns inbound stream quality metrics and a MOS estimate.
func (l *SIPLeg) RTPStats() RTPStats {
	l.rtpStatsMu.Lock()
	received := l.rtpReceived
	firstSeq := l.rtpFirstSeq
	lastSeq := l.rtpLastSeq
	jitter := l.rtpJitter
	hasFirst := l.rtpHasFirst
	clockRate := l.codecType.ClockRate()
	l.rtpStatsMu.Unlock()

	if !hasFirst || received < 2 {
		return RTPStats{}
	}

	expected := uint32(lastSeq-firstSeq) + 1
	var lost uint32
	if received < expected {
		lost = expected - received
	}
	lossRate := float64(lost) / float64(expected)
	jitterMs := jitter / float64(clockRate) * 1000

	return RTPStats{
		PacketsReceived: received,
		PacketsLost:     lost,
		JitterMs:        math.Round(jitterMs*100) / 100,
		MOSScore:        calculateMOS(lossRate, jitterMs),
	}
}

// writeLoop sends RTP packets on a 20ms ticker.
func (l *SIPLeg) writeLoop() {
	const (
		ptime            = 20 * time.Millisecond
		telephoneEventPT = uint8(101)
	)

	// PCM frame size at codec's native sample rate: samples per 20ms × 2 bytes
	pcmFrameBytes := l.codecType.SampleRate() / 50 * 2

	// RTP timestamp increment is codec-dependent: clockRate * 20ms
	samplesPerFrame := uint32(l.codecType.ClockRate() / 50)

	ticker := time.NewTicker(ptime)
	defer ticker.Stop()

	ssrc := rand.Uint32()
	var seqNum uint16
	var timestamp uint32
	silenceFrame := make([]byte, pcmFrameBytes)
	pt := l.rtpPT

	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
		}

		// When held, stop sending RTP — the remote is not listening.
		// Drain any queued outFrames so writers don't block.
		l.mu.RLock()
		isHeld := l.held
		l.mu.RUnlock()
		if isHeld {
			select {
			case <-l.outFrames:
			default:
			}
			timestamp += samplesPerFrame
			continue
		}

		// Check for pending DTMF digits.
		var dtmfDigits string
		select {
		case dtmfDigits = <-l.dtmfCh:
		default:
		}

		if dtmfDigits != "" {
			for _, ch := range dtmfDigits {
				pkts := sipmod.GenerateDTMFPackets(ch, telephoneEventPT, ssrc, seqNum, timestamp)
				for i, pkt := range pkts {
					if err := l.rtpSess.WriteRTP(pkt); err != nil {
						l.log.Error("writeLoop: DTMF WriteRTP failed", "error", err)
						return
					}
					seqNum++

					// Wait for next tick (except after last packet)
					if i < len(pkts)-1 {
						select {
						case <-l.ctx.Done():
							return
						case <-ticker.C:
						}
					}
				}
				// Advance timestamp past the DTMF event duration
				timestamp += samplesPerFrame * uint32(len(pkts))
			}
			continue
		}

		// Normal audio frame.
		var frame []byte
		select {
		case frame = <-l.outFrames:
		default:
			frame = silenceFrame
		}

		// Write to outgoing tap (for recording) before encoding.
		l.mu.RLock()
		oTap := l.outTap
		l.mu.RUnlock()
		if oTap != nil {
			oTap.Write(frame)
		}

		// Parse PCM bytes to int16 samples (already at native rate)
		samples := bytesToSamples(frame)

		encoded, err := l.encoder.Encode(samples)
		if err != nil {
			l.log.Error("writeLoop: encode failed", "error", err)
			return
		}

		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    pt,
				SequenceNumber: seqNum,
				Timestamp:      timestamp,
				SSRC:           ssrc,
			},
			Payload: encoded,
		}
		if err := l.rtpSess.WriteRTP(pkt); err != nil {
			l.log.Error("writeLoop: WriteRTP failed", "error", err)
			return
		}
		seqNum++
		timestamp += samplesPerFrame
	}
}

func (l *SIPLeg) Hangup(ctx context.Context) error {
	l.setState(StateHungUp)
	defer l.cancel()

	// Cancel hold timer if active.
	l.mu.Lock()
	if l.holdTimer != nil {
		l.holdTimer.Stop()
		l.holdTimer = nil
	}
	l.mu.Unlock()

	// Cancel session timer if active.
	l.stopSessionTimer()

	// Close RTP session
	if l.rtpSess != nil {
		l.rtpSess.Close()
	}

	// Send BYE via dialog
	if l.inbound != nil {
		return l.inbound.Dialog.Bye(ctx)
	}
	if l.outbound != nil {
		return l.outbound.Dialog.Bye(ctx)
	}
	return nil
}

// sipReader reads PCM frames from the inFrames channel.
type sipReader struct {
	frames <-chan []byte
	ctx    context.Context
	buf    []byte
}

func (r *sipReader) Read(p []byte) (int, error) {
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

// sipWriter sends PCM frames into SIPLeg's outFrames channel.
type sipWriter struct {
	frames chan<- []byte
	ctx    context.Context
}

func (w *sipWriter) Write(p []byte) (int, error) {
	frame := make([]byte, len(p))
	copy(frame, p)
	select {
	case w.frames <- frame:
		return len(p), nil
	case <-w.ctx.Done():
		return 0, io.ErrClosedPipe
	}
}

func (l *SIPLeg) AudioReader() io.Reader {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.inFrames == nil {
		return nil
	}
	return &sipReader{frames: l.inFrames, ctx: l.ctx}
}

func (l *SIPLeg) AudioWriter() io.Writer {
	if l.outFrames == nil {
		return nil
	}
	return &sipWriter{frames: l.outFrames, ctx: l.ctx}
}

// SetInTap sets a writer that receives a copy of every decoded incoming PCM
// frame (before it enters inFrames). Used for standalone leg recording.
func (l *SIPLeg) SetInTap(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inTap = w
}

// ClearInTap removes the incoming tap.
func (l *SIPLeg) ClearInTap() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inTap = nil
}

// SetOutTap sets a writer that receives a copy of every outgoing PCM frame
// (including silence) from the writeLoop. Used for standalone leg recording.
func (l *SIPLeg) SetOutTap(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.outTap = w
}

// ClearOutTap removes the outgoing tap.
func (l *SIPLeg) ClearOutTap() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.outTap = nil
}

// SetAMDTap sets a writer that receives decoded incoming PCM for answering
// machine detection. Separate from inTap so AMD and recording can coexist.
func (l *SIPLeg) SetAMDTap(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.amdTap = w
}

// ClearAMDTap removes the AMD tap.
func (l *SIPLeg) ClearAMDTap() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.amdTap = nil
}

// SetSpeakingTap sets a writer that receives decoded incoming PCM for
// voice activity detection.
func (l *SIPLeg) SetSpeakingTap(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.speakingTap = w
}

// ClearSpeakingTap removes the speaking detection tap.
func (l *SIPLeg) ClearSpeakingTap() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.speakingTap = nil
}

func (l *SIPLeg) OnDTMF(f func(digit rune)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onDTMF = f
}

// OnRTPTimeout sets a callback that fires when no RTP packets are received
// within the timeout period (30s). Called at most once.
func (l *SIPLeg) OnRTPTimeout(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onRTPTimeout = f
}

// ReInviteAnswerSDP builds an SDP body for responding to a remote re-INVITE.
// The direction is mirrored: if the remote sent "sendonly" (hold), we respond
// with "recvonly"; if "sendrecv" we respond with "sendrecv".
func (l *SIPLeg) ReInviteAnswerSDP(remoteDirection string) []byte {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Mirror the direction: remote sendonly → we recvonly, etc.
	ourDirection := "sendrecv"
	switch remoteDirection {
	case "sendonly":
		ourDirection = "recvonly"
	case "recvonly":
		ourDirection = "sendonly"
	case "inactive":
		ourDirection = "inactive"
	}

	if l.rtpSess == nil {
		return nil
	}

	return sipmod.GenerateReInviteSDP(sipmod.SDPConfig{
		LocalIP: l.localIP,
		RTPPort: l.rtpSess.LocalPort(),
		Codecs:  l.supportedCodecs,
	}, l.codecType, l.rtpPT, ourDirection)
}

// CallID returns the SIP Call-ID for this leg's dialog.
func (l *SIPLeg) CallID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.callID
}

// IsHeld returns true if the call is currently on hold.
func (l *SIPLeg) IsHeld() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.held
}

// OnHold registers a callback for when the leg is put on hold.
func (l *SIPLeg) OnHold(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onHold = f
}

// OnUnhold registers a callback for when the leg is taken off hold.
func (l *SIPLeg) OnUnhold(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onUnhold = f
}

// SetHeld updates hold state and manages the 2-hour auto-hangup timer.
func (l *SIPLeg) SetHeld(held bool) {
	l.mu.Lock()
	if l.held == held {
		l.mu.Unlock()
		return
	}
	l.held = held

	if held {
		l.state = StateHeld
		// Start 2-hour hold timer for auto-hangup.
		l.holdTimer = time.AfterFunc(2*time.Hour, func() {
			l.log.Info("hold timeout", "leg_id", l.id)
			l.mu.RLock()
			cb := l.onRTPTimeout
			l.mu.RUnlock()
			if cb != nil {
				cb()
			}
		})
	} else {
		l.state = StateConnected
		// Cancel hold timer.
		if l.holdTimer != nil {
			l.holdTimer.Stop()
			l.holdTimer = nil
		}
	}
	l.mu.Unlock()
}

// Hold initiates hold by sending a re-INVITE with sendonly SDP.
func (l *SIPLeg) Hold(ctx context.Context) error {
	l.mu.RLock()
	st := l.state
	isHeld := l.held
	l.mu.RUnlock()

	if isHeld {
		return nil // already held
	}
	if st != StateConnected {
		return fmt.Errorf("leg is %s, must be connected to hold", st)
	}

	sdpBody := sipmod.GenerateReInviteSDP(sipmod.SDPConfig{
		LocalIP: l.localIP,
		RTPPort: l.rtpSess.LocalPort(),
		Codecs:  l.supportedCodecs,
	}, l.codecType, l.rtpPT, "sendonly")

	var dialog interface{}
	if l.inbound != nil {
		dialog = l.inbound.Dialog
	} else if l.outbound != nil {
		dialog = l.outbound.Dialog
	} else {
		return fmt.Errorf("no dialog available")
	}

	if err := l.engine.SendReInvite(ctx, dialog, sdpBody); err != nil {
		return fmt.Errorf("hold re-INVITE: %w", err)
	}

	l.SetHeld(true)
	l.mu.RLock()
	cb := l.onHold
	l.mu.RUnlock()
	if cb != nil {
		cb()
	}
	return nil
}

// Unhold resumes the call by sending a re-INVITE with sendrecv SDP.
func (l *SIPLeg) Unhold(ctx context.Context) error {
	l.mu.RLock()
	isHeld := l.held
	l.mu.RUnlock()

	if !isHeld {
		return nil // not held
	}

	sdpBody := sipmod.GenerateReInviteSDP(sipmod.SDPConfig{
		LocalIP: l.localIP,
		RTPPort: l.rtpSess.LocalPort(),
		Codecs:  l.supportedCodecs,
	}, l.codecType, l.rtpPT, "sendrecv")

	var dialog interface{}
	if l.inbound != nil {
		dialog = l.inbound.Dialog
	} else if l.outbound != nil {
		dialog = l.outbound.Dialog
	} else {
		return fmt.Errorf("no dialog available")
	}

	if err := l.engine.SendReInvite(ctx, dialog, sdpBody); err != nil {
		return fmt.Errorf("unhold re-INVITE: %w", err)
	}

	l.SetHeld(false)
	l.mu.RLock()
	cb := l.onUnhold
	l.mu.RUnlock()
	if cb != nil {
		cb()
	}
	return nil
}

// HandleRemoteHold processes a remote re-INVITE's SDP direction for hold/unhold.
func (l *SIPLeg) HandleRemoteHold(direction string) {
	switch direction {
	case "sendonly", "inactive":
		l.SetHeld(true)
		l.mu.RLock()
		cb := l.onHold
		l.mu.RUnlock()
		if cb != nil {
			cb()
		}
	case "sendrecv", "recvonly":
		l.SetHeld(false)
		l.mu.RLock()
		cb := l.onUnhold
		l.mu.RUnlock()
		if cb != nil {
			cb()
		}
	}
}

// SessionTimerParams returns the negotiated session timer interval and refresher.
// Returns (0, "") if session timers are not active.
func (l *SIPLeg) SessionTimerParams() (interval uint32, refresher string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.sessionInterval, l.sessionRefresher
}

// OnSessionExpired registers a callback for when the session timer expires
// without a refresh. Typically used to hang up the call.
func (l *SIPLeg) OnSessionExpired(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onSessionExpired = f
}

// startSessionTimer starts the session refresh or expiry timer after the call
// is answered. Must only be called once.
func (l *SIPLeg) startSessionTimer() {
	l.mu.RLock()
	interval := l.sessionInterval
	refresher := l.sessionRefresher
	l.mu.RUnlock()

	if interval == 0 {
		return
	}

	if refresher == "uas" {
		// We are the refresher — send re-INVITE at half the interval.
		refreshAt := time.Duration(interval) * time.Second / 2
		l.log.Info("session timer: uas refresher", "leg_id", l.id, "interval", interval, "refresh_at", refreshAt)
		l.mu.Lock()
		l.sessionTimer = time.AfterFunc(refreshAt, l.sessionRefresh)
		l.mu.Unlock()
	} else {
		// Remote is the refresher — guard timer at full interval + 32s grace
		// (RFC 4028 §10 recommends a grace period of at least 32 seconds).
		guardAt := time.Duration(interval)*time.Second + 32*time.Second
		l.log.Info("session timer: uac refresher", "leg_id", l.id, "interval", interval, "guard_at", guardAt)
		l.mu.Lock()
		l.sessionTimer = time.AfterFunc(guardAt, l.sessionExpired)
		l.mu.Unlock()
	}
}

// sessionRefresh fires when we (UAS) need to refresh the session.
func (l *SIPLeg) sessionRefresh() {
	l.mu.RLock()
	st := l.state
	interval := l.sessionInterval
	l.mu.RUnlock()

	if st == StateHungUp {
		return
	}

	l.log.Info("session timer: sending refresh re-INVITE", "leg_id", l.id)

	sdpBody := l.ReInviteAnswerSDP("sendrecv")
	if sdpBody == nil {
		l.log.Error("session timer: could not generate refresh SDP", "leg_id", l.id)
		return
	}

	var dialog interface{}
	if l.inbound != nil {
		dialog = l.inbound.Dialog
	} else if l.outbound != nil {
		dialog = l.outbound.Dialog
	}
	if dialog == nil {
		return
	}

	if err := l.engine.SendReInvite(l.ctx, dialog, sdpBody); err != nil {
		l.log.Error("session timer: refresh re-INVITE failed", "leg_id", l.id, "error", err)
		// On failure, try once more at 75% of remaining interval.
		remaining := time.Duration(interval) * time.Second / 4
		l.mu.Lock()
		l.sessionTimer = time.AfterFunc(remaining, l.sessionExpired)
		l.mu.Unlock()
		return
	}

	// Schedule next refresh.
	refreshAt := time.Duration(interval) * time.Second / 2
	l.mu.Lock()
	l.sessionTimer = time.AfterFunc(refreshAt, l.sessionRefresh)
	l.mu.Unlock()
}

// sessionExpired fires when the remote failed to refresh the session in time.
func (l *SIPLeg) sessionExpired() {
	l.log.Info("session timer expired", "leg_id", l.id)
	l.mu.RLock()
	cb := l.onSessionExpired
	l.mu.RUnlock()
	if cb != nil {
		cb()
	}
}

// ResetSessionTimer resets the session timer, called when a refresh re-INVITE
// is received from the remote UA.
func (l *SIPLeg) ResetSessionTimer() {
	l.mu.Lock()
	interval := l.sessionInterval
	refresher := l.sessionRefresher
	t := l.sessionTimer
	l.mu.Unlock()

	if interval == 0 || t == nil {
		return
	}

	l.log.Debug("session timer reset", "leg_id", l.id, "interval", interval, "refresher", refresher)

	if refresher == "uas" {
		t.Reset(time.Duration(interval) * time.Second / 2)
	} else {
		t.Reset(time.Duration(interval)*time.Second + 32*time.Second)
	}
}

// stopSessionTimer stops the session timer. Called on hangup.
func (l *SIPLeg) stopSessionTimer() {
	l.mu.Lock()
	if l.sessionTimer != nil {
		l.sessionTimer.Stop()
		l.sessionTimer = nil
	}
	l.mu.Unlock()
}

func (l *SIPLeg) SendDTMF(ctx context.Context, digits string) error {
	if l.dtmfCh == nil {
		return fmt.Errorf("no DTMF channel available")
	}
	select {
	case l.dtmfCh <- digits:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// samplesToBytes converts int16 samples to 16-bit LE PCM bytes.
func samplesToBytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

// bytesToSamples converts 16-bit LE PCM bytes to int16 samples.
func bytesToSamples(pcm []byte) []int16 {
	n := len(pcm) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}
