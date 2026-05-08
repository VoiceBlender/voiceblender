package t140

import "errors"

// ErrInvalidPayload is returned when a RED payload cannot be parsed.
var ErrInvalidPayload = errors.New("t140: invalid RED payload")

const dedupWindow = 64

// Decoder turns RFC 4103 / RFC 2198 RTP payloads back into a UTF-8 text
// stream, deduplicating redundant blocks and inserting U+FFFD when packet
// loss exceeds the redundancy window.
type Decoder struct {
	haveSeq  bool
	lastSeq  uint16
	seenTS   map[uint32]struct{}
	recentTS []uint32
}

// NewDecoder returns an empty decoder.
func NewDecoder() *Decoder {
	return &Decoder{seenTS: make(map[uint32]struct{}, dedupWindow)}
}

// DecodePacket processes one RTP payload and returns the new T.140 text bytes
// to append to the consumer's stream. lossMarker is true when a sequence gap
// was not covered by RED redundancy (a single U+FFFD has been prepended).
//
// packetPT is the RTP payload type of the received packet. t140PT and redPT
// are the negotiated dynamic PTs for text/t140 and text/red respectively
// (redPT == 0 means RED was not negotiated).
func (d *Decoder) DecodePacket(seq uint16, ts uint32, packetPT, t140PT, redPT uint8, payload []byte) (text string, lossMarker bool, err error) {
	var gap int
	if d.haveSeq {
		diff := int16(seq - d.lastSeq)
		if diff > 1 {
			gap = int(diff) - 1
		}
	}

	var blocks []decodedBlock
	switch {
	case redPT != 0 && packetPT == redPT:
		blocks, err = parseRED(payload, ts)
		if err != nil {
			return "", false, err
		}
	default:
		// Treat anything else (matching t140PT, or unknown) as a plain
		// T.140 block carrying the whole payload at the packet timestamp.
		blocks = []decodedBlock{{ts: ts, data: payload}}
	}

	// Sort blocks by timestamp ascending so we emit oldest first.
	for i := 1; i < len(blocks); i++ {
		for j := i; j > 0 && less32(blocks[j].ts, blocks[j-1].ts); j-- {
			blocks[j-1], blocks[j] = blocks[j], blocks[j-1]
		}
	}

	var buf []byte
	emitted := 0
	for _, b := range blocks {
		if len(b.data) == 0 {
			// Length-0 placeholder block (RFC 2198): skip without
			// touching the ts dedup set so it can't shadow a real
			// primary block sharing the same timestamp.
			continue
		}
		if _, ok := d.seenTS[b.ts]; ok {
			continue
		}
		d.markSeen(b.ts)
		emitted++
		buf = append(buf, b.data...)
	}

	if gap > 0 && emitted < gap+1 {
		buf = append([]byte(ReplacementChar), buf...)
		lossMarker = true
	}

	if !d.haveSeq || int16(seq-d.lastSeq) > 0 {
		d.lastSeq = seq
		d.haveSeq = true
	}
	return string(buf), lossMarker, nil
}

func (d *Decoder) markSeen(ts uint32) {
	if _, ok := d.seenTS[ts]; ok {
		return
	}
	d.seenTS[ts] = struct{}{}
	d.recentTS = append(d.recentTS, ts)
	if len(d.recentTS) > dedupWindow {
		evict := d.recentTS[0]
		d.recentTS = d.recentTS[1:]
		delete(d.seenTS, evict)
	}
}

type decodedBlock struct {
	ts   uint32
	data []byte
}

type redHeader struct {
	primary bool
	ts      uint32
	length  uint16
}

func parseRED(payload []byte, primaryTS uint32) ([]decodedBlock, error) {
	var headers []redHeader
	i := 0
	for {
		if i >= len(payload) {
			return nil, ErrInvalidPayload
		}
		b0 := payload[i]
		if b0&0x80 == 0 {
			headers = append(headers, redHeader{primary: true, ts: primaryTS})
			i++
			break
		}
		if i+4 > len(payload) {
			return nil, ErrInvalidPayload
		}
		b1, b2, b3 := payload[i+1], payload[i+2], payload[i+3]
		tsOffset := uint32(b1)<<6 | uint32(b2>>2)
		length := (uint16(b2&0x03) << 8) | uint16(b3)
		headers = append(headers, redHeader{
			primary: false,
			ts:      primaryTS - tsOffset,
			length:  length,
		})
		i += 4
	}

	blocks := make([]decodedBlock, 0, len(headers))
	for _, h := range headers {
		if h.primary {
			blocks = append(blocks, decodedBlock{
				ts:   h.ts,
				data: payload[i:],
			})
			return blocks, nil
		}
		end := i + int(h.length)
		if end > len(payload) {
			return nil, ErrInvalidPayload
		}
		blocks = append(blocks, decodedBlock{
			ts:   h.ts,
			data: payload[i:end],
		})
		i = end
	}
	return blocks, nil
}

// less32 is true when a is "before" b under uint32 wraparound (RFC 1982).
func less32(a, b uint32) bool {
	return int32(a-b) < 0
}
