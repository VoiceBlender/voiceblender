package sip

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/pion/rtp"
)

// ErrNotRTP is returned by ReadRTP when a received UDP packet is not valid RTP
// (e.g. RTCP, STUN). Callers should continue reading on this error.
var ErrNotRTP = errors.New("not an RTP packet")

const rtpBufSize = 1500

// RTPSession manages a UDP socket for RTP send/receive.
// It implements symmetric RTP (RFC 4961): once an RTP packet is received,
// the remote address is latched to the source IP:port of that packet,
// overriding the SDP-provided address. This is essential for NAT traversal.
type RTPSession struct {
	conn       *net.UDPConn
	remoteAddr unsafe.Pointer // *net.UDPAddr, accessed atomically
	localPort  int
	allocator  *PortAllocator // non-nil when port was allocated from a pool
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

// NewRTPSessionFromAllocator creates an RTP session using a port from the
// allocator's pool. If alloc is nil, behaves like NewRTPSession (OS-assigned).
func NewRTPSessionFromAllocator(alloc *PortAllocator) (*RTPSession, error) {
	if alloc == nil {
		return NewRTPSession()
	}
	port, err := alloc.Allocate()
	if err != nil {
		return nil, err
	}
	sess, err := NewRTPSessionOnPort(port)
	if err != nil {
		alloc.Release(port)
		return nil, err
	}
	sess.allocator = alloc
	return sess, nil
}

// getRemote returns the current remote address atomically.
func (s *RTPSession) getRemote() *net.UDPAddr {
	return (*net.UDPAddr)(atomic.LoadPointer(&s.remoteAddr))
}

// setRemote stores the remote address atomically.
func (s *RTPSession) setRemote(addr *net.UDPAddr) {
	atomic.StorePointer(&s.remoteAddr, unsafe.Pointer(addr))
}

// SetRemote sets the remote address for sending RTP packets.
func (s *RTPSession) SetRemote(ip string, port int) error {
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return fmt.Errorf("resolve remote: %w", err)
	}
	s.setRemote(addr)
	return nil
}

// ReadRTP reads and unmarshals an RTP packet from the UDP socket. Blocks
// until data arrives. Implements symmetric RTP: the remote address is
// latched to the source IP:port of each incoming RTP packet.
func (s *RTPSession) ReadRTP() (*rtp.Packet, error) {
	buf := make([]byte, rtpBufSize)
	n, srcAddr, err := s.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}

	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf[:n]); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotRTP, err)
	}

	// Symmetric RTP: latch remote address to the source of incoming RTP.
	if srcAddr != nil {
		s.setRemote(srcAddr)
	}

	return pkt, nil
}

// WriteRTP marshals and sends an RTP packet to the remote address.
func (s *RTPSession) WriteRTP(pkt *rtp.Packet) error {
	addr := s.getRemote()
	if addr == nil {
		return fmt.Errorf("remote address not set")
	}
	data, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("rtp marshal: %w", err)
	}
	_, err = s.conn.WriteToUDP(data, addr)
	return err
}

// SendKeepalive sends a small burst of silence RTP packets to the remote
// address. This is used immediately after SetRemote on outbound calls to
// punch through NAT devices (port-latching) before the leg's full media
// pipeline starts.
func (s *RTPSession) SendKeepalive(payloadType uint8, count int) {
	addr := s.getRemote()
	if addr == nil || count <= 0 {
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
		s.conn.WriteToUDP(data, addr)
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

// Close closes the UDP connection and releases the port back to the allocator.
func (s *RTPSession) Close() error {
	err := s.conn.Close()
	if s.allocator != nil {
		s.allocator.Release(s.localPort)
	}
	return err
}
