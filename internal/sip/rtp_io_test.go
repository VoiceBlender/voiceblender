package sip

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func newLoopbackPair(t *testing.T) (*RTPSession, *net.UDPConn) {
	t.Helper()
	sess, err := NewRTPSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	sess.SetReadDeadline(time.Now().Add(2 * time.Second))
	return sess, peer
}

func sendRTP(t *testing.T, from *net.UDPConn, to *RTPSession, seq uint16, payload []byte) {
	t.Helper()
	raw, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq, SSRC: 7},
		Payload: payload,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: to.LocalPort()}
	if _, err := from.WriteToUDP(raw, dst); err != nil {
		t.Fatal(err)
	}
}

func TestReadRTPInto_ReusesPacketAndLatchesSource(t *testing.T) {
	sess, peer := newLoopbackPair(t)
	// An SDP address that differs from where media actually comes from.
	if err := sess.SetRemote("127.0.0.1", 9); err != nil {
		t.Fatal(err)
	}

	var pkt rtp.Packet
	for seq, payload := range [][]byte{{1, 2, 3}, {4, 5}} {
		sendRTP(t, peer, sess, uint16(seq), payload)
		if err := sess.ReadRTPInto(&pkt); err != nil {
			t.Fatal(err)
		}
		if pkt.SequenceNumber != uint16(seq) || !bytes.Equal(pkt.Payload, payload) {
			t.Fatalf("packet %d: got seq=%d payload=%v", seq, pkt.SequenceNumber, pkt.Payload)
		}
	}
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port
	if got := sess.RemoteAddr(); got == nil || got.Port != peerPort || !got.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("remote not latched to source: %v", got)
	}

	// A later SDP update is overridden again by the next packet, as before.
	if err := sess.SetRemote("127.0.0.1", 9); err != nil {
		t.Fatal(err)
	}
	sendRTP(t, peer, sess, 2, []byte{6})
	if err := sess.ReadRTPInto(&pkt); err != nil {
		t.Fatal(err)
	}
	if got := sess.RemoteAddr(); got.Port != peerPort {
		t.Fatalf("remote not re-latched after SetRemote: %v", got)
	}
}

func TestReadRTP_OwnsPayload(t *testing.T) {
	sess, peer := newLoopbackPair(t)
	sendRTP(t, peer, sess, 1, []byte{1, 1})
	first, err := sess.ReadRTP()
	if err != nil {
		t.Fatal(err)
	}
	sendRTP(t, peer, sess, 2, []byte{2, 2})
	if _, err := sess.ReadRTP(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Payload, []byte{1, 1}) {
		t.Fatalf("ReadRTP payload overwritten by a later read: %v", first.Payload)
	}
}

func TestReadRTPInto_RejectsMuxedRTCP(t *testing.T) {
	sess, peer := newLoopbackPair(t)
	rtcp := []byte{0x80, 200, 0, 1, 0, 0, 0, 7}
	if _, err := peer.WriteToUDP(rtcp, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sess.LocalPort()}); err != nil {
		t.Fatal(err)
	}
	var pkt rtp.Packet
	if err := sess.ReadRTPInto(&pkt); !errors.Is(err, ErrNotRTP) {
		t.Fatalf("got %v, want ErrNotRTP", err)
	}
}

func TestWriteRTPBuf_RoundTripAndReuse(t *testing.T) {
	sess, peer := newLoopbackPair(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr)
	if err := sess.SetRemote("127.0.0.1", peerAddr.Port); err != nil {
		t.Fatal(err)
	}
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 8, SequenceNumber: 42, Timestamp: 160, SSRC: 9},
		Payload: bytes.Repeat([]byte{0xD5}, 160),
	}
	buf, err := sess.WriteRTPBuf(&pkt, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw := make([]byte, 1500)
	n, _, err := peer.ReadFromUDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got rtp.Packet
	if err := got.Unmarshal(raw[:n]); err != nil {
		t.Fatal(err)
	}
	if got.SequenceNumber != 42 || got.Timestamp != 160 || got.PayloadType != 8 || !bytes.Equal(got.Payload, pkt.Payload) {
		t.Fatalf("round trip mismatch: %+v", got.Header)
	}

	allocs := testing.AllocsPerRun(50, func() {
		buf, _ = sess.WriteRTPBuf(&pkt, buf)
	})
	if allocs > 0 {
		t.Fatalf("WriteRTPBuf allocated %.1f times per packet with a reused buffer", allocs)
	}
}
