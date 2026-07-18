package api

import (
	"encoding/json"
	"errors"
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

// isDropLogThreshold returns true when n is a power of ten (1, 10, 100, …).
// Used to throttle the buffer-full warning: emit on first drop and on every
// 10× scale-up, keeping log volume bounded under sustained backpressure
// while still surfacing each escalation.
func isDropLogThreshold(n int64) bool {
	if n <= 0 {
		return false
	}
	for n > 1 {
		if n%10 != 0 {
			return false
		}
		n /= 10
	}
	return true
}

// vsiPingFrame builds the keepalive ping frame the VSI ping loop emits. The
// counter field is named "seq" rather than "event_id": streamed events on this
// socket carry an "event_id" of their own (the per-event idempotency key), and
// one socket must not advertise two meanings for that name.
func vsiPingFrame(seq int64) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"type": "ping",
		"seq":  seq,
	})
	return b
}

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
	// Fall back to the documented default when the config value is unset
	// (zero) — typically only happens in tests that build a Config{} directly.
	bufSize := s.Config.VSIEventBufferSize
	if bufSize <= 0 {
		bufSize = 256
	}
	ch := make(chan events.Event, bufSize)
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if appFilter != nil && !appFilter.MatchString(e.Data.GetAppID()) {
			return
		}
		select {
		case ch <- e:
		default:
			// Buffer full → drop. Log on the leading edge of a drop burst
			// (transition from 0 → 1) and on each power-of-10 threshold so
			// sustained backpressure is visible without flooding the log.
			// The counter, unlike the log, records every drop. This runs on
			// the publisher's goroutine (Bus.Publish is synchronous), so it
			// must stay non-blocking — a counter increment is.
			if s.Metrics != nil {
				s.Metrics.ObserveVSIDropped()
			}
			n := dropped.Add(1)
			if isDropLogThreshold(n) {
				s.Log.Warn("vsi: event buffer full, dropping event",
					"event_type", e.Type,
					"buffer_size", bufSize,
					"dropped_in_burst", n,
				)
			}
		}
	})
	defer unsub()

	lw := &wsLockedWriter{conn: conn}

	connMsg, _ := json.Marshal(map[string]string{"type": "connected"})
	if err := lw.writeText(connMsg); err != nil {
		s.Log.Error("vsi send connected failed", "error", err)
		return
	}

	appFilterStr := ""
	if appFilter != nil {
		appFilterStr = appFilter.String()
	}
	s.Log.Info("vsi client connected",
		"remote_addr", r.RemoteAddr,
		"app_filter", appFilterStr,
		"buffer_size", bufSize,
	)

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
		var seq int64
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if closed.Load() {
					return
				}
				seq++
				msg := vsiPingFrame(seq)
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
	s.Log.Info("vsi client disconnected",
		"remote_addr", r.RemoteAddr,
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
		wsutilx.SetReadDeadline(conn, wsutilx.DefaultReadTimeout.Load())

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
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "read_timeout"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return "peer_close"
	}
	return "error"
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
