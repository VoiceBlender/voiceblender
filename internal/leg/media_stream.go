package leg

import (
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/jitter"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// primaryStreamID is the stream identifier of the m-line every SIP dialog has.
// Keeping it stable and separate from a minted mid means single-stream legs
// address their media exactly as they did before multi-stream existed.
const primaryStreamID = "0"

// mediaStream owns everything belonging to one negotiated m=audio section: its
// RTP socket, codec, frame queues and read/write/pop loops. A single-stream leg
// has exactly one, reachable as SIPLeg.prim.
//
// Fields are written from the leg's setup paths and read by the loops. The tap
// writers and the direction fields are guarded by the owning leg's mutex; the
// receive statistics have their own.
type mediaStream struct {
	// id addresses the stream in the API; mid is its SDP a=mid token. index is
	// the m-line position, which is fixed for the life of the dialog.
	id      string
	mid     string
	index   int
	primary bool

	label   string
	content string
	lang    string

	// desiredDir is the direction the application asked for and survives
	// hold/unhold; negotiatedDir is what offer/answer actually settled on.
	// Collapsing the two would make unhold resurrect a sendonly stream as
	// sendrecv.
	desiredDir    string
	negotiatedDir string

	rtpSess   *sipmod.RTPSession
	codecType codec.CodecType
	rtpPT     uint8 // RTP payload type we receive on (echoed in our SDP)
	// rtpSendPT is the PT used when sending RTP. For dynamic codecs where the
	// peer picks its own PT (AMR-WB), this is the remote PT and differs from
	// rtpPT; 0 means "use rtpPT" (the symmetric case for all other codecs).
	rtpSendPT uint8

	// dtmfSendPT is the telephone-event PT to transmit on (the PT the remote
	// advertised); 0 means the default 101. dtmfClockRate is the negotiated
	// telephone-event clock rate — typically the rate the peer advertised, not
	// necessarily the audio codec's rate (phones like Fanvil pin
	// telephone-event at 8 kHz with an AMR-WB body). 0 means the default 8kHz.
	dtmfSendPT    uint8
	dtmfClockRate int
	// dtmfRecvPTs is the set of telephone-event PTs to accept on ingress, taken
	// from the peer's SDP. A peer advertising telephone-event on any other PT
	// would otherwise have its DTMF silently dropped.
	dtmfRecvPTs map[uint8]int
	lastDTMFTS  uint32 // timestamp of last fired end-of-event (dedup RFC 4733 retransmits)
	dtmfCh      chan string

	// AMR-WB negotiated parameters (only meaningful when codecType is AMR-WB).
	amrwbOctetAligned bool
	amrwbMode         int    // transmit mode (config ceiling clamped to peer mode-set)
	amrwbModeSet      string // peer's negotiated mode-set, echoed in our answer ("" = none)

	amrnbOctetAligned bool
	amrnbMode         int
	amrnbModeSet      string

	encoder   codec.Encoder
	decoder   codec.Decoder
	inFrames  chan []byte // decoded native-rate PCM from readLoop (or jitter-buffer popLoop)
	outFrames chan []byte // native-rate PCM to encode in writeLoop

	// Optional ingress jitter buffer. When non-nil, readLoop pushes decoded PCM
	// into jb keyed by RTP sequence number, and popLoop drains jb at a fixed
	// cadence into inFrames. When nil, readLoop pushes directly to inFrames
	// (passthrough — zero added latency, no reordering).
	jb           *jitter.Buffer
	jbTargetMs   int // target delay in ms; 0 = disabled
	jbMaxMs      int // max queue depth in ms
	jbFrameBytes int // native-rate 20ms frame size in bytes (silence size on underrun)

	inTap       io.Writer // copy of decoded incoming PCM (before inFrames)
	outTap      io.Writer // copy of outgoing PCM (from writeLoop, including silence)
	amdTap      io.Writer // separate from inTap so recording and AMD can coexist
	speakingTap io.Writer // decoded incoming PCM for voice activity detection

	// Inbound RTP stream statistics for MOS calculation.
	rtpStatsMu     sync.Mutex
	rtpReceived    uint32
	rtpFirstSeq    uint16
	rtpLastSeq     uint16
	rtpHasFirst    bool
	rtpJitter      float64 // running jitter in RTP clock units (RFC 3550 §A.8)
	rtpLastTransit int64   // last transit time in RTP clock units

	// roomID is the room this stream's audio is mixed into, which need not be
	// the leg's own room. Empty means detached.
	roomID string
	role   string
}

// newPrimaryStream returns the stream every SIP leg starts with. It occupies
// m-line 0 and carries the leg's DTMF, AMD and speaking-detection taps.
func newPrimaryStream() *mediaStream {
	return &mediaStream{
		id:         primaryStreamID,
		index:      0,
		primary:    true,
		desiredDir: sipmod.DirSendRecv,
	}
}

// initPrimaryStream allocates the leg's m-line-0 stream and registers it. Every
// constructor calls it before any media state is touched, so l.prim is never
// nil for the life of the leg.
func (l *SIPLeg) initPrimaryStream() {
	l.prim = newPrimaryStream()
	l.streams = []*mediaStream{l.prim}
	l.applyEngineGates()
}

// StreamMedia describes one negotiated audio stream's media endpoints, so a
// room can mix it without knowing how the leg is put together.
//
// Reader is nil when the stream does not receive from the peer, Writer nil when
// it does not transmit; a fully inactive stream has neither.
type StreamMedia struct {
	ID         string
	MID        string
	Primary    bool
	SampleRate int
	Direction  string // negotiated direction, from our point of view
	Reader     io.Reader
	Writer     io.Writer
}

// StreamMedia returns the media endpoints of one of the leg's audio streams.
func (l *SIPLeg) StreamMedia(streamID string) (StreamMedia, bool) {
	s, ok := l.streamByID(streamID)
	if !ok {
		return StreamMedia{}, false
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	sm := StreamMedia{
		ID:         s.id,
		MID:        s.mid,
		Primary:    s.primary,
		SampleRate: s.codecType.SampleRate(),
		Direction:  s.negotiatedDir,
	}
	if s.receives() && s.inFrames != nil {
		sm.Reader = &sipReader{frames: s.inFrames, ctx: l.ctx}
	}
	if s.sends() && s.outFrames != nil {
		sm.Writer = &sipWriter{frames: s.outFrames, ctx: l.ctx}
	}
	return sm, true
}

// SecondaryStreamIDs returns the IDs of every stream other than the primary,
// in m-line order.
func (l *SIPLeg) SecondaryStreamIDs() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []string
	for _, s := range l.streams {
		if !s.primary {
			out = append(out, s.id)
		}
	}
	return out
}

// StreamRooms returns the room each secondary stream is attached to, keyed by
// stream ID. The primary stream is excluded — its room is the leg's own
// RoomID, which every existing caller already tracks.
func (l *SIPLeg) StreamRooms() map[string]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out map[string]string
	for _, s := range l.streams {
		if s.primary || s.roomID == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[s.id] = s.roomID
	}
	return out
}

