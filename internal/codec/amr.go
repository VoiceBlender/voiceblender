package codec

import (
	"errors"
	"sync/atomic"

	amrnb "github.com/VoiceBlender/goamr-nb"
	amrwb "github.com/VoiceBlender/goamr-wb"
)

// DTXEncoder is implemented by encoders that run discontinuous transmission.
// Their Encode returns an empty payload for a frame that must not be sent,
// although its 20 ms still advance the RTP timestamp. Talkspurt reports whether
// the last payload starts a talkspurt, which sets the RTP marker bit.
type DTXEncoder interface {
	Talkspurt() bool
}

// GapDecoder is implemented by decoders that can fill a 20 ms interval that
// arrived without a packet: comfort noise while the peer is in DTX, concealment
// otherwise.
type GapDecoder interface {
	DecodeGap(dst []int16) ([]int16, error)
	// InDTX reports whether the last packet ended in a SID or NO_DATA frame,
	// after which a DTX sender stays silent until its next SID update or
	// speech.
	InDTX() bool
}

// FECDecoder is implemented by gap decoders that can rebuild the frame before
// a packet from redundancy that packet carries.
type FECDecoder interface {
	// DecodeFEC returns the 20 ms before the packet next: from its redundancy
	// when it has any, by concealment otherwise.
	DecodeFEC(dst []int16, next []byte) ([]int16, error)
}

// AMRConfig configures an AMR-NB or AMR-WB encoder/decoder pair for one RTP
// session. The fields mirror the negotiated RFC 4867 fmtp.
type AMRConfig struct {
	// Mode is the highest speech mode sent. Codec mode requests from the peer
	// can lower it, never raise it above Mode.
	Mode               int
	OctetAligned       bool
	ModeSet            []int
	ModeChangePeriod   int
	ModeChangeNeighbor bool
}

// NewAMRWBSession creates an AMR-WB encoder with DTX and a matching decoder
// that passes the peer's codec mode requests (CMR) on to the encoder.
func NewAMRWBSession(cfg AMRConfig) (Encoder, Decoder, error) {
	var modeSet []amrwb.Mode
	for _, m := range cfg.ModeSet {
		modeSet = append(modeSet, amrwb.Mode(m))
	}
	enc, err := amrwb.NewEncoder(amrwb.EncoderConfig{
		Mode:               amrwb.Mode(cfg.Mode),
		OctetAligned:       cfg.OctetAligned,
		DTX:                true,
		ModeSet:            modeSet,
		ModeChangePeriod:   cfg.ModeChangePeriod,
		ModeChangeNeighbor: cfg.ModeChangeNeighbor,
	})
	if err != nil {
		return nil, nil, err
	}
	dec := amrwb.NewDecoder(amrwb.DecoderConfig{OctetAligned: cfg.OctetAligned})
	e, d := newAMRSession[amrwb.Mode](enc, dec, cfg, amrwb.FrameSamples, amrwb.MaxPayloadBytesFor, amrwb.NoModeRequest, amrwb.FTSID)
	return e, d, nil
}

// NewAMRNBSession is NewAMRWBSession for AMR-NB.
func NewAMRNBSession(cfg AMRConfig) (Encoder, Decoder, error) {
	var modeSet []amrnb.Mode
	for _, m := range cfg.ModeSet {
		modeSet = append(modeSet, amrnb.Mode(m))
	}
	enc, err := amrnb.NewEncoder(amrnb.EncoderConfig{
		Mode:               amrnb.Mode(cfg.Mode),
		OctetAligned:       cfg.OctetAligned,
		DTX:                true,
		ModeSet:            modeSet,
		ModeChangePeriod:   cfg.ModeChangePeriod,
		ModeChangeNeighbor: cfg.ModeChangeNeighbor,
	})
	if err != nil {
		return nil, nil, err
	}
	dec := amrnb.NewDecoder(amrnb.DecoderConfig{OctetAligned: cfg.OctetAligned})
	e, d := newAMRSession[amrnb.Mode](enc, dec, cfg, amrnb.FrameSamples, amrnb.MaxPayloadBytesFor, amrnb.NoModeRequest, amrnb.FTSID)
	return e, d, nil
}

type amrLibEncoder[M ~int] interface {
	EncodeTo(dst []byte, samples []int16) (int, error)
	SetMode(m M)
	Talkspurt() bool
	Reset()
}

type amrLibDecoder[M ~int] interface {
	DecodeTo(dst []int16, payload []byte) (int, error)
	DecodeNoData(dst []int16) (int, error)
	ModeRequest() M
	Reset()
}

const noAMRModeRequest = -1

