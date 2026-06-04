package lkmedia

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// PeerConfig configures the pion PeerConnections used by the transport.
type PeerConfig struct {
	// ICEServers overrides the STUN/TURN list from JoinResponse. When non-empty,
	// VoiceBlender uses these instead of the server-supplied list.
	ICEServers []webrtc.ICEServer
	// RTPPortMin/RTPPortMax constrain pion's ephemeral UDP port range.
	// Both zero means use pion defaults.
	RTPPortMin, RTPPortMax int
}

// Callbacks bundles optional observability hooks the leg layer wires up to
// VoiceBlender's event bus (Model C: LK-internal state surfaces as scoped
// events on the leg, not as VB room participants).
type Callbacks struct {
	OnParticipantsUpdated  func(participants []*livekit.ParticipantInfo)
	OnSpeakersChanged      func(speakers []*livekit.SpeakerInfo)
	OnTrackPublishedRemote func(participantSID string, track *livekit.TrackInfo)
	OnTrackUnpublishedSID  func(trackSID string)
	OnConnectionQuality    func(updates []*livekit.ConnectionQualityInfo)
	OnRefreshToken         func(token string)
}

// Transport wraps the pion publisher + subscriber PeerConnections plus the
// signaling client. It exposes PCM audio I/O for the VoiceBlender room
// mixer and surfaces LK-internal state via Callbacks.
type Transport struct {
	cfg    Config
	peer   PeerConfig
	log    *slog.Logger
	signal *SignalClient
	cb     Callbacks

	publisher  *webrtc.PeerConnection
	subscriber *webrtc.PeerConnection

	localTrack *webrtc.TrackLocalStaticSample
	localCID   string
	localSID   atomic.Value // string; set when server confirms via TrackPublished

	mixer  *remoteMixer
	outPCM *streamBuffer // PCM bytes the leg writes (sendLoop reads)

	encoder *codec.OpusEncoder

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce   sync.Once
	closed      atomic.Bool
	closeErr    atomic.Pointer[error]
	closeReason atomic.Pointer[string]
	done        chan struct{}

	mu                   sync.Mutex
	publisherNegotiating bool
}

