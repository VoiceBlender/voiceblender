//go:build integration

package integration

import (
	"bufio"
	"context"
	"io"
	"net"

	"github.com/gobwas/ws"
)

// wsDial dials a WebSocket endpoint and returns a conn that is safe to read
// frames from immediately.
//
// gobwas/ws reads the HTTP 101 response through a bufio.Reader and hands it
// back when it still holds unconsumed bytes. Every VoiceBlender WS endpoint
// writes its first frame the instant the upgrade completes, so that frame
// routinely arrives in the same TCP segment as the handshake reply and ends
// up in that buffer. Discarding the reader therefore either loses the frame
// entirely (the next read blocks until the deadline) or leaves the stream
// mid-frame (the next read reports a reserved opcode) — the same dial racing
// the same server, sometimes fine, sometimes not.
func wsDial(ctx context.Context, url string) (net.Conn, error) {
	return wsDialWith(ctx, url, ws.DefaultDialer)
}

// wsDialWith is wsDial with a caller-supplied dialer (custom headers, etc).
func wsDialWith(ctx context.Context, url string, d ws.Dialer) (net.Conn, error) {
	conn, br, _, err := d.Dial(ctx, url)
	if err != nil {
		return nil, err
	}
	return withBufferedReader(conn, br), nil
}

func withBufferedReader(conn net.Conn, br *bufio.Reader) net.Conn {
	if br == nil || br.Buffered() == 0 {
		return conn
	}
	return &bufferedConn{Conn: conn, r: br}
}

// bufferedConn serves the handshake leftovers before the socket itself.
// Deadlines, writes, and Close still go straight to the embedded conn.
type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
