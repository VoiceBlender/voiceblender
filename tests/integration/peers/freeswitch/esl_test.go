package freeswitch

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadESLMessage_HeadersAndBody(t *testing.T) {
	raw := "Content-Type: api/response\nContent-Length: 11\n\n+OK alright" +
		"Content-Type: command/reply\nReply-Text: +OK\n\n"
	r := bufio.NewReader(strings.NewReader(raw))

	m, err := readESLMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	if m.contentType() != "api/response" || string(m.Body) != "+OK alright" {
		t.Fatalf("first frame = %q / %q", m.contentType(), m.Body)
	}
	m, err = readESLMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	if m.contentType() != "command/reply" || m.Headers["Reply-Text"] != "+OK" || len(m.Body) != 0 {
		t.Fatalf("second frame = %+v", m)
	}
}

func TestReadESLMessage_SkipsLeadingBlankLines(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n\nContent-Type: auth/request\n\n"))
	m, err := readESLMessage(r)
	if err != nil || m.contentType() != "auth/request" {
		t.Fatalf("got %+v, %v", m, err)
	}
}

func TestReadESLMessage_Errors(t *testing.T) {
	for name, raw := range map[string]string{
		"no colon":       "garbage\n\n",
		"bad length":     "Content-Length: x\n\n",
		"negative":       "Content-Length: -1\n\n",
		"truncated body": "Content-Length: 10\n\nabc",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readESLMessage(bufio.NewReader(strings.NewReader(raw))); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestParsePlainEvent_DecodesValuesAndBody(t *testing.T) {
	body := "Event-Name: BACKGROUND_JOB\n" +
		"Job-UUID: 1234\n" +
		"variable_sip_h_X-Interop-Tag: it-1%20a%3Ab\n" +
		"Content-Length: 21\n" +
		"\n" +
		"-ERR USER_BUSY\nextra\n"
	ev, b, err := parsePlainEvent([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if ev["Event-Name"] != "BACKGROUND_JOB" || ev["Job-UUID"] != "1234" {
		t.Errorf("headers = %v", ev)
	}
	if got := ev["variable_sip_h_X-Interop-Tag"]; got != "it-1 a:b" {
		t.Errorf("url-decoded value = %q", got)
	}
	if b != "-ERR USER_BUSY\nextra\n" {
		t.Errorf("body = %q", b)
	}
}

func TestParsePlainEvent_KeepsUndecodableValue(t *testing.T) {
	ev, _, err := parsePlainEvent([]byte("Event-Name: X\nRaw: 100%\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ev["Raw"] != "100%" {
		t.Errorf("Raw = %q", ev["Raw"])
	}
}

// fakeESL plays the server side of the event socket for one connection.
func fakeESL(t *testing.T, password string, handle func(cmd string) string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "Content-Type: auth/request\n\n")
		r := bufio.NewReader(conn)
		for {
			var lines []string
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\n")
				if line == "" {
					break
				}
				lines = append(lines, line)
			}
			cmd := strings.Join(lines, "\n")
			if strings.HasPrefix(cmd, "auth ") {
				reply := "+OK accepted"
				if cmd != "auth "+password {
					reply = "-ERR invalid"
				}
				io.WriteString(conn, "Content-Type: command/reply\nReply-Text: "+reply+"\n\n")
				continue
			}
			out := handle(cmd)
			io.WriteString(conn, "Content-Type: api/response\nContent-Length: "+strconv.Itoa(len(out))+"\n\n"+out)
		}
	}()
	return ln.Addr().String()
}

func TestESLConn_AuthAndAPI(t *testing.T) {
	addr := fakeESL(t, "pw", func(cmd string) string {
		switch cmd {
		case "api status":
			return "UP\n"
		case "api uuid_kill nope":
			return "-ERR No such channel!\n"
		}
		return "-USAGE what\n"
	})
	c, err := dialESL(addr, "pw", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if out, err := c.api("status"); err != nil || out != "UP" {
		t.Errorf("api status = %q, %v", out, err)
	}
	if _, err := c.api("uuid_kill nope"); err == nil {
		t.Error("-ERR output should be an error")
	}
	if _, err := c.api("bogus"); err == nil {
		t.Error("-USAGE output should be an error")
	}
}

func TestESLConn_AuthRejected(t *testing.T) {
	addr := fakeESL(t, "right", func(string) string { return "" })
	if _, err := dialESL(addr, "wrong", 2*time.Second); err == nil {
		t.Fatal("want an auth error")
	}
}
