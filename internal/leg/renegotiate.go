package leg

import (
	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// liveCodec is the codec configuration the media loops actually run on, held
// as one immutable value so a mid-call renegotiation can replace it wholesale.
// The loops read it per iteration and never see a half-applied change: without
// that, swapping a decoder while readLoop sits between "read packet" and
// "decode payload" would decode the new codec's payload with the old decoder.
//
// It doubles as the record of what the pipeline is running, which is what lets
// renegotiation detect a real change — the negotiated fields on mediaStream
// have already been overwritten by the time the answer is built.
type liveCodec struct {
	codecType codec.CodecType
	encoder   codec.Encoder
	decoder   codec.Decoder

	// rtpPT is the payload type we receive on, sendPT the one we transmit on.
	rtpPT  uint8
	sendPT uint8

	// pcmFrameBytes is 20 ms of native-rate PCM; samplesPerFrame the RTP
	// timestamp increment for the same 20 ms.
	pcmFrameBytes   int
	samplesPerFrame uint32

	dtmfSendPT        uint8
	dtmfSamplesPerPkt uint16

	// The encoder-shaping AMR parameters, kept so a mode (bitrate) change is
	// detected even when the codec itself is unchanged.
	amrwbMode         int
	amrwbOctetAligned bool
	amrnbMode         int
	amrnbOctetAligned bool
}

// sampleRate is the PCM rate this configuration produces and consumes.
func (c *liveCodec) sampleRate() int { return c.codecType.SampleRate() }

// snapshotCodec builds the live configuration from the stream's currently
// negotiated fields. The caller holds no lock; the fields it reads are written
// only by the negotiation paths, which are serialised by the dialog.
func snapshotCodec(s *mediaStream, enc codec.Encoder, dec codec.Decoder) *liveCodec {
	sendPT := s.rtpPT
	if s.rtpSendPT != 0 {
		sendPT = s.rtpSendPT
	}
	telephoneEventPT := s.dtmfSendPT
	if telephoneEventPT == 0 {
		telephoneEventPT = 101
	}
	dtmfSamples := uint16(s.dtmfClockRate / 50)
	if dtmfSamples == 0 {
		dtmfSamples = 160
	}
	return &liveCodec{
		codecType:         s.codecType,
		encoder:           enc,
		decoder:           dec,
		rtpPT:             s.rtpPT,
		sendPT:            sendPT,
		pcmFrameBytes:     s.codecType.SampleRate() / 50 * 2,
		samplesPerFrame:   uint32(s.codecType.ClockRate() / 50),
		dtmfSendPT:        telephoneEventPT,
		dtmfSamplesPerPkt: dtmfSamples,

		amrwbMode:         s.amrwbMode,
		amrwbOctetAligned: s.amrwbOctetAligned,
		amrnbMode:         s.amrnbMode,
		amrnbOctetAligned: s.amrnbOctetAligned,
	}
}

// live returns the configuration the loops are running on, or nil before media
// is set up.
func (s *mediaStream) live() *liveCodec { return s.liveCodec.Load() }

// needsRebuild reports whether the stream's newly negotiated parameters differ
// from what the pipeline is running in a way that requires a new encoder or
// decoder. A payload-type change alone does not: the loops read the PT from the
// same value and follow it without touching the codec.
func (c *liveCodec) needsRebuild(s *mediaStream) bool {
	if c.codecType != s.codecType {
		return true
	}
	switch s.codecType {
	case codec.CodecAMRWB:
		return c.amrwbMode != s.amrwbMode || c.amrwbOctetAligned != s.amrwbOctetAligned
	case codec.CodecAMRNB:
		return c.amrnbMode != s.amrnbMode || c.amrnbOctetAligned != s.amrnbOctetAligned
	}
	return false
}

// changed reports whether anything the loops read differs, including the
// payload types that need no codec rebuild.
func (c *liveCodec) changed(s *mediaStream) bool {
	if c.needsRebuild(s) {
		return true
	}
	next := snapshotCodec(s, c.encoder, c.decoder)
	return next.rtpPT != c.rtpPT || next.sendPT != c.sendPT ||
		next.dtmfSendPT != c.dtmfSendPT || next.dtmfSamplesPerPkt != c.dtmfSamplesPerPkt
}

// renegotiateStreamMedia re-points a running stream's media pipeline at the
// codec the latest offer/answer settled on. It is the mid-call counterpart of
// setupStreamMedia: the RTP socket, the frame channels and the loops all
// survive, so the leg keeps its identity in the room and its taps stay
// attached, while the encoder, decoder and framing follow the new codec.
//
// Returns the new sample rate and whether it differs from the old one. A rate
// change also has to reach the room, whose resamplers and filter chain were
// sized for the previous rate.
func (l *SIPLeg) renegotiateStreamMedia(s *mediaStream) (rate int, rateChanged bool) {
	cur := s.live()
	if cur == nil {
		// Media was never set up; nothing is running to renegotiate.
		return 0, false
	}
	if !cur.changed(s) {
		return cur.sampleRate(), false
	}

	enc, dec := cur.encoder, cur.decoder
	if cur.needsRebuild(s) {
		var err error
		enc, dec, err = l.buildCodecPair(s)
		if err != nil {
			// Keep running the old codec rather than dropping the media: a leg
			// that still sounds like the previous codec is recoverable, one
			// with no encoder at all is not.
			l.log.Error("re-negotiation: codec rebuild failed, keeping the running codec",
				"leg_id", l.id, "stream_id", s.id,
				"from", cur.codecType.String(), "to", s.codecType.String(), "error", err)
			return cur.sampleRate(), false
		}
	}

	next := snapshotCodec(s, enc, dec)
	rateChanged = next.sampleRate() != cur.sampleRate()

	l.mu.Lock()
	s.jbFrameBytes = next.pcmFrameBytes
	jb := s.jb
	l.mu.Unlock()

	// Publish before draining, so anything the loops push from here on is at
	// the new rate.
	s.liveCodec.Store(next)

	if rateChanged {
		// Queued PCM is at the old rate. Handing it to a room that has just
		// been told the rate changed would play it back at the wrong speed, so
		// drop it: at most 5 frames, 100 ms.
		if jb != nil {
			jb.Reset()
		}
		drain(s.inFrames)
		drain(s.outFrames)
	}

	l.log.Info("SIP leg media renegotiated",
		"leg_id", l.id,
		"stream_id", s.id,
		"codec", cur.codecType.String()+" -> "+next.codecType.String(),
		"sample_rate", next.sampleRate(),
		"rate_changed", rateChanged,
		"payload_type", next.rtpPT,
	)
	return next.sampleRate(), rateChanged
}

// drain empties a frame channel without blocking.
func drain(ch chan []byte) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// buildCodecPair constructs the encoder and decoder for a stream's currently
// negotiated codec. Shared by first-time setup and renegotiation so the two
// cannot drift apart.
func (l *SIPLeg) buildCodecPair(s *mediaStream) (codec.Encoder, codec.Decoder, error) {
	switch s.codecType {
	case codec.CodecAMRWB:
		enc, err := codec.NewAMRWBEncoder(s.amrwbMode, s.amrwbOctetAligned)
		if err != nil {
			return nil, nil, err
		}
		return enc, codec.NewAMRWBDecoder(s.amrwbOctetAligned), nil
	case codec.CodecAMRNB:
		enc, err := codec.NewAMRNBEncoder(s.amrnbMode, s.amrnbOctetAligned)
		if err != nil {
			return nil, nil, err
		}
		return enc, codec.NewAMRNBDecoder(s.amrnbOctetAligned), nil
	default:
		enc, err := codec.NewEncoder(s.codecType)
		if err != nil {
			return nil, nil, err
		}
		dec, err := codec.NewDecoder(s.codecType)
		if err != nil {
			return nil, nil, err
		}
		return enc, dec, nil
	}
}

// SetOnMediaRateChange registers a callback fired when a mid-call
// renegotiation changes the sample rate of one of the leg's streams. The room
// uses it to rebuild the resamplers and filter chain it sized for the old rate.
//
// streamID identifies which stream moved: a multi-stream leg negotiates each
// m-line separately, so the primary changing rate says nothing about the rest.
// The callback runs on the negotiation goroutine with no leg lock held.
func (l *SIPLeg) SetOnMediaRateChange(fn func(streamID string, rate int)) {
	l.mu.Lock()
	l.onMediaRateChange = fn
	l.mu.Unlock()
}

func (l *SIPLeg) notifyRateChange(streamID string, rate int) {
	l.mu.RLock()
	fn := l.onMediaRateChange
	l.mu.RUnlock()
	if fn != nil {
		fn(streamID, rate)
	}
}