// SetStreamRole records a secondary stream's routing role inside its room.
func (l *SIPLeg) SetStreamRole(streamID, role string) {
	s, ok := l.streamByID(streamID)
	if !ok || s.primary {
		return
	}
	l.mu.Lock()
	s.role = role
	l.mu.Unlock()
}

// SetStreamRoom records which room a secondary stream is mixed into.
func (l *SIPLeg) SetStreamRoom(streamID, roomID string) {
	s, ok := l.streamByID(streamID)
	if !ok || s.primary {
		return
	}
	l.mu.Lock()
	s.roomID = roomID
	l.mu.Unlock()
}

// StreamCount returns how many audio streams the leg is currently carrying.
func (l *SIPLeg) StreamCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.streams)
}

// streamByID returns the stream with the given API identifier.
func (l *SIPLeg) streamByID(id string) (*mediaStream, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.streams {
		if s.id == id {
			return s, true
		}
	}
	return nil, false
}

// audioStreams returns a snapshot of the leg's streams in m-line order.
func (l *SIPLeg) audioStreams() []*mediaStream {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*mediaStream, len(l.streams))
	copy(out, l.streams)
	return out
}

// negotiateInboundAnswer plans and materializes the answer to the inbound
// offer, returning the SDP body to send in a 183 or 200 OK.
func (l *SIPLeg) negotiateInboundAnswer(preferred codec.CodecType) ([]byte, error) {
	offer := l.inbound.RemoteSDP
	if offer == nil {
		return nil, fmt.Errorf("inbound call has no SDP offer")
	}

	textIdx := offeredTextMLine(offer)
	var textPort int
	var t140PT, redPT uint8
	var textRejected bool
	if textIdx >= 0 {
		textPort, t140PT, redPT, textRejected = l.setupInboundTextMedia(offer)
	}
	if textPort == 0 {
		// We are not carrying RTT, so the section is just another one to reject.
		textIdx = -1
	}

	plans := sipmod.PlanAnswer(offer, l.answerOptions(preferred, textIdx))
	sections := l.buildAnswerStreams(offer, plans)
	if len(sipmod.AcceptedAudio(plans)) == 0 {
		return nil, fmt.Errorf("no common codec negotiated")
	}

	cfg := sipmod.SDPConfig{
		LocalIP:       l.localIP,
		Codecs:        l.supportedCodecs,
		Streams:       sections,
		TextRTPPort:   textPort,
		TextT140PT:    t140PT,
		TextREDPT:     redPT,
		RTTRedundancy: l.rttRedundancy,
	}
	return sipmod.GenerateAnswer(cfg, l.prim.codecType, l.prim.rtpPT, textRejected), nil
}