func newAMRSession[M ~int](enc amrLibEncoder[M], dec amrLibDecoder[M], cfg AMRConfig,
	frameSamples int, maxPayload func(int) int, noRequest M, sidFT int) (*amrEncoder[M], *amrDecoder[M]) {
	// The peer's mode request crosses from the receive goroutine to the send
	// goroutine, which own the decoder and encoder respectively.
	cmr := &atomic.Int32{}
	cmr.Store(noAMRModeRequest)
	e := &amrEncoder[M]{
		lib:          enc,
		ceiling:      M(cfg.Mode),
		frameSamples: frameSamples,
		maxPayload:   maxPayload,
		cmr:          cmr,
		applied:      noAMRModeRequest,
	}
	d := &amrDecoder[M]{
		lib:          dec,
		frameSamples: frameSamples,
		octetAligned: cfg.OctetAligned,
		noRequest:    noRequest,
		sidFT:        sidFT,
		cmr:          cmr,
	}
	return e, d
}

type amrEncoder[M ~int] struct {
	lib          amrLibEncoder[M]
	ceiling      M
	frameSamples int
	maxPayload   func(frames int) int
	cmr          *atomic.Int32
	applied      int32
}

func (e *amrEncoder[M]) Encode(samples []int16) ([]byte, error) {
	return e.EncodeInto(nil, samples)
}

func (e *amrEncoder[M]) EncodeInto(dst []byte, samples []int16) ([]byte, error) {
	if r := e.cmr.Load(); r != e.applied {
		e.applied = r
		if r != noAMRModeRequest {
			e.lib.SetMode(min(M(r), e.ceiling))
		}
	}
	dst = growBytes(dst, e.maxPayload(len(samples)/e.frameSamples))
	n, err := e.lib.EncodeTo(dst, samples)
	if err != nil {
		return nil, err
	}
	return dst[:n], nil
}

func (e *amrEncoder[M]) Talkspurt() bool { return e.lib.Talkspurt() }

func (e *amrEncoder[M]) Reset() { e.lib.Reset() }

type amrDecoder[M ~int] struct {
	lib          amrLibDecoder[M]
	frameSamples int
	octetAligned bool
	noRequest    M
	sidFT        int
	cmr          *atomic.Int32
	inDTX        bool
}

var errAMRToC = errors.New("amr: malformed table of contents")

func (d *amrDecoder[M]) Decode(data []byte) ([]int16, error) {
	return d.DecodeInto(nil, data)
}

func (d *amrDecoder[M]) DecodeInto(dst []int16, data []byte) ([]int16, error) {
	frames, lastFT, ok := amrToC(data, d.octetAligned)
	if !ok {
		return nil, errAMRToC
	}
	dst = growInt16(dst, frames*d.frameSamples)
	n, err := d.lib.DecodeTo(dst, data)
	if err != nil {
		return nil, err
	}
	d.inDTX = lastFT == d.sidFT || lastFT == amrFTNoData
	if m := d.lib.ModeRequest(); m != d.noRequest {
		d.cmr.Store(int32(m))
	}
	return dst[:n], nil
}

func (d *amrDecoder[M]) DecodeGap(dst []int16) ([]int16, error) {
	dst = growInt16(dst, d.frameSamples)
	n, err := d.lib.DecodeNoData(dst)
	if err != nil {
		return nil, err
	}
	return dst[:n], nil
}

func (d *amrDecoder[M]) InDTX() bool { return d.inDTX }

func (d *amrDecoder[M]) Reset() {
	d.lib.Reset()
	d.inDTX = false
}

// amrFTNoData is the RFC 4867 NO_DATA frame type, shared by AMR-NB and AMR-WB.
const amrFTNoData = 15

// amrToC walks an RFC 4867 payload's table of contents and returns the number
// of frames and the frame type of the last one.
func amrToC(p []byte, octetAligned bool) (frames, lastFT int, ok bool) {
	if octetAligned {
		for i := 1; i < len(p); i++ {
			frames++
			lastFT = int(p[i]>>3) & 0x0f
			if p[i]&0x80 == 0 {
				return frames, lastFT, true
			}
		}
		return 0, 0, false
	}
	// Bandwidth-efficient: a 4-bit CMR, then 6-bit entries of F, FT(4), Q.
	bit := func(i int) int { return int(p[i/8]>>(7-i%8)) & 1 }
	for off := 4; off+6 <= len(p)*8; off += 6 {
		frames++
		lastFT = bit(off+1)<<3 | bit(off+2)<<2 | bit(off+3)<<1 | bit(off+4)
		if bit(off) == 0 {
			return frames, lastFT, true
		}
	}
	return 0, 0, false
}