// NewTransport dials LiveKit, completes signaling JOIN, sets up both pion
// PeerConnections, and starts the audio pumps. On any error during setup
// the transport is closed and the error is returned without emitting
// events.
func NewTransport(ctx context.Context, cfg Config, signalCfg SignalConfig, peer PeerConfig, cb Callbacks) (*Transport, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	log := cfg.Log.With("component", "lkmedia.transport")
	if signalCfg.Log == nil {
		signalCfg.Log = log
	}

	sc, err := dialSignal(ctx, signalCfg)
	if err != nil {
		return nil, err
	}
	join := sc.JoinResponse()

	if !join.SubscriberPrimary {
		// v1 only handles the modern subscriber-primary mode. The legacy
		// publisher-primary flow has a different offer/answer ordering;
		// supporting it is tracked separately if a deployment requires it.
		_ = sc.Close(livekit.DisconnectReason_CLIENT_INITIATED)
		return nil, errors.New("lkmedia: server requires legacy publisher-primary mode (not supported)")
	}

	enc, err := codec.NewOpusEncoder()
	if err != nil {
		_ = sc.Close(livekit.DisconnectReason_CLIENT_INITIATED)
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	if err := enc.SetBitrate(cfg.OpusBitrate); err != nil {
		log.Warn("opus SetBitrate failed; using library default", "error", err, "bitrate", cfg.OpusBitrate)
	}

	mixer := newRemoteMixer(cfg, log)
	mixer.Start()

	tctx, cancel := context.WithCancel(context.Background())
	t := &Transport{
		cfg:     cfg,
		peer:    peer,
		log:     log,
		signal:  sc,
		cb:      cb,
		mixer:   mixer,
		outPCM:  newStreamBuffer(cfg.IngressBufferBytes(), cfg.FrameMs),
		encoder: enc,
		ctx:     tctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}

	pub, sub, localTrack, err := t.buildPeerConnections(join.IceServers)
	if err != nil {
		t.cleanup(err, "livekit_pc_setup_failed")
		return nil, err
	}
	t.publisher = pub
	t.subscriber = sub
	t.localTrack = localTrack
	t.localCID = "voiceblender-audio-" + uuid.New().String()

	go t.signalLoop()
	go t.sendLoop()

	// AddTrack request kicks off the publish flow once the subscriber PC
	// completes its initial offer/answer with the server.
	if err := sc.AddTrack(&livekit.AddTrackRequest{
		Cid:           t.localCID,
		Name:          "voice",
		Type:          livekit.TrackType_AUDIO,
		Source:        livekit.TrackSource_MICROPHONE,
		AudioFeatures: []livekit.AudioTrackFeature{livekit.AudioTrackFeature_TF_NO_DTX},
	}); err != nil {
		t.cleanup(err, "livekit_add_track_failed")
		return nil, err
	}

	log.Info("livekit transport connected",
		"room", join.Room.GetName(),
		"identity", join.Participant.GetIdentity(),
	)
	return t, nil
}

// dialSignal is the injection point tests use to substitute a fake
// signaling client; production code calls the real signal.Connect.
var dialSignal = func(ctx context.Context, cfg SignalConfig) (*SignalClient, error) {
	return Connect(ctx, cfg)
}

// buildPeerConnections wires up the publisher + subscriber pion PCs and
// the local audio track. The MediaEngine registers Opus at PT 111 (the
// LiveKit-conventional payload type for audio).
func (t *Transport) buildPeerConnections(serverICE []*livekit.ICEServer) (*webrtc.PeerConnection, *webrtc.PeerConnection, *webrtc.TrackLocalStaticSample, error) {
	ice := mergeICEServers(t.peer.ICEServers, serverICE)
	pcCfg := webrtc.Configuration{ICEServers: ice}

	se := webrtc.SettingEngine{}
	if t.peer.RTPPortMin > 0 && t.peer.RTPPortMax > 0 {
		if err := se.SetEphemeralUDPPortRange(uint16(t.peer.RTPPortMin), uint16(t.peer.RTPPortMax)); err != nil {
			return nil, nil, nil, fmt.Errorf("set port range: %w", err)
		}
	}

	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, nil, nil, fmt.Errorf("register opus: %w", err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))

	publisher, err := api.NewPeerConnection(pcCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("new publisher PC: %w", err)
	}
	subscriber, err := api.NewPeerConnection(pcCfg)
	if err != nil {
		_ = publisher.Close()
		return nil, nil, nil, fmt.Errorf("new subscriber PC: %w", err)
	}

	localTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "voiceblender",
	)
	if err != nil {
		_ = publisher.Close()
		_ = subscriber.Close()
		return nil, nil, nil, fmt.Errorf("new local track: %w", err)
	}
	if _, err := publisher.AddTransceiverFromTrack(localTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	}); err != nil {
		_ = publisher.Close()
		_ = subscriber.Close()
		return nil, nil, nil, fmt.Errorf("add transceiver: %w", err)
	}

	publisher.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = t.signal.SendTrickle(c.ToJSON(), livekit.SignalTarget_PUBLISHER)
	})
	subscriber.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = t.signal.SendTrickle(c.ToJSON(), livekit.SignalTarget_SUBSCRIBER)
	})
	subscriber.OnTrack(t.handleRemoteTrack)

	publisher.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		t.log.Debug("publisher PC state", "state", s.String())
		if s == webrtc.PeerConnectionStateFailed {
			t.cleanup(errors.New("publisher PC failed"), "livekit_publisher_failed")
		}
	})
	subscriber.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		t.log.Debug("subscriber PC state", "state", s.String())
		if s == webrtc.PeerConnectionStateFailed {
			t.cleanup(errors.New("subscriber PC failed"), "livekit_subscriber_failed")
		}
	})

	publisher.OnNegotiationNeeded(t.onPublisherNegotiationNeeded)

	return publisher, subscriber, localTrack, nil
}

