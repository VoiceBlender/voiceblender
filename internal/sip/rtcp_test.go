package sip

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

func recvHeader(seq uint16, ts uint32) *rtp.Header {
	return &rtp.Header{SSRC: 0xabc, SequenceNumber: seq, Timestamp: ts}
}

func TestRTCPReceptionReport_LossAndWrap(t *testing.T) {
	r := &rtcpState{clockRate: 8000}
	now := time.Now()
	// 65530..65535, 0..9 is 16 packets across a wrap; drop two of them.
	seq := uint16(65530)
	for i := 0; i < 16; i++ {
		if i != 3 && i != 9 {
			r.onReceive(recvHeader(seq, uint32(i)*160), now.Add(time.Duration(i)*20*time.Millisecond))
		}
		seq++
	}
	rr, ok := r.receptionReport(now)
	if !ok {
		t.Fatal("no reception report")
	}
	if rr.SSRC != 0xabc || rr.TotalLost != 2 {
		t.Errorf("ssrc %x lost %d, want abc 2", rr.SSRC, rr.TotalLost)
	}
	if rr.LastSequenceNumber != 1<<16+9 {
		t.Errorf("extended highest seq = %d, want %d", rr.LastSequenceNumber, 1<<16+9)
	}
	if want := uint8(2 * 256 / 16); rr.FractionLost != want {
		t.Errorf("fraction lost = %d, want %d", rr.FractionLost, want)
	}
	if rr.Jitter > 1 {
		t.Errorf("evenly paced packets gave jitter %d", rr.Jitter)
	}
	if _, ok := r.receptionReport(now); ok {
		t.Error("a report with nothing received since the last one")
	}

	// The next interval loses nothing, whatever the cumulative count says.
	r.onReceive(recvHeader(10, 16*160), now.Add(320*time.Millisecond))
	if rr, _ := r.receptionReport(now); rr.FractionLost != 0 || rr.TotalLost != 2 {
		t.Errorf("second interval: fraction %d total %d", rr.FractionLost, rr.TotalLost)
	}
}

func TestRTCPJitter_IgnoresRepeatedTimestamps(t *testing.T) {
	r := &rtcpState{clockRate: 8000}
	now := time.Now()
	for i := 0; i < 10; i++ {
		r.onReceive(recvHeader(uint16(i), uint32(i)*160), now.Add(time.Duration(i)*20*time.Millisecond))
	}
	// An RFC 4733 event: one timestamp for 200 ms of packets.
	for i := 10; i < 20; i++ {
		r.onReceive(recvHeader(uint16(i), 10*160), now.Add(time.Duration(i)*20*time.Millisecond))
	}
	if r.src.jitter > 1 {
		t.Errorf("jitter = %.1f after a telephone-event", r.src.jitter)
	}
	// 10 ms late on an 8 kHz clock is 80 units, of which a sixteenth lands.
	r.onReceive(recvHeader(20, 20*160), now.Add(410*time.Millisecond))
	if r.src.jitter < 4 || r.src.jitter > 6 {
		t.Errorf("jitter = %.1f, want about 5", r.src.jitter)
	}
}

func TestRTCPReport_RTTFromEchoedSR(t *testing.T) {
	r := &rtcpState{clockRate: 8000}
	r.onSend(&rtp.Header{SSRC: 7, Timestamp: 1000}, 160, time.Now())
	sentAt := time.Now()
	sr := r.compound(sentAt, "c", false)[0].(*rtcp.SenderReport)
	if sr.PacketCount != 1 || sr.OctetCount != 160 || sr.SSRC != 7 {
		t.Fatalf("SR = %+v", sr)
	}
	// The peer held our SR for 30 ms and the path adds 40 ms in total.
	rr := &rtcp.ReceiverReport{SSRC: 9, Reports: []rtcp.ReceptionReport{{
		SSRC: 7, TotalLost: 3, Jitter: 80,
		LastSenderReport: uint32(sr.NTPTime >> 16),
		Delay:            30 * 65536 / 1000,
	}}}
	r.onRTCP([]rtcp.Packet{rr}, sentAt.Add(70*time.Millisecond))
	rep, ok := r.report()
	if !ok {
		t.Fatal("no report")
	}
	if rep.PacketsSent != 1 || rep.PacketsLost != 3 || rep.JitterMs != 10 {
		t.Errorf("report = %+v", rep)
	}
	if rep.RTT < 39*time.Millisecond || rep.RTT > 41*time.Millisecond {
		t.Errorf("rtt = %v, want 40ms", rep.RTT)
	}

	// A block about someone else's SSRC is not about us.
	r2 := &rtcpState{ssrc: 7}
	r2.onRTCP([]rtcp.Packet{&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 8}}}}, time.Now())
	if _, ok := r2.report(); ok {
		t.Error("report taken from a block on another SSRC")
	}
}

