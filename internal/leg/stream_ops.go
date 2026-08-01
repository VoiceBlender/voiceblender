package leg

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// StreamRequest describes an audio stream to add to a live dialog.
type StreamRequest struct {
	Direction string // "" means sendrecv
	Lang      string
	Content   string
	Label     string
}

// StreamInfo is a read-only view of one negotiated audio stream.
type StreamInfo struct {
	ID         string
	MID        string
	Index      int
	Primary    bool
	State      string
	Direction  string
	Desired    string
	Codec      string
	SampleRate int
	LocalPort  int
	RemoteAddr string
	Label      string
	Content    string
	Lang       string
	RoomID     string
	Role       string
}

// ErrStreamNotFound is returned for an unknown stream ID.
var ErrStreamNotFound = fmt.Errorf("stream not found")

// Streams returns a view of every audio stream on the leg, in m-line order.
func (l *SIPLeg) Streams() []StreamInfo {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]StreamInfo, 0, len(l.streams))
	for _, s := range l.streams {
		out = append(out, l.streamInfoLocked(s))
	}
	return out
}

// Stream returns a view of one audio stream.
func (l *SIPLeg) Stream(streamID string) (StreamInfo, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, s := range l.streams {
		if s.id == streamID {
			return l.streamInfoLocked(s), true
		}
	}
	return StreamInfo{}, false
}

// streamInfoLocked builds a view of s. Caller holds l.mu.
func (l *SIPLeg) streamInfoLocked(s *mediaStream) StreamInfo {
	info := StreamInfo{
		ID:        s.id,
		MID:       s.mid,
		Index:     s.index,
		Primary:   s.primary,
		State:     sipmod.SlotActive.String(),
		Direction: s.negotiatedDir,
		Desired:   s.desiredDir,
		Label:     s.label,
		Content:   s.content,
		Lang:      s.lang,
		Role:      s.role,
		RoomID:    s.roomID,
	}
	if s.primary {
		info.RoomID = l.roomID
	}
	if info.Direction == "" {
		info.Direction = sipmod.DirSendRecv
	}
	if s.codecType != codec.CodecUnknown {
		info.Codec = s.codecType.String()
		info.SampleRate = s.codecType.SampleRate()
	}
	if s.rtpSess != nil {
		info.LocalPort = s.rtpSess.LocalPort()
		if addr := s.rtpSess.RemoteAddr(); addr != nil {
			info.RemoteAddr = addr.String()
		}
	}
	if slot, ok := l.mlines.ByStreamID(s.id); ok {
		info.State = slot.State.String()
	}
	return info
}

// AddStream negotiates an additional m=audio section on the live dialog via a
// re-INVITE, per RFC 3264 §8.1. The new section is appended below the existing
// ones; it never reuses a tombstoned slot, so the peer's positional matching
// stays valid.
//
// A peer that rejects the new section with port 0 leaves the call untouched and
// yields an error naming the rejection.
func (l *SIPLeg) AddStream(ctx context.Context, req StreamRequest) (StreamInfo, error) {
	l.mu.RLock()
	enabled, max, count := l.multiStream, l.multiStreamMax, l.mlines.ActiveAudioCount()
	primaryUp := l.prim.rtpSess != nil
	l.mu.RUnlock()

	if !enabled {
		return StreamInfo{}, newStreamError(http.StatusConflict, "multi-stream is disabled (SIP_MULTI_STREAM_ENABLED)")
	}
	if !primaryUp {
		return StreamInfo{}, newStreamError(http.StatusConflict, "leg has no negotiated media yet")
	}
	if count >= max {
		return StreamInfo{}, newStreamError(http.StatusConflict, "leg already carries %d audio streams (cap %d)", count, max)
	}

	dialog, err := l.dialogForReInvite()
	if err != nil {
		return StreamInfo{}, err
	}

	l.mu.Lock()
	l.ensurePrimarySlotLocked()
	l.mu.Unlock()

	sess, err := l.newRTPSession()
	if err != nil {
		return StreamInfo{}, fmt.Errorf("allocate RTP session: %w", err)
	}

	direction := req.Direction
	if direction == "" {
		direction = sipmod.DirSendRecv
	}

	l.mu.Lock()
	mid := l.mlines.MintMID()
	index := l.mlines.Len()
	s := &mediaStream{
		id:            strconv.Itoa(index),
		mid:           mid,
		index:         index,
		rtpSess:       sess,
		codecType:     l.prim.codecType,
		rtpPT:         l.prim.rtpPT,
		label:         req.Label,
		content:       req.Content,
		lang:          req.Lang,
		desiredDir:    direction,
		negotiatedDir: direction,
	}
	local := sipmod.AudioStream{
		Port:      sess.LocalPort(),
		Direction: direction,
		Codecs:    l.supportedCodecs,
		MID:       mid,
		Label:     req.Label,
		Content:   req.Content,
		Lang:      req.Lang,
	}
	l.streams = append(l.streams, s)
	l.mlines.Append(sipmod.MLineSlot{
		Media: "audio", Proto: []string{"RTP", "AVP"},
		MID: mid, Label: req.Label, Content: req.Content, Lang: req.Lang,
		State: sipmod.SlotPending, StreamID: s.id, Local: local,
	})
	l.mu.Unlock()

	offer := l.offerFromTable()
	if _, err := l.engine.SendReInviteAnswer(ctx, dialog, offer, l.applyAnswer); err != nil {
		l.closeStream(s)
		return StreamInfo{}, fmt.Errorf("add-stream re-INVITE: %w", err)
	}

	// applyAnswer has zipped the peer's answer on; a port-0 section leaves the
	// slot without a live remote address.
	if s.rtpSess == nil || s.rtpSess.RemoteAddr() == nil {
		l.closeStream(s)
		return StreamInfo{}, newStreamError(http.StatusConflict, "peer rejected the additional audio stream")
	}

	l.setupStreamMedia(s)

	info, _ := l.Stream(s.id)
	return info, nil
}

