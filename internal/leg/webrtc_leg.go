package leg

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/zaf/g711"
)

const (
	pcmFrameSize = 320 // 160 samples * 2 bytes at 8kHz/20ms
	rtpClockRate = 8000
	rtpPtime     = 20
	rtpSamples   = 160
)

// WebRTCLeg wraps a pion PeerConnection as a Leg.
type WebRTCLeg struct {
	id    string
	state LegState
	mu    sync.RWMutex

	pc         *webrtc.PeerConnection
	localTrack *webrtc.TrackLocalStaticRTP

	ctx       context.Context
	cancel    context.CancelFunc
	roomID    string
	muted     atomic.Bool
	deaf      atomic.Bool
	createdAt time.Time

	// Ring buffers for mixer integration
	inFrames  chan []byte // incoming PCM frames from browser
	outFrames chan []byte // outgoing PCM frames to browser

	// Trickle ICE
	iceCandidates []webrtc.ICECandidateInit
	iceDone       bool

	onDTMF func(digit rune)
	log    *slog.Logger
}

func NewWebRTCLeg(pc *webrtc.PeerConnection, localTrack *webrtc.TrackLocalStaticRTP, log *slog.Logger) *WebRTCLeg {
	ctx, cancel := context.WithCancel(context.Background())
	l := &WebRTCLeg{
		id:         uuid.New().String(),
		state:      StateConnected,
		createdAt:  time.Now(),
		pc:         pc,
		localTrack: localTrack,
		ctx:        ctx,
		cancel:     cancel,
		inFrames:   make(chan []byte, 5),
		outFrames:  make(chan []byte, 5),
		log:        log,
	}

	// Start outbound writer goroutine
	go l.writeLoop()

	return l
}

func (l *WebRTCLeg) ID() string      { return l.id }
func (l *WebRTCLeg) Type() LegType   { return TypeWebRTC }
func (l *WebRTCLeg) SampleRate() int { return 8000 }

func (l *WebRTCLeg) State() LegState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *WebRTCLeg) Context() context.Context { return l.ctx }

func (l *WebRTCLeg) RoomID() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roomID
}

func (l *WebRTCLeg) SetRoomID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roomID = id
}

func (l *WebRTCLeg) IsMuted() bool              { return l.muted.Load() }
func (l *WebRTCLeg) SetMuted(m bool)            { l.muted.Store(m) }
func (l *WebRTCLeg) IsDeaf() bool               { return l.deaf.Load() }
func (l *WebRTCLeg) SetDeaf(d bool)             { l.deaf.Store(d) }
func (l *WebRTCLeg) SetSpeakingTap(_ io.Writer) {}
func (l *WebRTCLeg) ClearSpeakingTap()          {}
func (l *WebRTCLeg) IsHeld() bool               { return false }

func (l *WebRTCLeg) CreatedAt() time.Time          { return l.createdAt }
func (l *WebRTCLeg) AnsweredAt() time.Time         { return l.createdAt } // WebRTC legs are connected immediately
func (l *WebRTCLeg) SIPHeaders() map[string]string { return nil }
func (l *WebRTCLeg) RTPStats() RTPStats            { return RTPStats{} }

func (l *WebRTCLeg) Answer(_ context.Context) error {
	return fmt.Errorf("webrtc legs do not need explicit answer")
}

func (l *WebRTCLeg) Hangup(_ context.Context) error {
	l.mu.Lock()
	l.state = StateHungUp
	l.mu.Unlock()
	l.cancel()
	return l.pc.Close()
}

func (l *WebRTCLeg) OnDTMF(f func(digit rune)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onDTMF = f
}

func (l *WebRTCLeg) SendDTMF(_ context.Context, _ string) error {
	return fmt.Errorf("DTMF send over WebRTC not yet implemented")
}

// AddICECandidate adds a remote ICE candidate to the peer connection.
func (l *WebRTCLeg) AddICECandidate(c webrtc.ICECandidateInit) error {
	return l.pc.AddICECandidate(c)
}