// offeredTextMLine returns the index of an m=text section we could carry, or
// -1. RTT is only accepted as the final m-line: the generator appends m=text
// after the audio sections, so accepting one in any other position would
// reorder the answer and break RFC 3264 §6 positional matching.
func offeredTextMLine(offer *sipmod.SDPMedia) int {
	for i, ml := range offer.MLines {
		if ml.Media != "text" {
			continue
		}
		if i == len(offer.MLines)-1 {
			return i
		}
		return -1
	}
	return -1
}

// applyEngineGates copies the engine's negotiation policy onto the leg, so
// negotiation never has to reach back into the engine.
func (l *SIPLeg) applyEngineGates() {
	if l.engine == nil {
		return
	}
	l.strictMLines = l.engine.StrictMLineAnswer()
}

// answerOptions builds the negotiation gates for this leg.
func (l *SIPLeg) answerOptions(preferred codec.CodecType, textMLine int) sipmod.AnswerOptions {
	return sipmod.AnswerOptions{
		SupportedCodecs: l.supportedCodecs,
		Preferred:       preferred,
		TextMLineIndex:  textMLine,
		StrictMLines:    l.strictMLines,
	}
}

// buildAnswerStreams materializes an answer plan: it allocates an RTP session
// per accepted section, configures the codec, and records every slot in the
// m-line table. The returned sections are positionally aligned with the offer,
// as RFC 3264 §6 requires.
//
// A section we cannot materialize (allocator exhausted, SetRemote failure) is
// downgraded to a port-0 rejection rather than failing the call.
func (l *SIPLeg) buildAnswerStreams(offer *sipmod.SDPMedia, plans []sipmod.SlotPlan) []sipmod.AudioStream {
	sections := make([]sipmod.AudioStream, 0, len(plans))

	for _, p := range plans {
		switch p.Action {
		case sipmod.SlotSkip, sipmod.SlotOmit:
			l.recordSlot(p, "", sipmod.SlotTombstone, sipmod.AudioStream{})
			continue
		case sipmod.SlotReject:
			sections = append(sections, sipmod.AudioStream{Port: 0, MID: p.MID})
			l.recordSlot(p, "", sipmod.SlotTombstone, sipmod.AudioStream{})
			continue
		}

		s, sec, ok := l.materializeStream(offer, p)
		if !ok {
			sections = append(sections, sipmod.AudioStream{Port: 0, MID: p.MID})
			l.recordSlot(p, "", sipmod.SlotTombstone, sipmod.AudioStream{})
			continue
		}
		sections = append(sections, sec)
		l.recordSlot(p, s.id, sipmod.SlotActive, sec)
	}

	return sections
}

