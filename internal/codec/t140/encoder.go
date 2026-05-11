package t140

// Encoder accumulates UTF-8 text and produces RTP payloads in either plain
// T.140 (RFC 4103) or RED-wrapped form (RFC 2198) carrying up to N
// generations of prior chunks for loss resilience.
type Encoder struct {
	redundancy int
	t140PT     uint8
	history    []chunk
	pending    []byte
}

type chunk struct {
	ts   uint32
	data []byte
}

// NewEncoder returns an encoder that retains the last redundancyLevel chunks
// as RED redundancy. redundancyLevel == 0 produces plain T.140 packets.
// t140PT is the RTP payload type carrying the inner T.140 blocks (used in the
// RED block headers).
func NewEncoder(redundancyLevel int, t140PT uint8) *Encoder {
	if redundancyLevel < 0 {
		redundancyLevel = 0
	}
	return &Encoder{
		redundancy: redundancyLevel,
		t140PT:     t140PT,
	}
}

// Push appends UTF-8 text to the pending buffer. Call repeatedly between
// flushes; the encoder coalesces all pending bytes into one primary block.
func (e *Encoder) Push(text string) {
	if len(text) == 0 {
		return
	}
	e.pending = append(e.pending, text...)
}

// HasPending reports whether there are pending bytes that would be flushed.
func (e *Encoder) HasPending() bool { return len(e.pending) > 0 }

// Flush builds an RTP payload for the currently pending text. The returned
// useRED flag tells the caller which payload type to use: true → text/red PT,
// false → text/t140 PT. When there is nothing to send and no history the
// returned payload is nil.
//
// ts is the absolute RTP timestamp of this packet (1 kHz clock).
func (e *Encoder) Flush(ts uint32) (payload []byte, useRED bool) {
	primaryData := e.pending
	e.pending = nil

	// Plain T.140 mode (no redundancy configured) — emit a bare payload.
	if e.redundancy == 0 {
		if len(primaryData) == 0 {
			return nil, false
		}
		out := make([]byte, len(primaryData))
		copy(out, primaryData)
		return out, false
	}

	if len(primaryData) == 0 && len(e.history) == 0 {
		return nil, false
	}

	// RFC 4103 §4.3 / RFC 2198: always emit exactly `e.redundancy` redundant
	// block headers; leading slots are length-0 placeholders when history is
	// shallower than the configured depth.
	emptySlots := e.redundancy - len(e.history)
	headerLen := 4*e.redundancy + 1
	totalDataLen := len(primaryData)
	for _, c := range e.history {
		totalDataLen += len(c.data)
	}
	out := make([]byte, 0, headerLen+totalDataLen)

	for i := 0; i < e.redundancy; i++ {
		if i < emptySlots {
			// Length-0 placeholder; offset doesn't matter for an empty body.
			out = append(out, 0x80|(e.t140PT&0x7F))
			out = append(out, 0x00, 0x00, 0x00)
			continue
		}
		c := e.history[i-emptySlots]
		offset := ts - c.ts
		if offset > 0x3FFF {
			offset = 0x3FFF
		}
		blen := uint16(len(c.data))
		if blen > 0x3FF {
			blen = 0x3FF
		}
		out = append(out, 0x80|(e.t140PT&0x7F))
		out = append(out, byte(offset>>6))
		out = append(out, byte((offset&0x3F)<<2)|byte(blen>>8))
		out = append(out, byte(blen&0xFF))
	}
	out = append(out, e.t140PT&0x7F)

	for i := 0; i < e.redundancy; i++ {
		if i < emptySlots {
			continue
		}
		out = append(out, e.history[i-emptySlots].data...)
	}
	out = append(out, primaryData...)

	e.pushHistory(chunk{ts: ts, data: primaryData})
	return out, true
}

func (e *Encoder) pushHistory(c chunk) {
	if e.redundancy == 0 {
		return
	}
	cp := make([]byte, len(c.data))
	copy(cp, c.data)
	e.history = append(e.history, chunk{ts: c.ts, data: cp})
	if len(e.history) > e.redundancy {
		e.history = e.history[len(e.history)-e.redundancy:]
	}
}
