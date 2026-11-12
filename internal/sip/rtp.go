package sip

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// ErrNotRTP is returned by ReadRTP when a received UDP packet is not valid RTP
// (e.g. RTCP, STUN). Callers should continue reading on this error.
var ErrNotRTP = errors.New("not an RTP packet")

// ErrSRTPAuth accompanies ErrNotRTP for a packet that failed SRTP
// authentication. Unlike RTCP or STUN it is no sign the peer is alive.
var ErrSRTPAuth = errors.New("srtp authentication failed")

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
	closeOnce  sync.Once
	closeErr   error
	readBuf    []byte // ReadRTPInto's buffer, owned by the single reader

	// SDES-SRTP state per direction; nil while the session carries plain RTP.
	tx atomic.Pointer[srtpDir]
	rx atomic.Pointer[srtpDir]

	rtcp     rtcpState
	rtcpMux  atomic.Bool
	rtcpOn   atomic.Bool
	rtcpOnce sync.Once
	cname    string
	done     chan struct{}
}

func newSession(conn *net.UDPConn) *RTPSession {
	return &RTPSession{
		conn:      conn,
		localPort: conn.LocalAddr().(*net.UDPAddr).Port,
		done:      make(chan struct{}),
	}
}

// NewRTPSession creates a new RTP session listening on a random UDP port.
// The socket is dual-stack on Linux (accepts both v4 and v6 when
// /proc/sys/net/ipv6/bindv6only is 0, which is the default).
func NewRTPSession() (*RTPSession, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6unspecified, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}
	return newSession(conn), nil
}

// NewRTPSessionOnPort creates a new RTP session on a specific local port.
// Same dual-stack semantics as NewRTPSession.
func NewRTPSessionOnPort(port int) (*RTPSession, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6unspecified, Port: port})
	if err != nil {
		return nil, fmt.Errorf("listen udp on port %d: %w", port, err)
	}
	return newSession(conn), nil
}

const rtpBindAttempts = 8

// NewRTPSessionFromAllocator creates an RTP session using a port from the
// allocator's pool. If alloc is nil, behaves like NewRTPSession (OS-assigned).
// A port in the pool can be held by a process outside this one, so a bind
// failure moves on to the next port instead of failing the call.
func NewRTPSessionFromAllocator(alloc *PortAllocator) (*RTPSession, error) {
	if alloc == nil {
		return NewRTPSession()
	}
	var bindErr error
	for i := 0; i < rtpBindAttempts; i++ {
		port, err := alloc.Allocate()
		if err != nil {
			return nil, err
		}
		sess, err := NewRTPSessionOnPort(port)
		if err != nil {
			alloc.Release(port)
			bindErr = err
			continue
		}
		sess.allocator = alloc
		return sess, nil
	}
	return nil, bindErr
}

// getRemote returns the current remote address atomically.
func (s *RTPSession) getRemote() *net.UDPAddr {
	return (*net.UDPAddr)(atomic.LoadPointer(&s.remoteAddr))
}

// RemoteAddr returns the address media is currently sent to, or nil before the
// first SetRemote. Reflects symmetric-RTP latching as well as SDP negotiation.
func (s *RTPSession) RemoteAddr() *net.UDPAddr {
	return s.getRemote()
}

// setRemote stores the remote address atomically.
func (s *RTPSession) setRemote(addr *net.UDPAddr) {
	atomic.StorePointer(&s.remoteAddr, unsafe.Pointer(addr))
}

// SetRemote sets the remote address for sending RTP packets. ip may be
// either an IPv4 or IPv6 literal (or a hostname); IPv6 literals are
// bracketed via net.JoinHostPort.
func (s *RTPSession) SetRemote(ip string, port int) error {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("resolve remote: %w", err)
	}
	s.setRemote(addr)
	return nil
}

