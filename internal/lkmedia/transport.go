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

// pendingTrack records a remote audio track whose participant identity
// is not yet known. Once a ParticipantUpdate carries the matching SID we
// fire OnRemoteAudioTrack with the cached pcm reader.
type pendingTrack struct {
	pcm      io.Reader
	trackSID string
}

// PeerConfig configures the pion PeerConnections used by the transport.
type PeerConfig struct {
	// ICEServers overrides the STUN/TURN list from JoinResponse. When non-empty,
	// VoiceBlender uses these instead of the server-supplied list.
	ICEServers []webrtc.ICEServer
	// RTPPortMin/RTPPortMax constrain pion's ephemeral UDP port range.
	// Both zero means use pion defaults.
	RTPPortMin, RTPPortMax int
}

// Callbacks bundles the hooks the API layer wires up to bridge LK
// signaling state into VoiceBlender's leg/room model. Model B: each
// subscribed remote audio track becomes a LiveKitParticipantLeg via
// OnRemoteAudioTrack; the rest are observability events.
type Callbacks struct {
	// OnRemoteAudioTrack is the central callback for Model B. Fires once
	// per remote audio track the subscriber PC subscribes to. The API
	// layer is expected to spawn a LiveKitParticipantLeg using `pcm` as
	// the leg's AudioReader and add it to the umbrella's VB room. The
	// reader yields PCM16-LE at 48 kHz; reads return io.EOF when the
	// track ends or the transport closes.
	//
	// `identity` may be empty if pion fires OnTrack before the
	// corresponding ParticipantUpdate arrives. In that case the callback
	// also receives `participantSID` so callers can map it later.
	OnRemoteAudioTrack func(identity, participantSID, trackSID string, pcm io.Reader)

	// OnRemoteAudioTrackEnded fires when a previously-published track is
	// torn down (TrackUnpublished, ParticipantUpdate DISCONNECTED, or the
	// underlying RTP stream stops). The API layer cleans up the matching
	// participant leg.
	OnRemoteAudioTrackEnded func(trackSID string)

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
	cb     atomic.Pointer[Callbacks]

	publisher  *webrtc.PeerConnection
	subscriber *webrtc.PeerConnection

	localTrack *webrtc.TrackLocalStaticSample
	localCID   string
	localSID   atomic.Value // string; set when server confirms via TrackPublished

	outPCM *streamBuffer // PCM the publish leg writes; sendLoop drains it.

	// pendingTracks holds remote audio tracks that arrived before we
	// learned their participant identity. Keyed by participantSID; the
	// value is the pcmReader passed to OnRemoteAudioTrack once the
	// matching ParticipantUpdate arrives. Guarded by mu.
	pendingTracks map[string]pendingTrack

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

	// participants tracks the LK room's current participants, indexed by
	// identity (preferred over SID because identity is durable across the
	// participant's lifecycle). Updated when the server sends
	// ParticipantUpdate; the API surface (GET .../participants) reads
	// from this snapshot.
	participants map[string]*livekit.ParticipantInfo
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

	tctx, cancel := context.WithCancel(context.Background())
	t := &Transport{
		cfg:           cfg,
		peer:          peer,
		log:           log,
		signal:        sc,
		outPCM:        newStreamBuffer(cfg.IngressBufferBytes(), cfg.FrameMs),
		encoder:       enc,
		ctx:           tctx,
		cancel:        cancel,
		done:          make(chan struct{}),
		pendingTracks: map[string]pendingTrack{},
		participants:  map[string]*livekit.ParticipantInfo{},
	}
	t.cb.Store(&cb)

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
		if cb := t.callbacks(); cb != nil {
			if cb.OnRemoteAudioTrackEnded != nil {
				cb.OnRemoteAudioTrackEnded(e.TrackSID)
			}
			if cb.OnTrackUnpublishedSID != nil {
				cb.OnTrackUnpublishedSID(e.TrackSID)
			}
		}

	case SignalEventParticipantUpdate:
		t.mergeParticipantUpdate(e.Participants)
		t.drainPendingTracksFor(e.Participants)
		if cb := t.callbacks(); cb != nil && cb.OnParticipantsUpdated != nil {
			cb.OnParticipantsUpdated(e.Participants)
		}

	case SignalEventSpeakersChanged:
		if cb := t.callbacks(); cb != nil && cb.OnSpeakersChanged != nil {
			cb.OnSpeakersChanged(e.Speakers)
		}

	case SignalEventConnectionQuality:
		if cb := t.callbacks(); cb != nil && cb.OnConnectionQuality != nil {
			cb.OnConnectionQuality(e.Updates)
		}

	case SignalEventRefreshToken:
		if cb := t.callbacks(); cb != nil && cb.OnRefreshToken != nil {
			cb.OnRefreshToken(e.Token)
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

// handleRemoteTrack is invoked by pion when the subscriber PC discovers
// a new remote audio track. Spawns a decoder goroutine that produces
// PCM16-LE bytes on an io.Pipe, then either fires OnRemoteAudioTrack
// immediately (if we already know the participant) or stashes the reader
// in pendingTracks (drained by the next matching ParticipantUpdate).
//
// Model B: each remote audio track becomes its own LiveKitParticipantLeg
// in the umbrella's VB room. This handler only owns the decode loop; the
// API layer constructs and registers the leg from the callback.
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
	trackSID := track.ID()             // pion exposes the LiveKit track SID here
	participantSID := track.StreamID() // pion's stream ID = LK participant SID (PA_*)

	pcmReader, pcmWriter := io.Pipe()
	go func() {
		defer pcmWriter.Close()
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
			if _, werr := pcmWriter.Write(int16ToBytes(samples)); werr != nil {
				return
			}
		}
	}()

	// Best-effort identity lookup: scan the cached participants for a
	// match on participantSID. If the participant arrived first we can
	// fire the callback immediately; otherwise stash for later.
	identity := t.identityForSID(participantSID)
	if identity != "" {
		t.fireRemoteAudioTrack(identity, participantSID, trackSID, pcmReader)
		return
	}
	t.mu.Lock()
	t.pendingTracks[participantSID] = pendingTrack{pcm: pcmReader, trackSID: trackSID}
	t.mu.Unlock()
	t.log.Debug("remote audio track buffered pending ParticipantUpdate",
		"participant_sid", participantSID, "track_sid", trackSID)
}

