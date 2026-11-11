package sip

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// DefaultRTCPInterval is the RFC 3550 §6.2 minimum report interval.
const DefaultRTCPInterval = 5 * time.Second

// RTCPReport is the peer's view of the stream we send, taken from the
// reception reports in its RTCP.
type RTCPReport struct {
	PacketsSent uint32
	PacketsLost uint32
	JitterMs    float64
	// RTT stays zero until the peer echoes one of our sender reports.
	RTT time.Duration
}

// rtcpState is the RFC 3550 bookkeeping behind the reports we send and the
// ones we receive.
type rtcpState struct {
	mu        sync.Mutex
	clockRate uint32

	ssrc     uint32
	sent     bool // an RTP packet went out since the last report
	packets  uint32
	octets   uint32
	lastTS   uint32
	lastTSAt time.Time

	src      recvState
	lastSR   uint32
	lastSRAt time.Time

	peer         RTCPReport
	peerJitter   uint32
	peerReported bool
}

// recvState tracks the peer's RTP stream; a new SSRC starts it afresh.
type recvState struct {
	ssrc        uint32
	seen        bool
	baseSeq     uint32
	maxSeq      uint16
	cycles      uint32
	received    uint32
	expPrior    uint32
	recvPrior   uint32
	jitter      float64
	lastTS      uint32
	lastArrival time.Time
}

func (r *rtcpState) onSend(h *rtp.Header, payload int, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.SSRC != r.ssrc {
		r.ssrc, r.packets, r.octets = h.SSRC, 0, 0
		r.peerReported = false
	}
	r.sent = true
	r.packets++
	r.octets += uint32(payload)
	r.lastTS, r.lastTSAt = h.Timestamp, now
}

// onReceive follows RFC 3550 §A.1 without the probation period, and §A.8 for
// jitter. A packet repeating the previous timestamp is left out of the jitter:
// RFC 4733 events reuse one timestamp for their whole duration.
func (r *rtcpState) onReceive(h *rtp.Header, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	src := &r.src
	if !src.seen || h.SSRC != src.ssrc {
		*src = recvState{ssrc: h.SSRC, seen: true, baseSeq: uint32(h.SequenceNumber), maxSeq: h.SequenceNumber}
	} else if d := h.SequenceNumber - src.maxSeq; d != 0 && d < 0x8000 {
		if h.SequenceNumber < src.maxSeq {
			src.cycles += 1 << 16
		}
		src.maxSeq = h.SequenceNumber
	}
	src.received++

	if r.clockRate == 0 || (!src.lastArrival.IsZero() && h.Timestamp == src.lastTS) {
		return
	}
	if !src.lastArrival.IsZero() {
		d := now.Sub(src.lastArrival).Seconds()*float64(r.clockRate) - float64(int32(h.Timestamp-src.lastTS))
		src.jitter += (math.Abs(d) - src.jitter) / 16
	}
	src.lastArrival, src.lastTS = now, h.Timestamp
}

// receptionReport describes the peer's stream, or reports false when nothing
// arrived since the previous report.
func (r *rtcpState) receptionReport(now time.Time) (rtcp.ReceptionReport, bool) {
	src := &r.src
	if !src.seen || src.received == src.recvPrior {
		return rtcp.ReceptionReport{}, false
	}
	extMax := src.cycles + uint32(src.maxSeq)
	expected := extMax - src.baseSeq + 1
	lost := max(0, min(int64(expected)-int64(src.received), 0x7fffff))

	expInt := int64(expected - src.expPrior)
	lostInt := expInt - int64(src.received-src.recvPrior)
	src.expPrior, src.recvPrior = expected, src.received
	var fraction uint8
	if expInt > 0 && lostInt > 0 {
		fraction = uint8(min(lostInt<<8/expInt, 255))
	}

	var dlsr uint32
	if r.lastSR != 0 {
		dlsr = uint32(now.Sub(r.lastSRAt).Seconds() * 65536)
	}
	return rtcp.ReceptionReport{
		SSRC:               src.ssrc,
		FractionLost:       fraction,
		TotalLost:          uint32(lost),
		LastSequenceNumber: extMax,
		Jitter:             uint32(src.jitter),
		LastSenderReport:   r.lastSR,
		Delay:              dlsr,
	}, true
}

// compound builds the report for one interval: SR while we send, RR
// otherwise, followed by the mandatory SDES CNAME (RFC 3550 §6.1).
func (r *rtcpState) compound(now time.Time, cname string, bye bool) []rtcp.Packet {
	r.mu.Lock()
	defer r.mu.Unlock()
	var reports []rtcp.ReceptionReport
	if rr, ok := r.receptionReport(now); ok {
		reports = append(reports, rr)
	}
	var head rtcp.Packet
	if r.sent {
		rtpTime := r.lastTS + uint32(now.Sub(r.lastTSAt).Seconds()*float64(r.clockRate))
		head = &rtcp.SenderReport{
			SSRC:        r.ssrc,
			NTPTime:     ntpTime(now),
			RTPTime:     rtpTime,
			PacketCount: r.packets,
			OctetCount:  r.octets,
			Reports:     reports,
		}
		r.sent = false
	} else {
		head = &rtcp.ReceiverReport{SSRC: r.ssrc, Reports: reports}
	}
	pkts := []rtcp.Packet{head, &rtcp.SourceDescription{Chunks: []rtcp.SourceDescriptionChunk{{
		Source: r.ssrc,
		Items:  []rtcp.SourceDescriptionItem{{Type: rtcp.SDESCNAME, Text: cname}},
	}}}}
	if bye {
		pkts = append(pkts, &rtcp.Goodbye{Sources: []uint32{r.ssrc}})
	}
	return pkts
}

