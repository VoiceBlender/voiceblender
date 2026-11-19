package sip

// RFC 3550 §A.1 limits.
const (
	rtpMaxDropout  = 3000
	rtpMaxMisorder = 100
	rtpSeqMod      = 1 << 16
)

// RecvStats counts received and expected RTP packets per RFC 3550 §A.1,
// without probation. A new SSRC or a confirmed sequence jump starts a new run;
// earlier runs stay in the totals so the counts cover the whole call.
type RecvStats struct {
	run      uint32 // incremented per run; zero before the first packet
	ssrc     uint32
	baseSeq  uint32 // first sequence number of the current run
	maxSeq   uint16 // highest sequence number seen in the current run
	cycles   uint32 // sequence wraps in the current run, times 2^16
	runRecv  uint32 // packets counted in the current run
	badSeq   uint32 // seq+1 of an unconfirmed jump; > 0xffff when none
	expPrior uint32 // run expected count at the last Interval (§A.3)
	rcvPrior uint32 // run received count at the last Interval
	pastExp  uint32 // expected packets of the earlier runs
	pastRcv  uint32 // received packets of the earlier runs
}

// Update reports false for the first packet of a large jump, which stays
// uncounted until the next packet confirms a sequence restart. Callers should
// keep such a packet out of timing statistics too.
func (r *RecvStats) Update(ssrc uint32, seq uint16) bool {
	if r.run == 0 || ssrc != r.ssrc {
		r.restart(ssrc, seq)
		return true
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
			return false
		}
		r.restart(ssrc, seq)
		return true
	default:
		// Late or duplicate.
	}
	r.runRecv++
	return true
}

func (r *RecvStats) restart(ssrc uint32, seq uint16) {
	if r.run != 0 {
		r.pastExp += r.RunExpected()
		r.pastRcv += r.runRecv
	}
	*r = RecvStats{
		run:     r.run + 1,
		ssrc:    ssrc,
		baseSeq: uint32(seq),
		maxSeq:  seq,
		runRecv: 1,
		badSeq:  rtpSeqMod + 1,
		pastExp: r.pastExp,
		pastRcv: r.pastRcv,
	}
}

// Run identifies the current run: it changes whenever a new SSRC or a
// sequence restart makes the sender's timestamps unrelated to earlier ones.
func (r *RecvStats) Run() uint32 { return r.run }

// SSRC is the sender of the current run.
func (r *RecvStats) SSRC() uint32 { return r.ssrc }

// ExtendedMax is the current run's extended highest sequence number.
func (r *RecvStats) ExtendedMax() uint32 { return r.cycles + uint32(r.maxSeq) }

// RunExpected is the number of packets the current run should have delivered.
func (r *RecvStats) RunExpected() uint32 {
	if r.run == 0 {
		return 0
	}
	return r.ExtendedMax() - r.baseSeq + 1
}

// RunReceived is the number of packets counted in the current run.
func (r *RecvStats) RunReceived() uint32 { return r.runRecv }

// Interval returns the current run's expected and received counts since the
// previous call (RFC 3550 §A.3).
func (r *RecvStats) Interval() (expected, received uint32) {
	exp := r.RunExpected()
	expected, received = exp-r.expPrior, r.runRecv-r.rcvPrior
	r.expPrior, r.rcvPrior = exp, r.runRecv
	return expected, received
}

// Received is the number of packets counted over the whole call.
func (r *RecvStats) Received() uint32 {
	if r.run == 0 {
		return 0
	}
	return r.pastRcv + r.runRecv
}

// Expected is the number of packets the whole call should have delivered.
func (r *RecvStats) Expected() uint32 {
	if r.run == 0 {
		return 0
	}
	return r.pastExp + r.RunExpected()
}

// Lost is zero when duplicates outnumber the losses.
func (r *RecvStats) Lost() uint32 {
	exp, rcv := r.Expected(), r.Received()
	if rcv >= exp {
		return 0
	}
	return exp - rcv
}