// ReadRTP reads and unmarshals an RTP packet from the UDP socket. Blocks
// until data arrives. Implements symmetric RTP: the remote address is
// latched to the source IP:port of each incoming RTP packet. The returned
// packet owns its memory.
func (s *RTPSession) ReadRTP() (*rtp.Packet, error) {
	buf := make([]byte, rtpBufSize)
	pkt := &rtp.Packet{}
	if err := s.readInto(pkt, buf); err != nil {
		return nil, err
	}
	return pkt, nil
}

// ReadRTPInto is ReadRTP without per-packet allocation: pkt is overwritten and
// its payload aliases a session buffer that is only valid until the next call.
// At most one goroutine may call it per session.
func (s *RTPSession) ReadRTPInto(pkt *rtp.Packet) error {
	if s.readBuf == nil {
		s.readBuf = make([]byte, rtpBufSize)
	}
	return s.readInto(pkt, s.readBuf)
}

func (s *RTPSession) readInto(pkt *rtp.Packet, buf []byte) error {
	n, src, err := s.conn.ReadFromUDPAddrPort(buf)
	if err != nil {
		return err
	}

	// RFC 5761 §4: RTCP muxed on the RTP port has its PT byte in 192..223.
	// It must not reach the RTP path, where it would unmarshal cleanly and
	// feed garbage to the payload decoder.
	if n >= 2 {
		pt2 := buf[1]
		if pt2 >= 192 && pt2 <= 223 {
			return s.readRTCP(buf[:n])
		}
	}

	if rx := s.rx.Load(); rx != nil {
		plain, err := rx.decrypt(buf[:n])
		if err != nil {
			return fmt.Errorf("%w: %w: %v", ErrNotRTP, ErrSRTPAuth, err)
		}
		n = len(plain)
	}

	if err := pkt.Unmarshal(buf[:n]); err != nil {
		return fmt.Errorf("%w: %v", ErrNotRTP, err)
	}
	if s.rtcpOn.Load() {
		s.rtcp.onReceive(&pkt.Header, time.Now())
	}

	// Latching only after decryption keeps an unauthenticated packet from
	// redirecting the media. Symmetric RTP: latch remote address to the source of incoming RTP.
	if cur := s.getRemote(); cur == nil || cur.AddrPort() != src {
		s.setRemote(net.UDPAddrFromAddrPort(src))
	}
	return nil
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
	if err := s.send(data, addr); err != nil {
		return err
	}
	s.noteSent(pkt)
	return nil
}

func (s *RTPSession) noteSent(pkt *rtp.Packet) {
	if s.rtcpOn.Load() {
		s.rtcp.onSend(&pkt.Header, len(pkt.Payload), time.Now())
	}
}