func TestRTCPCompound_RRWhileNotSending(t *testing.T) {
	r := &rtcpState{ssrc: 5}
	pkts := r.compound(time.Now(), "cname-x", true)
	if _, ok := pkts[0].(*rtcp.ReceiverReport); !ok {
		t.Fatalf("first packet is %T, want RR", pkts[0])
	}
	sdes := pkts[1].(*rtcp.SourceDescription)
	if sdes.Chunks[0].Source != 5 || sdes.Chunks[0].Items[0].Text != "cname-x" {
		t.Errorf("sdes = %+v", sdes)
	}
	if bye, ok := pkts[2].(*rtcp.Goodbye); !ok || bye.Sources[0] != 5 {
		t.Errorf("third packet = %+v, want BYE", pkts[2])
	}
}

// rtcpPair returns two sessions sending media and reports to each other, each
// with a reader running so incoming RTCP is processed.
func rtcpPair(t *testing.T) (a, b *RTPSession) {
	t.Helper()
	a, b = mustSession(t), mustSession(t)
	for _, p := range [][2]*RTPSession{{a, b}, {b, a}} {
		if err := p[0].SetRemote("127.0.0.1", p[1].LocalPort()); err != nil {
			t.Fatal(err)
		}
		p[0].SetRTCPMux(true)
		p[0].SetClockRate(8000)
	}
	return a, b
}

func drain(s *RTPSession) {
	go func() {
		pkt := &rtp.Packet{}
		for {
			s.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err := s.ReadRTPInto(pkt); err != nil && !errors.Is(err, ErrNotRTP) {
				return
			}
		}
	}()
}

func sendMedia(t *testing.T, s *RTPSession, ssrc uint32, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: uint16(i), Timestamp: uint32(i) * 160}, Payload: make([]byte, 160)}
		if err := s.WriteRTP(pkt); err != nil {
			t.Fatal(err)
		}
	}
}

// pace sends 20 ms packets until the test ends, as a live stream does.
func pace(t *testing.T, s *RTPSession, ssrc uint32) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			_ = s.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: uint16(i), Timestamp: uint32(i) * 160}, Payload: make([]byte, 160)})
		}
	}()
}

func waitReport(t *testing.T, s *RTPSession, until func(RTCPReport) bool) RTCPReport {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rep, ok := s.RTCPReport(); ok && until(rep) {
			return rep
		}
		time.Sleep(10 * time.Millisecond)
	}
	rep, _ := s.RTCPReport()
	t.Fatalf("no matching RTCP report; last %+v", rep)
	return rep
}

func TestRTCPSession_Exchange(t *testing.T) {
	a, b := rtcpPair(t)
	a.StartRTCP(50 * time.Millisecond)
	b.StartRTCP(50 * time.Millisecond)
	drain(a)
	drain(b)

	pace(t, a, 0x1111)
	rep := waitReport(t, a, func(r RTCPReport) bool { return r.RTT > 0 })
	if rep.PacketsSent == 0 || rep.PacketsLost != 0 || rep.JitterMs > 10 {
		t.Errorf("a's report = %+v", rep)
	}
	if rep.RTT > 100*time.Millisecond {
		t.Errorf("loopback rtt = %v", rep.RTT)
	}
	if _, ok := b.RTCPReport(); ok {
		t.Error("b sent no RTP, so nothing should report on it")
	}
}

func TestRTCPSession_SRTCP(t *testing.T) {
	a, b := rtcpPair(t)
	ka, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	kb, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_80)
	if err := a.SetSRTP(&ka, &kb); err != nil {
		t.Fatal(err)
	}
	if err := b.SetSRTP(&kb, &ka); err != nil {
		t.Fatal(err)
	}
	a.StartRTCP(50 * time.Millisecond)
	b.StartRTCP(50 * time.Millisecond)
	drain(a)
	drain(b)
	pace(t, a, 0x2222)
	waitReport(t, a, func(r RTCPReport) bool { return r.RTT > 0 })

	// A plain RTCP packet, or one keyed with anything else, fails authentication.
	if err := b.readRTCP(mustMarshal(t, &rtcp.ReceiverReport{SSRC: 1})); !errors.Is(err, ErrSRTPAuth) {
		t.Errorf("plain RTCP on an SRTP session: %v", err)
	}
}