// materializeStream creates (or reuses) the stream backing an accepted plan.
func (l *SIPLeg) materializeStream(offer *sipmod.SDPMedia, p sipmod.SlotPlan) (*mediaStream, sipmod.AudioStream, bool) {
	ra := &offer.Audio[p.AudioIdx]

	l.mu.Lock()
	s := l.streamForIndex(p.Index)
	l.mu.Unlock()

	if s.rtpSess == nil {
		sess, err := l.newRTPSession()
		if err != nil {
			l.log.Warn("answer: RTP session allocation failed, rejecting section",
				"leg_id", l.id, "m_line", p.Index, "error", err)
			return nil, sipmod.AudioStream{}, false
		}
		s.rtpSess = sess
	}
	if err := s.rtpSess.SetRemote(ra.RemoteIP, ra.RemotePort); err != nil {
		l.log.Warn("answer: set remote failed, rejecting section",
			"leg_id", l.id, "m_line", p.Index, "error", err)
		if !s.primary {
			s.close()
			s.rtpSess = nil
		}
		return nil, sipmod.AudioStream{}, false
	}

	l.mu.Lock()
	s.mid, s.label, s.content, s.lang = p.MID, p.Label, p.Content, p.Lang
	s.codecType = p.Codec
	s.rtpPT = p.PT
	s.negotiatedDir = p.Direction
	s.desiredDir = p.Direction
	l.configureAMRWB(s, offer, p.PT)
	l.configureAMRNB(s, offer, p.PT)
	l.configureDTMF(s, offer)
	l.mu.Unlock()

	return s, sipmod.AudioStream{
		Port:              s.rtpSess.LocalPort(),
		Direction:         p.Direction,
		Codecs:            []codec.CodecType{p.Codec},
		CodecPTs:          map[codec.CodecType]uint8{p.Codec: p.PT},
		DTMFPT:            s.dtmfSendPT,
		DTMFClockRate:     s.dtmfClockRate,
		OfferTE48k:        p.Codec == codec.CodecOpus,
		AMRWBOctetAligned: s.amrwbOctetAligned,
		AMRWBModeSet:      s.amrwbModeSet,
		AMRNBOctetAligned: s.amrnbOctetAligned,
		AMRNBModeSet:      s.amrnbModeSet,
		MID:               s.mid,
		Label:             s.label,
		Content:           s.content,
		Lang:              s.lang,
	}, true
}

// newRTPSession binds a media socket, drawing from the engine's port pool when
// there is one and letting the OS assign otherwise.
func (l *SIPLeg) newRTPSession() (*sipmod.RTPSession, error) {
	if l.engine == nil {
		return sipmod.NewRTPSession()
	}
	return sipmod.NewRTPSessionFromAllocator(l.engine.PortAllocator())
}

// streamForIndex returns the stream occupying an m-line, creating it if this is
// the first time the dialog has used that slot. Caller holds l.mu.
func (l *SIPLeg) streamForIndex(index int) *mediaStream {
	for _, s := range l.streams {
		if s.index == index {
			return s
		}
	}
	s := &mediaStream{
		id:    strconv.Itoa(index),
		index: index,
	}
	l.streams = append(l.streams, s)
	return s
}

// recordSlot writes an answered m-line into the dialog's slot table, extending
// it when the peer offered a position we have not seen before.
func (l *SIPLeg) recordSlot(p sipmod.SlotPlan, streamID string, state sipmod.SlotState, local sipmod.AudioStream) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for l.mlines.Len() <= p.Index {
		l.mlines.Append(sipmod.MLineSlot{Media: p.Media, Proto: p.Proto})
	}
	slot := l.mlines.Slot(p.Index)
	slot.Media = p.Media
	slot.Proto = p.Proto
	slot.MID = p.MID
	slot.Label = p.Label
	slot.Content = p.Content
	slot.Lang = p.Lang
	slot.State = state
	slot.StreamID = streamID
	slot.Local = local
	l.mlines.ReserveMID(p.MID)
	if state == sipmod.SlotTombstone {
		slot.Local.Port = 0
	}
}