// send writes one marshalled RTP packet, encrypting it when SRTP is active.
func (s *RTPSession) send(data []byte, addr *net.UDPAddr) error {
	tx := s.tx.Load()
	if tx == nil {
		_, err := s.conn.WriteToUDP(data, addr)
		return err
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	out, err := tx.ctx.EncryptRTP(tx.buf, data, &tx.hdr)
	if err != nil {
		return fmt.Errorf("srtp encrypt: %w", err)
	}
	tx.buf = out[:0]
	_, err = s.conn.WriteToUDP(out, addr)
	return err
}

// WriteRTPBuf is WriteRTP marshalling into buf, which is grown only when too
// small. It returns the buffer for the caller to reuse on the next packet.
func (s *RTPSession) WriteRTPBuf(pkt *rtp.Packet, buf []byte) ([]byte, error) {
	addr := s.getRemote()
	if addr == nil {
		return buf, fmt.Errorf("remote address not set")
	}
	n := pkt.MarshalSize()
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := pkt.MarshalTo(buf); err != nil {
		return buf, fmt.Errorf("rtp marshal: %w", err)
	}
	if err := s.send(buf, addr); err != nil {
		return buf, err
	}
	s.noteSent(pkt)
	return buf, nil
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
		_ = s.send(data, addr)
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

// Close closes the UDP connection and releases the port back to the allocator,
// first sending an RTCP BYE when reports are on. Idempotent: subsequent calls
// are no-ops. Multiple rollback paths in the engine ladder can each call Close
// on the same session.
func (s *RTPSession) Close() error {
	s.closeOnce.Do(func() {
		if s.rtcpOn.Load() {
			_ = s.sendRTCP(true)
		}
		close(s.done)
		s.closeErr = s.conn.Close()
		if s.allocator != nil {
			s.allocator.Release(s.localPort)
		}
	})
	return s.closeErr
}

// srtpDir is the SRTP context for one direction of a session. pion contexts
// are not safe for concurrent use, so every use holds mu.
type srtpDir struct {
	mu      sync.Mutex
	ctx     *srtp.Context
	key     SDESCrypto
	hdr     rtp.Header
	rtcpHdr rtcp.Header
	buf     []byte
}

func newSRTPDir(c *SDESCrypto, decrypt bool) (*srtpDir, error) {
	keyLen, _, err := suiteKeyLens(c.Suite)
	if err != nil {
		return nil, err
	}
	var opts []srtp.ContextOption
	if len(c.MKI) > 0 {
		opts = append(opts, srtp.MasterKeyIndicator(c.MKI))
	}
	if decrypt {
		opts = append(opts, srtp.SRTPReplayProtection(srtpReplayWindow), srtp.SRTCPReplayProtection(srtpReplayWindow))
	}
	ctx, err := srtp.CreateContext(c.Key[:keyLen], c.Key[keyLen:], sdesSuites[c.Suite], opts...)
	if err != nil {
		return nil, fmt.Errorf("srtp context: %w", err)
	}
	d := &srtpDir{ctx: ctx, key: *c}
	if !decrypt {
		d.buf = make([]byte, 0, rtpBufSize+64)
	}
	return d, nil
}

const srtpReplayWindow = 128

// decrypt authenticates and decrypts pkt in place, returning the plain RTP.
func (d *srtpDir) decrypt(pkt []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ctx.DecryptRTP(pkt, pkt, &d.hdr)
}

// SetSRTP keys the session with SDES-SRTP: local encrypts what we send,
// remote decrypts what the peer sends. Passing nil for both returns the
// session to plain RTP. A direction whose key is unchanged keeps its context,
// so a re-INVITE that repeats the keys does not reset the rollover counter.
func (s *RTPSession) SetSRTP(local, remote *SDESCrypto) error {
	if local == nil && remote == nil {
		s.tx.Store(nil)
		s.rx.Store(nil)
		return nil
	}
	if local == nil || remote == nil {
		return fmt.Errorf("srtp needs both a local and a remote key")
	}
	var tx, rx *srtpDir
	if cur := s.tx.Load(); cur == nil || !cur.key.SameKey(local) {
		d, err := newSRTPDir(local, false)
		if err != nil {
			return fmt.Errorf("local key: %w", err)
		}
		tx = d
	}
	if cur := s.rx.Load(); cur == nil || !cur.key.SameKey(remote) {
		d, err := newSRTPDir(remote, true)
		if err != nil {
			return fmt.Errorf("remote key: %w", err)
		}
		rx = d
	}
	if tx != nil {
		s.tx.Store(tx)
	} else if cur := s.tx.Load(); cur.key.Tag != local.Tag {
		cur.mu.Lock()
		cur.key.Tag = local.Tag
		cur.mu.Unlock()
	}
	if rx != nil {
		s.rx.Store(rx)
	}
	return nil
}

// LocalCrypto returns the attribute keying what we send, or nil when the
// session carries plain RTP.
func (s *RTPSession) LocalCrypto() *SDESCrypto {
	d := s.tx.Load()
	if d == nil {
		return nil
	}
	d.mu.Lock()
	c := d.key
	d.mu.Unlock()
	return &c
}

// SRTPEnabled reports whether the session encrypts its media.
func (s *RTPSession) SRTPEnabled() bool { return s.tx.Load() != nil }
