package api

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// wsHandleCommand runs on the connection's recv loop, so anything it blocks on
// blocks every later command from that client. A record-start whose S3 endpoint
// accepts the connection and never answers must therefore come back on the
// configured probe budget, and immediately once the connection is gone.
//
// Both targets are exercised: doStartRecordLeg/Room resolve the leg or room
// before the storage backend, so a missing one returns early and never reaches
// the probe this is about.
func TestWSRecordStartDoesNotStallRecvLoop(t *testing.T) {
	targets := []struct {
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
	}

	cases := []struct {
		name string
		// deadConn stands in for a connection whose client has gone away.
		deadConn bool
	}{
		{name: "live connection bounded by the probe budget"},
		{name: "dead connection returns at once", deadConn: true},
	}

	for _, target := range targets {
		for _, tc := range cases {
			t.Run(target.name+"/"+tc.name, func(t *testing.T) {
				s := newTestServer(t)
				s.Config.S3RequestPreflightTimeout = 200 * time.Millisecond
				target.setup(t, s, target.id)

				payload, err := json.Marshal(map[string]any{
					"id":            target.id,
					"storage":       "s3",
					"s3_bucket":     "b",
					"s3_endpoint":   blackHoleEndpoint(t),
					"s3_access_key": "k",
					"s3_secret_key": "s",
				})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				msg := vsiInMsg{Type: target.cmdType, Payload: payload}

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.deadConn {
					cancel()
				}

				lw := newWSLockedWriter(discardConn{}, 2*time.Second)

				done := make(chan struct{})
				start := time.Now()
				go func() {
					defer close(done)
					s.wsHandleCommand(ctx, lw, msg)
				}()

				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatalf("%s still blocked after %v: the recv loop is stalled on the S3 probe",
						target.cmdType, time.Since(start))
				}
			})
		}
	}
}

// discardConn is a net.Conn that swallows writes, so wsHandleCommand can emit
// its response without a real socket. The embedded net.Conn is nil, so every
// method the writer touches has to be overridden here or it panics at runtime
// — which no compile-time gate catches.
type discardConn struct{ net.Conn }

func (discardConn) Write(b []byte) (int, error)      { return len(b), nil }
func (discardConn) Close() error                     { return nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }
