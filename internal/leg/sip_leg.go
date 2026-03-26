package leg

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
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

	ctx        context.Context
	cancel     context.CancelFunc
	roomID     string
	muted      atomic.Bool
	createdAt  time.Time
	answeredAt time.Time // zero if never answered
	answerCh      chan struct{} // signaled by REST answer endpoint (inbound only)
	onDTMF        func(digit rune)
	onRTPTimeout  func() // called when no RTP received within timeout
	onHold        func() // called when leg is put on hold
	onUnhold      func() // called when leg is taken off hold

	callID    string      // SIP Call-ID for re-INVITE matching
	held      bool        // true when call is on hold
	holdTimer *time.Timer // 2-hour auto-hangup timer

	// Media
	rtpSess   *sipmod.RTPSession
	codecType codec.CodecType
	rtpPT     uint8 // negotiated RTP payload type (may differ from codec default for dynamic PTs)
	encoder   codec.Encoder
	decoder   codec.Decoder
	inFrames  chan []byte // decoded native-rate PCM from readLoop
	outFrames chan []byte // native-rate PCM to encode in writeLoop
	dtmfCh    chan string // DTMF digits to send in writeLoop

	earlyMediaSDP []byte         // SDP sent in 183, reused in 200 OK on Answer
	sipHeaders    map[string]string // X-* headers from inbound INVITE or outbound request

	engine          *sipmod.Engine    // for sending re-INVITEs
	localIP         string            // for SDP answer generation
	supportedCodecs []codec.CodecType // from engine config

	// Optional taps for recording on standalone legs.
	inTap  io.Writer // copy of decoded incoming PCM (before inFrames)
	outTap io.Writer // copy of outgoing PCM (from writeLoop, including silence)

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

	return &SIPLeg{
		id:              uuid.New().String(),
		legType:         TypeSIPInbound,
		state:           StateRinging,
		createdAt:       time.Now(),
		inbound:         call,
		sipHeaders:      hdrs,
		ctx:             ctx,
		cancel:          cancel,
		answerCh:        make(chan struct{}),
		callID:          callID,
		engine:          engine,
		localIP:         engine.BindIP(),
		supportedCodecs: engine.Codecs(),
		log:             log,
	}
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

	l.mu.Lock()
	l.answeredAt = time.Now()
	l.mu.Unlock()
	l.setState(StateConnected)
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

func (l *SIPLeg) IsMuted() bool    { return l.muted.Load() }
func (l *SIPLeg) SetMuted(m bool)  { l.muted.Store(m) }

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
	rtpSess, err := sipmod.NewRTPSession()
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
		if err := l.inbound.Dialog.RespondSDP(sdp); err != nil {
			return fmt.Errorf("respond SDP: %w", err)
		}
		l.mu.Lock()
		l.answeredAt = time.Now()
		l.mu.Unlock()
		l.setState(StateConnected)
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
	rtpSess, err := sipmod.NewRTPSession()
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
	if err := l.inbound.Dialog.RespondSDP(answerSDP); err != nil {
		rtpSess.Close()
		return fmt.Errorf("respond SDP: %w", err)
	}

	l.setupMedia()
	l.mu.Lock()
	l.answeredAt = time.Now()
	l.mu.Unlock()
	l.setState(StateConnected)
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

		// Handle DTMF telephone-event (PT 101 = 8kHz, PT 100 = 48kHz for Opus)
		if pkt.PayloadType == 100 || pkt.PayloadType == 101 {
			ev, err := sipmod.DecodeDTMFEvent(pkt.Payload)
			if err != nil {
				continue
			}
			// Only fire callback on end-of-event to avoid duplicates
			if ev.EndOfEvent {
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

		// Write to incoming tap (for recording) before pushing to inFrames.
		l.mu.RLock()
		tap := l.inTap
		l.mu.RUnlock()
		if tap != nil {
			tap.Write(pcm)
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
