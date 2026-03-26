package sip

import (
	"errors"
	"fmt"
	"net"

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

// LocalPort returns the local UDP port this session is listening on.
func (s *RTPSession) LocalPort() int {
	return s.localPort
}

// Close closes the UDP connection.
func (s *RTPSession) Close() error {
	return s.conn.Close()
}