// identityForSID is a snapshot lookup of the participants cache.
func (t *Transport) identityForSID(sid string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ident, p := range t.participants {
		if p.GetSid() == sid {
			return ident
		}
	}
	return ""
}

// fireRemoteAudioTrack delivers a (identity, participantSID, trackSID, pcm)
// tuple to the OnRemoteAudioTrack callback if one is registered. The
// callback runs on its own goroutine so a slow callback cannot block
// pion's OnTrack handler or the signaling loop.
func (t *Transport) fireRemoteAudioTrack(identity, participantSID, trackSID string, pcm io.Reader) {
	cb := t.callbacks()
	if cb == nil || cb.OnRemoteAudioTrack == nil {
		// Without a callback the PCM has nowhere to go — drain to avoid
		// stalling the decoder pipe.
		go io.Copy(io.Discard, pcm)
		return
	}
	go cb.OnRemoteAudioTrack(identity, participantSID, trackSID, pcm)
}

// drainPendingTracksFor scans the participants cache for SIDs we have
// pending tracks for and fires the callback. Called whenever the
// participants map is updated.
func (t *Transport) drainPendingTracksFor(participants []*livekit.ParticipantInfo) {
	if len(participants) == 0 {
		return
	}
	type fired struct {
		identity, participantSID, trackSID string
		pcm                                io.Reader
	}
	var ready []fired
	t.mu.Lock()
	for _, p := range participants {
		if p == nil {
			continue
		}
		sid := p.GetSid()
		ident := p.GetIdentity()
		if sid == "" || ident == "" {
			continue
		}
		if pt, ok := t.pendingTracks[sid]; ok {
			ready = append(ready, fired{ident, sid, pt.trackSID, pt.pcm})
			delete(t.pendingTracks, sid)
		}
	}
	t.mu.Unlock()
	for _, f := range ready {
		t.fireRemoteAudioTrack(f.identity, f.participantSID, f.trackSID, f.pcm)
	}
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

// AudioWriter accepts PCM16-LE bytes at 48 kHz. The publish leg's
// AudioWriter calls land here; sendLoop drains them, Opus-encodes, and
// publishes to the local LiveKit track. Model B: remote audio is no
// longer sum-mixed by the transport — each remote LK participant is its
// own VB leg via OnRemoteAudioTrack, so there is no transport-level
// AudioReader anymore.
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

// CloseClient is a convenience for callers that don't want to depend on
// the livekit protocol enums — equivalent to Close(CLIENT_INITIATED).
func (t *Transport) CloseClient() error {
	return t.Close(livekit.DisconnectReason_CLIENT_INITIATED)
}

// SetCallbacks replaces the observability callbacks. Used by callers
// that need the leg's ID inside the callbacks but only have it after
// NewTransport returns. Thread-safe.
func (t *Transport) SetCallbacks(cb Callbacks) {
	t.cb.Store(&cb)
}

// callbacks loads the current callbacks pointer. Never nil — NewTransport
// stores a zero-value Callbacks at construction.
func (t *Transport) callbacks() *Callbacks {
	return t.cb.Load()
}

// mergeParticipantUpdate folds an incoming ParticipantUpdate into the
// per-leg cache. Disconnect entries are removed; active entries replace.
func (t *Transport) mergeParticipantUpdate(updates []*livekit.ParticipantInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range updates {
		if p == nil || p.GetIdentity() == "" {
			continue
		}
		if p.GetState() == livekit.ParticipantInfo_DISCONNECTED {
			delete(t.participants, p.GetIdentity())
			continue
		}
		t.participants[p.GetIdentity()] = p
	}
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