// adoptOutboundStreams zips the peer's answer onto the audio sections we
// offered. Sections the peer disabled with port 0 are torn down and tombstoned;
// an answer shorter than the offer is treated as rejecting the missing tail,
// which some devices do rather than echoing every section.
//
// Returns false when not even the primary stream survived.
func (l *SIPLeg) adoptOutboundStreams(call *sipmod.OutboundCall) bool {
	if len(call.OfferedStreams) <= 1 {
		return false // legacy single-stream path handles itself
	}

	if n := len(call.RemoteSDP.Audio); n < len(call.OfferedStreams) {
		l.log.Warn("answer carries fewer audio sections than offered; treating the tail as rejected",
			"leg_id", l.id, "offered", len(call.OfferedStreams), "answered", n)
	}

	primaryUp := false
	for i, os := range call.OfferedStreams {
		sess := call.RTPSess
		if i > 0 {
			if i-1 >= len(call.ExtraRTPSess) {
				break
			}
			sess = call.ExtraRTPSess[i-1]
		}

		var ra *sipmod.RemoteAudioStream
		if i < len(call.RemoteSDP.Audio) {
			ra = &call.RemoteSDP.Audio[i]
		}

		if ra == nil || ra.RemotePort == 0 {
			sess.Close()
			l.recordOfferedSlot(i, os, "", sipmod.SlotTombstone)
			l.log.Info("peer rejected offered audio stream",
				"leg_id", l.id, "m_line", i, "mid", os.MID)
			continue
		}

		selected, pt, ok := sipmod.NegotiateCodecStream(ra, l.supportedCodecs, codec.CodecUnknown)
		if !ok {
			sess.Close()
			l.recordOfferedSlot(i, os, "", sipmod.SlotTombstone)
			continue
		}
		if err := sess.SetRemote(ra.RemoteIP, ra.RemotePort); err != nil {
			sess.Close()
			l.recordOfferedSlot(i, os, "", sipmod.SlotTombstone)
			continue
		}

		l.mu.Lock()
		s := l.streamForIndex(i)
		s.rtpSess = sess
		s.mid, s.label, s.content, s.lang = os.MID, os.Label, os.Content, os.Lang
		s.codecType = selected
		// As the offerer we receive on our own payload type; configureAMRWB
		// resets the send PT from the answer for codecs that renumber.
		s.rtpPT = selected.PayloadType()
		s.desiredDir = os.Direction
		s.negotiatedDir = sipmod.MirrorDirection(ra.EffectiveDirection())
		l.configureAMRWB(s, call.RemoteSDP, pt)
		l.configureAMRNB(s, call.RemoteSDP, pt)
		l.configureDTMF(s, call.RemoteSDP)
		l.mu.Unlock()

		l.recordOfferedSlot(i, os, s.id, sipmod.SlotActive)
		if i == 0 {
			primaryUp = true
		}
	}

	if primaryUp {
		l.startAcceptedStreams()
	}
	return primaryUp
}

// recordOfferedSlot writes an offered m-line into the dialog's slot table.
func (l *SIPLeg) recordOfferedSlot(index int, os sipmod.AudioStream, streamID string, state sipmod.SlotState) {
	local := os
	if state == sipmod.SlotTombstone {
		local = sipmod.AudioStream{}
	}
	l.recordSlot(sipmod.SlotPlan{
		Index:   index,
		Media:   "audio",
		Proto:   []string{"RTP", "AVP"},
		MID:     os.MID,
		Label:   os.Label,
		Content: os.Content,
		Lang:    os.Lang,
	}, streamID, state, local)
}

// closeAllStreams releases every stream's RTP socket. Used when answering fails
// after ports were already allocated.
func (l *SIPLeg) closeAllStreams() {
	for _, s := range l.audioStreams() {
		s.close()
	}
}

// startAcceptedStreams brings up the media pipeline for every stream that the
// answer accepted and that is not already running.
func (l *SIPLeg) startAcceptedStreams() {
	for _, s := range l.audioStreams() {
		if s.rtpSess == nil || s.encoder != nil || s.decoder != nil {
			continue
		}
		l.setupStreamMedia(s)
	}
}

// sends reports whether the negotiated direction lets us transmit. A stream
// that only receives must not run a writeLoop (RFC 3264 §6.1).
func (s *mediaStream) sends() bool {
	switch s.negotiatedDir {
	case sipmod.DirRecvOnly, sipmod.DirInactive:
		return false
	default:
		return true
	}
}

// receives reports whether the negotiated direction lets us receive media.
func (s *mediaStream) receives() bool {
	switch s.negotiatedDir {
	case sipmod.DirSendOnly, sipmod.DirInactive:
		return false
	default:
		return true
	}
}

// acceptsDTMFPT reports whether an ingress RTP payload type is a negotiated
// telephone-event. Absent negotiation it falls back to the conventional PTs so
// behaviour matches what the leg did before the PTs were tracked.
func (s *mediaStream) acceptsDTMFPT(pt uint8) bool {
	if len(s.dtmfRecvPTs) == 0 {
		return pt == 100 || pt == 101
	}
	_, ok := s.dtmfRecvPTs[pt]
	return ok
}

// close releases the stream's RTP socket, returning its port to the allocator.
func (s *mediaStream) close() {
	if s.rtpSess != nil {
		s.rtpSess.Close()
	}
}