func mustMarshal(t *testing.T, p rtcp.Packet) []byte {
	t.Helper()
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawPeer is a bare UDP socket standing in for a peer we inspect the wire on.
func rawPeer(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readRTCPFrom(c *net.UDPConn, within time.Duration) ([]byte, bool) {
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(within))
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return nil, false
		}
		if n >= 2 && buf[1] >= 192 && buf[1] <= 223 {
			return append([]byte(nil), buf[:n]...), true
		}
	}
}

func TestRTCPSession_WireFormat(t *testing.T) {
	peer := rawPeer(t)
	s := mustSession(t)
	if err := s.SetRemote("127.0.0.1", peer.LocalAddr().(*net.UDPAddr).Port); err != nil {
		t.Fatal(err)
	}
	s.SetClockRate(8000)

	// Not multiplexed: nothing may go to the RTP port.
	s.StartRTCP(20 * time.Millisecond)
	if _, ok := readRTCPFrom(peer, 150*time.Millisecond); ok {
		t.Fatal("RTCP sent on the RTP port without a=rtcp-mux")
	}

	s.SetRTCPMux(true)
	sendMedia(t, s, 0x3333, 5)
	raw, ok := readRTCPFrom(peer, time.Second)
	if !ok {
		t.Fatal("no RTCP")
	}
	pkts, err := rtcp.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	sr, ok := pkts[0].(*rtcp.SenderReport)
	if !ok || sr.SSRC != 0x3333 || sr.PacketCount != 5 {
		t.Fatalf("first packet = %+v", pkts[0])
	}
	sdes, ok := pkts[1].(*rtcp.SourceDescription)
	if !ok || sdes.Chunks[0].Source != 0x3333 || sdes.Chunks[0].Items[0].Type != rtcp.SDESCNAME || sdes.Chunks[0].Items[0].Text == "" {
		t.Errorf("second packet = %+v", pkts[1])
	}

	s.Close()
	for {
		raw, ok := readRTCPFrom(peer, time.Second)
		if !ok {
			t.Fatal("no BYE on close")
		}
		pkts, _ := rtcp.Unmarshal(raw)
		if bye, ok := pkts[len(pkts)-1].(*rtcp.Goodbye); ok {
			if bye.Sources[0] != 0x3333 {
				t.Errorf("BYE for %x", bye.Sources[0])
			}
			break
		}
	}
}

func TestRTCPSession_SRTCPOnTheWire(t *testing.T) {
	peer := rawPeer(t)
	s := mustSession(t)
	if err := s.SetRemote("127.0.0.1", peer.LocalAddr().(*net.UDPAddr).Port); err != nil {
		t.Fatal(err)
	}
	local, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_32)
	remote, _ := NewSDESCrypto(1, SuiteAES128CMSHA1_32)
	if err := s.SetSRTP(&local, &remote); err != nil {
		t.Fatal(err)
	}
	s.SetRTCPMux(true)
	s.StartRTCP(20 * time.Millisecond)

	raw, ok := readRTCPFrom(peer, time.Second)
	if !ok {
		t.Fatal("no SRTCP")
	}
	ctx, err := srtp.CreateContext(local.Key[:16], local.Key[16:], srtp.ProtectionProfileAes128CmHmacSha1_32)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := ctx.DecryptRTCP(nil, raw, nil)
	if err != nil {
		t.Fatalf("decrypt with the advertised key: %v", err)
	}
	pkts, err := rtcp.Unmarshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if sdes, ok := pkts[1].(*rtcp.SourceDescription); !ok || sdes.Chunks[0].Items[0].Text != s.cname {
		t.Errorf("decrypted compound = %+v", pkts)
	}
	if bytes.Contains(raw, []byte(s.cname)) {
		t.Error("SRTCP carried the CNAME in the clear")
	}
}

func TestRTCPSession_OffByDefault(t *testing.T) {
	peer := rawPeer(t)
	s := mustSession(t)
	if err := s.SetRemote("127.0.0.1", peer.LocalAddr().(*net.UDPAddr).Port); err != nil {
		t.Fatal(err)
	}
	s.SetRTCPMux(true)
	s.StartRTCP(0)
	sendMedia(t, s, 1, 3)
	s.Close()
	if _, ok := readRTCPFrom(peer, 150*time.Millisecond); ok {
		t.Error("RTCP sent without StartRTCP")
	}
}