// signalLoop consumes typed events from the signaling client and reacts.
// Runs until the signaling client closes or we hit a fatal error.
func (t *Transport) signalLoop() {
	defer t.cleanup(nil, "livekit_signal_loop_exit")
	for {
		select {
		case <-t.ctx.Done():
			return
		case ev, ok := <-t.signal.Events():
			if !ok {
				return
			}
			t.dispatchSignal(ev)
		}
	}
}

func (t *Transport) dispatchSignal(ev SignalEvent) {
	switch e := ev.(type) {
	case SignalEventOffer:
		// Server offered on the subscriber PC. Answer.
		if err := t.subscriber.SetRemoteDescription(e.SDP); err != nil {
			t.log.Error("subscriber SetRemoteDescription", "error", err)
			t.cleanup(err, "livekit_set_remote_desc_failed")
			return
		}
		answer, err := t.subscriber.CreateAnswer(nil)
		if err != nil {
			t.log.Error("subscriber CreateAnswer", "error", err)
			t.cleanup(err, "livekit_create_answer_failed")
			return
		}
		if err := t.subscriber.SetLocalDescription(answer); err != nil {
			t.log.Error("subscriber SetLocalDescription", "error", err)
			t.cleanup(err, "livekit_set_local_desc_failed")
			return
		}
		if err := t.signal.SendAnswer(answer); err != nil {
			t.log.Error("send answer", "error", err)
			t.cleanup(err, "livekit_signal_send_failed")
			return
		}

	case SignalEventAnswer:
		// Server answered our publisher offer.
		if err := t.publisher.SetRemoteDescription(e.SDP); err != nil {
			t.log.Error("publisher SetRemoteDescription", "error", err)
			t.cleanup(err, "livekit_set_publisher_remote_failed")
			return
		}
		t.mu.Lock()
		t.publisherNegotiating = false
		t.mu.Unlock()

	case SignalEventTrickle:
		switch e.Target {
		case livekit.SignalTarget_PUBLISHER:
			if err := t.publisher.AddICECandidate(e.Candidate); err != nil {
				t.log.Debug("publisher AddICECandidate", "error", err)
			}
		case livekit.SignalTarget_SUBSCRIBER:
			if err := t.subscriber.AddICECandidate(e.Candidate); err != nil {
				t.log.Debug("subscriber AddICECandidate", "error", err)
			}
		}

	case SignalEventTrackPublished:
		// Server confirmed our local AddTrack. Bind SID.
		if e.CID == t.localCID && e.Track != nil {
			t.localSID.Store(e.Track.GetSid())
			t.log.Debug("local track published", "sid", e.Track.GetSid())
		}

	case SignalEventTrackUnpublished:
		t.mixer.RemoveLane(e.TrackSID)
		if t.cb.OnTrackUnpublishedSID != nil {
			t.cb.OnTrackUnpublishedSID(e.TrackSID)
		}

	case SignalEventParticipantUpdate:
		if t.cb.OnParticipantsUpdated != nil {
			t.cb.OnParticipantsUpdated(e.Participants)
		}

	case SignalEventSpeakersChanged:
		if t.cb.OnSpeakersChanged != nil {
			t.cb.OnSpeakersChanged(e.Speakers)
		}

	case SignalEventConnectionQuality:
		if t.cb.OnConnectionQuality != nil {
			t.cb.OnConnectionQuality(e.Updates)
		}

	case SignalEventRefreshToken:
		if t.cb.OnRefreshToken != nil {
			t.cb.OnRefreshToken(e.Token)
		}

	case SignalEventLeave:
		t.cleanup(nil, leaveReasonString(e.Reason))
	}
}

