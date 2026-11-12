package freeswitch

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// eslMessage is one frame of the event socket protocol: a header block, then
// Content-Length bytes of body.
type eslMessage struct {
	Headers map[string]string
	Body    []byte
}

func (m eslMessage) contentType() string { return m.Headers["Content-Type"] }

// readESLMessage reads one frame.
func readESLMessage(r *bufio.Reader) (eslMessage, error) {
	m := eslMessage{Headers: map[string]string{}}
	sawHeader := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return m, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !sawHeader {
				continue
			}
			break
		}
		sawHeader = true
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return m, fmt.Errorf("esl: malformed header line %q", line)
		}
		m.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if cl := m.Headers["Content-Length"]; cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 {
			return m, fmt.Errorf("esl: bad Content-Length %q", cl)
		}
		m.Body = make([]byte, n)
		if _, err := io.ReadFull(r, m.Body); err != nil {
			return m, err
		}
	}
	return m, nil
}

// parsePlainEvent decodes a text/event-plain body: URL-encoded headers, a
// blank line, then the event's own Content-Length bytes of body.
func parsePlainEvent(b []byte) (map[string]string, string, error) {
	ev := map[string]string{}
	r := bufio.NewReader(strings.NewReader(string(b)))
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, "", fmt.Errorf("esl: malformed event line %q", line)
		}
		dv, derr := url.QueryUnescape(strings.TrimSpace(v))
		if derr != nil {
			dv = strings.TrimSpace(v)
		}
		ev[strings.TrimSpace(k)] = dv
		if err != nil {
			break
		}
	}
	var body string
	if cl := ev["Content-Length"]; cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil {
			return nil, "", fmt.Errorf("esl: bad event Content-Length %q", cl)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, "", fmt.Errorf("esl: short event body: %w", err)
		}
		body = string(buf)
	}
	return ev, body, nil
}

// eslConn is an inbound event socket connection.
type eslConn struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex // serialises command/reply pairs
}

func dialESL(addr, password string, timeout time.Duration) (*eslConn, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	c := &eslConn{conn: conn, r: bufio.NewReader(conn)}
	conn.SetDeadline(time.Now().Add(timeout))
	m, err := readESLMessage(c.r)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("esl: read greeting: %w", err)
	}
	if m.contentType() != "auth/request" {
		conn.Close()
		return nil, fmt.Errorf("esl: unexpected greeting %q", m.contentType())
	}
	reply, err := c.command("auth " + password)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(reply.Headers["Reply-Text"], "+OK") {
		conn.Close()
		return nil, fmt.Errorf("esl: auth rejected: %s", reply.Headers["Reply-Text"])
	}
	conn.SetDeadline(time.Time{})
	return c, nil
}

func (c *eslConn) write(cmd string) error {
	_, err := io.WriteString(c.conn, cmd+"\n\n")
	return err
}

// command sends cmd and returns its reply. Only for a connection that is not
// subscribed to events, where the next frame is always the reply.
func (c *eslConn) command(cmd string) (eslMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.write(cmd); err != nil {
		return eslMessage{}, err
	}
	for {
		m, err := readESLMessage(c.r)
		if err != nil {
			return m, err
		}
		switch m.contentType() {
		case "command/reply", "api/response":
			return m, nil
		case "text/disconnect-notice":
			return m, errors.New("esl: disconnected")
		}
	}
}

// api runs an API command and returns its output. Output starting with -ERR
// or -USAGE is returned as an error.
func (c *eslConn) api(cmd string) (string, error) {
	m, err := c.command("api " + cmd)
	if err != nil {
		return "", err
	}
	out := strings.TrimRight(string(m.Body), "\n")
	if strings.HasPrefix(out, "-ERR") || strings.HasPrefix(out, "-USAGE") {
		return out, fmt.Errorf("esl api %q: %s", cmd, out)
	}
	return out, nil
}

func (c *eslConn) Close() error { return c.conn.Close() }
