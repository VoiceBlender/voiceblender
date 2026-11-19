package leg

// RFC 3550 §A.1 limits.
const (
	rtpMaxDropout  = 3000
	rtpMaxMisorder = 100
	rtpSeqMod      = 1 << 16
)

// rtpRecvStats counts received and expected RTP packets per RFC 3550 §A.1,
// without probation. A new SSRC or a confirmed sequence jump starts a new run;
// earlier runs stay in the totals so the counts cover the whole call.
type rtpRecvStats struct {
	seen     bool
	ssrc     uint32
	baseSeq  uint32 // first sequence number of the current run
	maxSeq   uint16 // highest sequence number seen in the current run
	cycles   uint32 // sequence wraps in the current run, times 2^16
	runRecv  uint32 // packets counted in the current run
	badSeq   uint32 // seq+1 of an unconfirmed jump; > 0xffff when none
	priorExp uint32 // expected packets of the earlier runs
	priorRcv uint32 // received packets of the earlier runs
}

// update leaves the first packet of a large jump uncounted; only the next
// packet confirms a sequence restart.
func (r *rtpRecvStats) update(ssrc uint32, seq uint16) {
	if !r.seen || ssrc != r.ssrc {
		r.restart(ssrc, seq)
		return
	}
	udelta := seq - r.maxSeq
	switch {
	case udelta < rtpMaxDropout:
		if seq < r.maxSeq {
			r.cycles += rtpSeqMod
		}
		r.maxSeq = seq
	case uint32(udelta) <= rtpSeqMod-rtpMaxMisorder:
		if uint32(seq) != r.badSeq {
			r.badSeq = (uint32(seq) + 1) & (rtpSeqMod - 1)
			return
		}
		r.restart(ssrc, seq)
		return
	default:
		// Late or duplicate.
	}
	r.runRecv++
}

func (r *rtpRecvStats) restart(ssrc uint32, seq uint16) {
	if r.seen {
		r.priorExp += r.runExpected()
		r.priorRcv += r.runRecv
	}
	*r = rtpRecvStats{
		seen:     true,
		ssrc:     ssrc,
		baseSeq:  uint32(seq),
		maxSeq:   seq,
		runRecv:  1,
		badSeq:   rtpSeqMod + 1,
		priorExp: r.priorExp,
		priorRcv: r.priorRcv,
	}
}

func (r *rtpRecvStats) runExpected() uint32 {
	return r.cycles + uint32(r.maxSeq) - r.baseSeq + 1
}

func (r *rtpRecvStats) received() uint32 {
	if !r.seen {
		return 0
	}
	return r.priorRcv + r.runRecv
}

func (r *rtpRecvStats) expected() uint32 {
	if !r.seen {
		return 0
	}
	return r.priorExp + r.runExpected()
}

// lost is zero when duplicates outnumber the losses.
func (r *rtpRecvStats) lost() uint32 {
	exp, rcv := r.expected(), r.received()
	if rcv >= exp {
		return 0
	}
	return exp - rcv
}