// onPublisherNegotiationNeeded is invoked by pion when the publisher PC's
// transceivers change (e.g. when we attach our local track). Generates an
// offer and ships it via signaling.
func (t *Transport) onPublisherNegotiationNeeded() {
	t.mu.Lock()
	if t.publisherNegotiating {
		t.mu.Unlock()
		return
	}
	t.publisherNegotiating = true
	t.mu.Unlock()

	offer, err := t.publisher.CreateOffer(nil)
	if err != nil {
		t.log.Error("publisher CreateOffer", "error", err)
		t.mu.Lock()
		t.publisherNegotiating = false
		t.mu.Unlock()
		return
	}
	if err := t.publisher.SetLocalDescription(offer); err != nil {
		t.log.Error("publisher SetLocalDescription", "error", err)
		t.mu.Lock()
		t.publisherNegotiating = false
		t.mu.Unlock()
		return
	}
	if err := t.signal.SendOffer(offer); err != nil {
		t.log.Error("send publisher offer", "error", err)
	}
}

// handleRemoteTrack is invoked by pion when the subscriber PC discovers a
// new remote audio track. Spawns a decoder goroutine that feeds the
// per-track lane in the remote sum-mixer.
func (t *Transport) handleRemoteTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	mime := track.Codec().MimeType
	if mime != webrtc.MimeTypeOpus {
		t.log.Debug("remote non-opus track ignored", "mime", mime, "sid", track.ID())
		return
	}
	dec, err := codec.NewOpusDecoder()
	if err != nil {
		t.log.Error("opus decoder", "error", err)
		return
	}
	sid := track.ID() // pion exposes the LiveKit track SID here
	writer := t.mixer.AddLane(sid, track.StreamID())
	t.log.Debug("remote audio track subscribed", "sid", sid)

	go func() {
		defer t.mixer.RemoveLane(sid)
		buf := make([]byte, 1500)
		for {
			if t.ctx.Err() != nil {
				return
			}
			n, _, rerr := track.Read(buf)
			if rerr != nil {
				return
			}
			pkt := &rtp.Packet{}
			if err := pkt.Unmarshal(buf[:n]); err != nil {
				continue
			}
			samples, derr := dec.Decode(pkt.Payload)
			if derr != nil || len(samples) == 0 {
				continue
			}
			pcm := int16ToBytes(samples)
			_, _ = writer.Write(pcm)
		}
	}()
}

// sendLoop reads PCM16-LE bytes from the leg's AudioWriter (via outPCM),
// chunks into 20 ms frames, encodes Opus, and publishes to the local
// track. Pion drives the RTP packaging; we only supply samples.
func (t *Transport) sendLoop() {
	frameBytes := t.cfg.FrameBytesPCM()
	frameDur := time.Duration(t.cfg.FrameMs) * time.Millisecond
	pcm := make([]byte, frameBytes)
	silence := make([]byte, frameBytes)
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
		}

		// Non-blocking pull from outPCM. If a full frame is ready, use
		// it; otherwise emit Opus silence (encoded zeros). The sendLoop
		// cadence must not drift waiting on slow PCM input — the
		// publisher PC's congestion control wants paced packets.
		n, err := readFull(t.outPCM, pcm)
		if err != nil && err != io.EOF {
			t.log.Debug("outPCM read", "error", err)
			return
		}
		var frame []byte
		if n == frameBytes {
			frame = pcm
		} else {
			frame = silence
		}
		samples := bytesToInt16(frame)
		encoded, err := t.encoder.Encode(samples)
		if err != nil {
			t.log.Warn("opus encode", "error", err)
			continue
		}
		if err := t.localTrack.WriteSample(media.Sample{Data: encoded, Duration: frameDur}); err != nil {
			t.log.Debug("WriteSample", "error", err)
			// Track writes can fail transiently before the publisher PC
			// is connected; keep going until ctx cancels.
		}
	}
}

