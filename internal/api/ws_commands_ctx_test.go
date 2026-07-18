package api

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

// blackHoleEndpoint returns the address of a listener that accepts connections
// and then never replies. A refused or reset connection would fail instantly
// and prove nothing about the deadline, so the probe must be left to hang.
func blackHoleEndpoint(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})

	return "http://" + ln.Addr().String()
}

// TestWSRecordStartHonoursConnectionContext pins that the VSI record-start
// dispatch bounds the S3 bucket preflight by the connection's context rather
// than an uncancellable one. With context.Background() at the call site the
// probe burns its full 10s budget on the recv-loop goroutine, leaving every
// later command from that client unread.
//
// Both targets must exist: doStartRecordLeg/Room resolve the leg or room
// before the storage backend, so a missing one returns early and never
// reaches the preflight this is about.
func TestWSRecordStartHonoursConnectionContext(t *testing.T) {
	endpoint := blackHoleEndpoint(t)

	for _, tc := range []struct {
		name    string
		cmdType string
		id      string
		setup   func(t *testing.T, s *Server, id string)
	}{
		{
			name:    "leg",
			cmdType: "leg_record_start",
			id:      "leg-1",
			setup: func(t *testing.T, s *Server, id string) {
				s.LegMgr.Add(&apiMockLeg{id: id, createdAt: time.Now()})
			},
		},
		{
			name:    "room",
			cmdType: "room_record_start",
			id:      "room-1",
			setup: func(t *testing.T, s *Server, id string) {
				if _, err := s.RoomMgr.Create(id, "app", 16000); err != nil {
					t.Fatalf("create room: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			// The endpoint is plain http, which NewS3Backend rejects outright
			// unless the operator opted in — without this the call would fail
			// before ever probing.
			s.Config.S3AllowInsecureEndpoint = true
			tc.setup(t, s, tc.id)

			payload, err := json.Marshal(map[string]interface{}{
				"id":            tc.id,
				"storage":       "s3",
				"s3_bucket":     "b",
				"s3_endpoint":   endpoint,
				"s3_access_key": "k",
				"s3_secret_key": "s",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			msg := vsiInMsg{Type: tc.cmdType, Payload: payload}

			// Stands in for a connection whose client has gone away.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			lw := &wsLockedWriter{conn: discardConn{}}

			done := make(chan struct{})
			start := time.Now()
			go func() {
				defer close(done)
				s.wsHandleCommand(ctx, lw, msg)
			}()

			// Generous against an immediate return, far below the 10s preflight
			// budget the call falls back to when ctx is not threaded through.
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("%s still blocked after %v: the connection context was "+
					"ignored and the preflight ran on its own budget, stalling "+
					"the recv loop", tc.cmdType, time.Since(start))
			}
		})
	}
}

// discardConn is a net.Conn that swallows writes, so wsHandleCommand can emit
// its error response without a real socket.
type discardConn struct{ net.Conn }

func (discardConn) Write(b []byte) (int, error) { return len(b), nil }
func (discardConn) Close() error                { return nil }