// RemoveStream disables one of the leg's secondary streams with a re-INVITE
// carrying port 0 for its section (RFC 3264 §8.2). The m-line slot survives as
// a tombstone — the count never decreases for the life of the dialog.
func (l *SIPLeg) RemoveStream(ctx context.Context, streamID string) error {
	s, ok := l.streamByID(streamID)
	if !ok {
		return ErrStreamNotFound
	}
	if s.primary {
		return newStreamError(http.StatusConflict, "the primary stream carries the call and cannot be removed")
	}

	dialog, err := l.dialogForReInvite()
	if err != nil {
		return err
	}

	l.mu.Lock()
	l.ensurePrimarySlotLocked()
	l.mu.Unlock()

	// Tombstone first so the offer we build already carries port 0 for it.
	l.closeStream(s)

	offer := l.offerFromTable()
	if _, err := l.engine.SendReInviteAnswer(ctx, dialog, offer, l.applyAnswer); err != nil {
		return fmt.Errorf("remove-stream re-INVITE: %w", err)
	}
	return nil
}

// ensurePrimarySlotLocked backfills the m-line-0 slot for dialogs negotiated
// through the legacy single-stream path, which never populated the table. Without
// it a table-derived offer would omit the primary audio section entirely and a
// newly added stream would collide with it at index 0. Caller holds l.mu.
func (l *SIPLeg) ensurePrimarySlotLocked() {
	if l.mlines.Len() > 0 {
		return
	}
	local := sipmod.AudioStream{
		Direction: sipmod.DirSendRecv,
		Codecs:    []codec.CodecType{l.prim.codecType},
		CodecPTs:  map[codec.CodecType]uint8{l.prim.codecType: l.prim.rtpPT},
		// The primary section keeps no a=mid: it never had one on the wire, and
		// minting one now would change the SDP a live peer already matched.
		DTMFPT:            l.prim.dtmfSendPT,
		DTMFClockRate:     l.prim.dtmfClockRate,
		OfferTE48k:        l.prim.codecType == codec.CodecOpus,
		AMRWBOctetAligned: l.prim.amrwbOctetAligned,
		AMRWBModeSet:      l.prim.amrwbModeSet,
		AMRNBOctetAligned: l.prim.amrnbOctetAligned,
		AMRNBModeSet:      l.prim.amrnbModeSet,
	}
	if l.prim.rtpSess != nil {
		local.Port = l.prim.rtpSess.LocalPort()
	}
	l.mlines.Append(sipmod.MLineSlot{
		Media: "audio", Proto: []string{"RTP", "AVP"},
		State: sipmod.SlotActive, StreamID: l.prim.id, Local: local,
	})
}

// offerFromTable renders the dialog's whole m-line vector as an offer body,
// tombstones included, so the count and ordering the peer matched against are
// preserved.
func (l *SIPLeg) offerFromTable() []byte {
	l.mu.RLock()
	defer l.mu.RUnlock()

	cfg := sipmod.SDPConfig{
		LocalIP: l.localIP,
		Codecs:  l.supportedCodecs,
		Streams: l.mlines.LocalStreams(l.held, nil),
	}
	if l.textRtpSess != nil {
		cfg.TextRTPPort = l.textRtpSess.LocalPort()
		cfg.TextT140PT = l.textT140PT
		cfg.TextREDPT = l.textREDPT
		cfg.RTTRedundancy = l.rttRedundancy
	}
	return sipmod.GenerateOffer(cfg)
}

// dialogForReInvite returns the dialog to renegotiate on.
func (l *SIPLeg) dialogForReInvite() (interface{}, error) {
	if l.engine == nil {
		return nil, newStreamError(http.StatusConflict, "leg has no SIP engine")
	}
	if l.inbound != nil {
		return l.inbound.Dialog, nil
	}
	if l.outbound != nil {
		return l.outbound.Dialog, nil
	}
	return nil, newStreamError(http.StatusConflict, "no dialog available")
}

// streamError carries an HTTP status so the API layer can map it without
// re-deriving intent from the message.
type streamError struct {
	status int
	msg    string
}

func (e *streamError) Error() string { return e.msg }

// Status returns the HTTP status this failure maps to.
func (e *streamError) Status() int { return e.status }

func newStreamError(status int, format string, args ...interface{}) error {
	return &streamError{status: status, msg: fmt.Sprintf(format, args...)}
}