// readFull tries to drain n bytes from r non-blockingly. Returns however
// many bytes were available; never blocks if the reader is empty.
func readFull(r io.Reader, p []byte) (int, error) {
	// streamBuffer's Read blocks if not enough data is available. For the
	// sendLoop we want non-blocking: peek under the lock first. Since
	// streamBuffer doesn't expose a peek API, we use a short-deadline
	// goroutine pattern with a buffered channel... actually simpler: use
	// a separate non-blocking helper on *streamBuffer.
	if sb, ok := r.(*streamBuffer); ok {
		return sb.tryRead(p)
	}
	return r.Read(p)
}

// AudioReader is the sum-mixed PCM16-LE stream from all LiveKit remote
// participants, paced at FrameMs cadence. Returned reader is owned by
// the transport; do not close it from the caller.
func (t *Transport) AudioReader() io.Reader { return t.mixer.Output() }

// AudioWriter accepts PCM16-LE bytes at 48 kHz. Bytes are chunked, Opus
// encoded, and published to LiveKit.
func (t *Transport) AudioWriter() io.Writer { return t.outPCM }

// Done is closed when the transport disconnects.
func (t *Transport) Done() <-chan struct{} { return t.done }

// Err returns the disconnect error, or nil for clean shutdown.
func (t *Transport) Err() error {
	if p := t.closeErr.Load(); p != nil {
		return *p
	}
	return nil
}

// CloseReason returns the short tag describing why the transport closed.
func (t *Transport) CloseReason() string {
	if p := t.closeReason.Load(); p != nil {
		return *p
	}
	return ""
}

// LocalIdentity returns the participant identity from the JoinResponse —
// useful for the leg's Headers() map.
func (t *Transport) LocalIdentity() string {
	if t.signal == nil || t.signal.JoinResponse() == nil {
		return ""
	}
	return t.signal.JoinResponse().Participant.GetIdentity()
}

// RoomName returns the LK room name from JoinResponse.
func (t *Transport) RoomName() string {
	if t.signal == nil || t.signal.JoinResponse() == nil {
		return ""
	}
	return t.signal.JoinResponse().Room.GetName()
}

// Close shuts down the transport (signaling Leave + PC tear-down).
// Idempotent.
func (t *Transport) Close(reason livekit.DisconnectReason) error {
	t.cleanup(nil, leaveReasonString(reason))
	if t.signal != nil {
		_ = t.signal.Close(reason)
	}
	return nil
}

// cleanup is the one-shot teardown path: cancels the context, closes
// PCs, closes mixer, marks Done(). Idempotent.
func (t *Transport) cleanup(err error, reason string) {
	t.closeOnce.Do(func() {
		if err != nil {
			t.closeErr.Store(&err)
		}
		if reason != "" {
			t.closeReason.Store(&reason)
		}
		t.cancel()
		if t.publisher != nil {
			_ = t.publisher.Close()
		}
		if t.subscriber != nil {
			_ = t.subscriber.Close()
		}
		if t.mixer != nil {
			t.mixer.Close()
		}
		if t.outPCM != nil {
			t.outPCM.Close()
		}
		t.closed.Store(true)
		close(t.done)
	})
}

// mergeICEServers prefers operator-supplied ICE config; falls back to the
// server-supplied list. (LiveKit deployments typically provide their own
// TURN servers in JoinResponse.IceServers.)
func mergeICEServers(operator []webrtc.ICEServer, server []*livekit.ICEServer) []webrtc.ICEServer {
	if len(operator) > 0 {
		return operator
	}
	out := make([]webrtc.ICEServer, 0, len(server))
	for _, s := range server {
		out = append(out, webrtc.ICEServer{
			URLs:       s.GetUrls(),
			Username:   s.GetUsername(),
			Credential: s.GetCredential(),
		})
	}
	return out
}

// int16ToBytes converts an int16 slice to PCM16-LE bytes.
func int16ToBytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

// bytesToInt16 reverses int16ToBytes.
func bytesToInt16(p []byte) []int16 {
	out := make([]int16, len(p)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(p[i*2:]))
	}
	return out
}
