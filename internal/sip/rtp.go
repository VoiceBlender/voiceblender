package sip

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/pion/rtp"
)

// ErrNotRTP is returned by ReadRTP when a received UDP packet is not valid RTP
// (e.g. RTCP, STUN). Callers should continue reading on this error.
var ErrNotRTP = errors.New("not an RTP packet")

const rtpBufSize = 1500

// RTPSession manages a UDP socket for RTP send/receive.
type RTPSession struct {
	conn       *net.UDPConn
	remoteAddr *net.UDPAddr
	localPort  int
}

// NewRTPSession creates a new RTP session listening on a random UDP port.
func NewRTPSession() (*RTPSession, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}

	addr := conn.LocalAddr().(*net.UDPAddr)
	return &RTPSession{
		conn:      conn,
		localPort: addr.Port,
	}, nil
}

// NewRTPSessionOnPort creates a new RTP session on a specific local port.
func NewRTPSessionOnPort(port int) (*RTPSession, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		return nil, fmt.Errorf("listen udp on port %d: %w", port, err)
	}

	addr := conn.LocalAddr().(*net.UDPAddr)
	return &RTPSession{
		conn:      conn,
		localPort: addr.Port,
	}, nil
}

// SetRemote sets the remote address for sending RTP packets.
func (s *RTPSession) SetRemote(ip string, port int) error {
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return fmt.Errorf("resolve remote: %w", err)
	}
	s.remoteAddr = addr
	return nil
}

// ReadRTP reads and unmarshals an RTP packet from the UDP socket. Blocks until data arrives.
func (s *RTPSession) ReadRTP() (*rtp.Packet, error) {
	buf := make([]byte, rtpBufSize)
	n, err := s.conn.Read(buf)
	if err != nil {
		return nil, err
	}

	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf[:n]); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotRTP, err)
	}
	return pkt, nil
}

// WriteRTP marshals and sends an RTP packet to the remote address.
func (s *RTPSession) WriteRTP(pkt *rtp.Packet) error {
	if s.remoteAddr == nil {
		return fmt.Errorf("remote address not set")
	}
	data, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("rtp marshal: %w", err)
	}
	_, err = s.conn.WriteToUDP(data, s.remoteAddr)
	return err
}

// SendKeepalive sends a small burst of silence RTP packets to the remote
// address. This is used immediately after SetRemote on outbound calls to
// punch through NAT devices (port-latching) before the leg's full media
// pipeline starts.
func (s *RTPSession) SendKeepalive(payloadType uint8, count int) {
	if s.remoteAddr == nil || count <= 0 {
		return
	}
	// 160 bytes of 0xFF = 20ms of PCMU silence (works for port-latching
	// regardless of actual codec since NAT only cares about the UDP flow).
	silence := make([]byte, 160)
	for i := range silence {
		silence[i] = 0xFF
	}
	var seq uint16
	var ts uint32
	for i := 0; i < count; i++ {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    payloadType,
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           0, // throwaway SSRC; real writeLoop will use its own
			},
			Payload: silence,
		}
		data, err := pkt.Marshal()
		if err != nil {
			return
		}
		s.conn.WriteToUDP(data, s.remoteAddr)
		seq++
		ts += 160
	}
}

// LocalPort returns the local UDP port this session is listening on.
func (s *RTPSession) LocalPort() int {
	return s.localPort
}

// SetReadDeadline sets a deadline on the underlying UDP socket for reads.
func (s *RTPSession) SetReadDeadline(t time.Time) error {
	return s.conn.SetReadDeadline(t)
}

// Close closes the UDP connection.
func (s *RTPSession) Close() error {
	return s.conn.Close()
}