// PushLocalCandidate buffers a locally gathered ICE candidate for retrieval by the client.
func (l *WebRTCLeg) PushLocalCandidate(c webrtc.ICECandidateInit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.iceCandidates = append(l.iceCandidates, c)
}

// SetICEGatheringDone marks local ICE gathering as complete.
func (l *WebRTCLeg) SetICEGatheringDone() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.iceDone = true
}

// DrainCandidates returns and clears all buffered local ICE candidates,
// plus a flag indicating whether gathering is complete.
func (l *WebRTCLeg) DrainCandidates() ([]webrtc.ICECandidateInit, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cs := l.iceCandidates
	l.iceCandidates = nil
	return cs, l.iceDone
}

// HandleTrack is called from OnTrack to process incoming audio.
func (l *WebRTCLeg) HandleTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				return
			}
			// Decode the RTP payload (assumed PCMU for now)
			pkt := &rtp.Packet{}
			if err := pkt.Unmarshal(buf[:n]); err != nil {
				continue
			}
			pcm := ulawToPCM(pkt.Payload)
			select {
			case l.inFrames <- pcm:
			default:
				// Drop oldest frame to avoid blocking
				select {
				case <-l.inFrames:
				default:
				}
				l.inFrames <- pcm
			}
		}
	}()
}

// AudioReader returns a reader that provides decoded PCM from the browser.
func (l *WebRTCLeg) AudioReader() io.Reader {
	return &webrtcReader{frames: l.inFrames, ctx: l.ctx}
}

// AudioWriter returns a writer that sends PCM to the browser.
func (l *WebRTCLeg) AudioWriter() io.Writer {
	return &webrtcWriter{frames: l.outFrames, ctx: l.ctx}
}

func (l *WebRTCLeg) writeLoop() {
	var seqNum uint16
	var timestamp uint32
	ticker := time.NewTicker(time.Duration(rtpPtime) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			var frame []byte
			select {
			case frame = <-l.outFrames:
			default:
				// Send silence
				frame = make([]byte, pcmFrameSize)
			}
			encoded := pcmToUlaw(frame)
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    0, // PCMU
					SequenceNumber: seqNum,
					Timestamp:      timestamp,
					SSRC:           12345678,
				},
				Payload: encoded,
			}
			raw, err := pkt.Marshal()
			if err != nil {
				continue
			}
			if _, err := l.localTrack.Write(raw); err != nil {
				return
			}
			seqNum++
			timestamp += rtpSamples
		}
	}
}

// webrtcReader reads PCM frames from a channel.
type webrtcReader struct {
	frames <-chan []byte
	ctx    context.Context
	buf    []byte
}

func (r *webrtcReader) Read(p []byte) (int, error) {
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

// webrtcWriter writes PCM frames into a channel.
type webrtcWriter struct {
	frames chan<- []byte
	ctx    context.Context
}

func (w *webrtcWriter) Write(p []byte) (int, error) {
	frame := make([]byte, len(p))
	copy(frame, p)
	select {
	case w.frames <- frame:
		return len(p), nil
	case <-w.ctx.Done():
		return 0, io.ErrClosedPipe
	}
}

// ulawToPCM decodes PCMU to 16-bit LE PCM.
func ulawToPCM(ulaw []byte) []byte {
	pcm := make([]byte, len(ulaw)*2)
	for i, b := range ulaw {
		sample := g711.DecodeUlawFrame(b)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}
	return pcm
}

// pcmToUlaw encodes 16-bit LE PCM to PCMU.
func pcmToUlaw(pcm []byte) []byte {
	ulaw := make([]byte, len(pcm)/2)
	for i := 0; i < len(pcm)/2; i++ {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		ulaw[i] = g711.EncodeUlawFrame(sample)
	}
	return ulaw
}

// clampInt16 clamps an int32 sample to int16 range.
func clampInt16(s int32) int16 {
	if s > math.MaxInt16 {
		return math.MaxInt16
	}
	if s < math.MinInt16 {
		return math.MinInt16
	}
	return int16(s)
}