func (r *rtcpState) onRTCP(pkts []rtcp.Packet, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range pkts {
		switch p := p.(type) {
		case *rtcp.SenderReport:
			r.lastSR, r.lastSRAt = uint32(p.NTPTime>>16), now
			r.onReports(p.Reports, now)
		case *rtcp.ReceiverReport:
			r.onReports(p.Reports, now)
		}
	}
}

func (r *rtcpState) onReports(reports []rtcp.ReceptionReport, now time.Time) {
	for _, rr := range reports {
		if r.ssrc == 0 || rr.SSRC != r.ssrc {
			continue
		}
		r.peerReported = true
		r.peerJitter = rr.Jitter
		// TotalLost is 24-bit signed; duplicates can drive it negative.
		r.peer.PacketsLost = 0
		if rr.TotalLost < 0x800000 {
			r.peer.PacketsLost = rr.TotalLost
		}
		if rr.LastSenderReport != 0 {
			d := uint32(ntpTime(now)>>16) - rr.LastSenderReport - rr.Delay
			if int32(d) >= 0 {
				r.peer.RTT = time.Duration(uint64(d) * uint64(time.Second) / 65536)
			}
		}
	}
}

func (r *rtcpState) report() (RTCPReport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.peerReported {
		return RTCPReport{}, false
	}
	rep := r.peer
	rep.PacketsSent = r.packets
	if r.clockRate > 0 {
		rep.JitterMs = float64(r.peerJitter) / float64(r.clockRate) * 1000
	}
	return rep, true
}

// ntpTime converts t to the 64-bit NTP timestamp format of RFC 3550 §4.
func ntpTime(t time.Time) uint64 {
	const unixToNTP = 2208988800
	sec := uint64(t.Unix()) + unixToNTP
	frac := uint64(t.Nanosecond()) << 32 / uint64(time.Second)
	return sec<<32 | frac
}

// newCNAME returns a random per-session CNAME as RFC 7022 recommends.
func newCNAME() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomSSRC() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	if v := binary.BigEndian.Uint32(b[:]); v != 0 {
		return v
	}
	return 1
}

// SetClockRate sets the RTP clock of the negotiated codec, which jitter and
// sender-report timestamps are measured in.
func (s *RTPSession) SetClockRate(hz int) {
	s.rtcp.mu.Lock()
	s.rtcp.clockRate = uint32(hz)
	s.rtcp.mu.Unlock()
}

// SetRTCPMux records whether offer/answer settled on RTCP multiplexed onto
// the RTP port (RFC 5761). Reports are only sent while it is.
func (s *RTPSession) SetRTCPMux(on bool) { s.rtcpMux.Store(on) }

// RTCPMuxed reports what SetRTCPMux last recorded.
func (s *RTPSession) RTCPMuxed() bool { return s.rtcpMux.Load() }

// StartRTCP begins sending reports every interval, randomised by ±50% as
// RFC 3550 §6.3.1 requires. Later calls are no-ops.
func (s *RTPSession) StartRTCP(interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.rtcpOnce.Do(func() {
		s.cname = newCNAME()
		s.rtcp.mu.Lock()
		if s.rtcp.ssrc == 0 {
			s.rtcp.ssrc = randomSSRC()
		}
		s.rtcp.mu.Unlock()
		s.rtcpOn.Store(true)
		go s.rtcpLoop(interval)
	})
}

// RTCPReport returns the peer's latest report on our stream, if any.
func (s *RTPSession) RTCPReport() (RTCPReport, bool) { return s.rtcp.report() }

func (s *RTPSession) rtcpLoop(interval time.Duration) {
	t := time.NewTimer(randomInterval(interval) / 2)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		_ = s.sendRTCP(false)
		t.Reset(randomInterval(interval))
	}
}

func randomInterval(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.5 + mrand.Float64()))
}

func (s *RTPSession) sendRTCP(bye bool) error {
	if !s.rtcpMux.Load() {
		return nil
	}
	addr := s.getRemote()
	if addr == nil {
		return nil
	}
	data, err := rtcp.Marshal(s.rtcp.compound(time.Now(), s.cname, bye))
	if err != nil {
		return fmt.Errorf("rtcp marshal: %w", err)
	}
	if tx := s.tx.Load(); tx != nil {
		tx.mu.Lock()
		data, err = tx.ctx.EncryptRTCP(nil, data, &tx.rtcpHdr)
		tx.mu.Unlock()
		if err != nil {
			return fmt.Errorf("srtcp encrypt: %w", err)
		}
	}
	_, err = s.conn.WriteToUDP(data, addr)
	return err
}

// readRTCP takes in an RTCP compound packet muxed on the RTP port.
func (s *RTPSession) readRTCP(b []byte) error {
	if rx := s.rx.Load(); rx != nil {
		rx.mu.Lock()
		plain, err := rx.ctx.DecryptRTCP(nil, b, &rx.rtcpHdr)
		rx.mu.Unlock()
		if err != nil {
			return fmt.Errorf("%w: %w: %v", ErrNotRTP, ErrSRTPAuth, err)
		}
		b = plain
	}
	if pkts, err := rtcp.Unmarshal(b); err == nil {
		s.rtcp.onRTCP(pkts, time.Now())
	}
	return fmt.Errorf("%w: rtcp-on-rtp-port", ErrNotRTP)
}
