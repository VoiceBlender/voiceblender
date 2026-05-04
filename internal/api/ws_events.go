package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// vsiInMsg is the wire format for client → server messages on the VSI
// WebSocket. The Type field selects the operation; RequestID, when set, is
// echoed back in the response so the client can correlate async replies.
// Payload carries command-specific data.
type vsiInMsg struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// vsiOutMsg is the wire format for server → client response messages
// (distinct from streamed events which use the Event.MarshalJSON shape).
type vsiOutMsg struct {
	Type      string      `json:"type"`
	RequestID string      `json:"request_id,omitempty"`
	Data      interface{} `json:"data,omitempty"`
}

const vsiBufSize = 256

func (s *Server) vsi(w http.ResponseWriter, r *http.Request) {
	// Parse optional app_id regex filter before upgrade so we can reject with 400.
	var appFilter *regexp.Regexp
	if pattern := r.URL.Query().Get("app_id"); pattern != "" {
		var err error
		appFilter, err = regexp.Compile(pattern)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid app_id regex: %v", err))
			return
		}
	}

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		s.Log.Error("vsi upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	connectedAt := time.Now()

	var dropped atomic.Int64
	ch := make(chan events.Event, vsiBufSize)
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if appFilter != nil && !appFilter.MatchString(e.Data.GetAppID()) {
			return
		}
		select {
		case ch <- e:
		default:
			dropped.Add(1)
		}
	})
	defer unsub()

	lw := &wsLockedWriter{conn: conn}

	connMsg, _ := json.Marshal(map[string]string{"type": "connected"})
	if err := lw.writeText(connMsg); err != nil {
		s.Log.Error("vsi send connected failed", "error", err)
		return
	}

	s.Log.Info("vsi client connected")

	var closed atomic.Bool
	done := make(chan struct{})

	// Send loop: forward bus events as JSON text frames.
	// Before each event, check if any were dropped and notify the client.
	go func() {
		for {
			select {
			case e := <-ch:
				if n := dropped.Swap(0); n > 0 {
					s.Log.Warn("vsi: notifying client of dropped events", "count", n)
					notice, _ := json.Marshal(map[string]interface{}{
						"type":  "events_dropped",
						"count": n,
					})
					if err := lw.writeText(notice); err != nil {
						return
					}
				}
				data, err := json.Marshal(e)
				if err != nil {
					s.Log.Warn("vsi marshal failed", "type", e.Type, "error", err)
					continue
				}
				if err := lw.writeText(data); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Ping loop.
	go func() {
		var eventID int64
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if closed.Load() {
					return
				}
				eventID++
				msg, _ := json.Marshal(map[string]interface{}{
					"type":     "ping",
					"event_id": eventID,
				})
				if err := lw.writeText(msg); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Recv loop with typed dispatch. Returns when the client sends a "stop"
	// command, the WebSocket frame parse fails, or the read deadline
	// elapses (zombie connection / network partition).
	reason := s.vsiRecvLoop(conn, lw, &closed)

	close(done)
	s.Log.Info("session closed",
		"kind", "vsi",
		"reason", reason,
		"duration_ms", time.Since(connectedAt).Milliseconds(),
	)
}

// vsiRecvLoop reads frames from the VSI client and dispatches commands.
// Returns a short string describing why the loop exited, suitable for the
// "reason" field of the structured shutdown log: "stop" (client sent stop
// command), "read_timeout" (idle deadline elapsed — zombie connection),
// "peer_close" (clean WS close), or "error" (other read/parse error).
func (s *Server) vsiRecvLoop(conn net.Conn, lw *wsLockedWriter, closed *atomic.Bool) string {
	controlHandler := wsutil.ControlFrameHandler(conn, ws.StateServerSide)
	rd := &wsutil.Reader{
		Source: conn,
		State:  ws.StateServerSide,
		OnIntermediate: func(hdr ws.Header, r io.Reader) error {
			return controlHandler(hdr, r)
		},
	}

	for {
		// Refresh deadline before each blocking read. Without this, a
		// half-open client TCP wedges this goroutine and (via the close-on-
		// return cleanup) every other goroutine pinned to the connection.
		wsutilx.SetReadDeadline(conn, wsutilx.DefaultReadTimeout)

		hdr, err := rd.NextFrame()
		if err != nil {
			return classifyReadError(err)
		}

		if hdr.OpCode.IsControl() {
			if err := controlHandler(hdr, rd); err != nil {
				return classifyReadError(err)
			}
			continue
		}

		payload, err := io.ReadAll(rd)
		if err != nil {
			return classifyReadError(err)
		}

		if hdr.OpCode != ws.OpText {
			continue
		}

		var msg vsiInMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			s.vsiSendResponse(lw, "", "error",
				map[string]string{"message": "invalid JSON"})
			continue
		}

		switch msg.Type {
		case "pong":
			continue
		case "stop":
			closed.Store(true)
			return "stop"
		default:
			s.wsHandleCommand(lw, msg)
		}
	}
}

// classifyReadError maps a recv-loop read error to a short reason label.
// Used by structured shutdown logs so operators can distinguish clean
// disconnects from idle-timeout (zombie connection) closures.
func classifyReadError(err error) string {
	if err == nil {
		return "unknown"
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "read_timeout"
	}
	if err == io.EOF || errIsClosedConn(err) {
		return "peer_close"
	}
	return "error"
}

func errIsClosedConn(err error) bool {
	// net.ErrClosed exposes via "use of closed network connection"; EOF on
	// a half-closed conn is similar. Match on substring to avoid an extra
	// import.
	return err != nil && (err.Error() == "use of closed network connection" ||
		err.Error() == "EOF")
}

func (s *Server) vsiSendResponse(lw *wsLockedWriter, requestID, typ string, data interface{}) {
	resp := vsiOutMsg{
		Type:      typ,
		RequestID: requestID,
		Data:      data,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	lw.writeText(b)
}
