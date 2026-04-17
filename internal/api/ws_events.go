package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// wsEventsInMsg is the wire format for client → server messages on the event
// WebSocket. The Type field selects the operation; RequestID, when set, is
// echoed back in the response so the client can correlate async replies.
// Payload carries command-specific data (unused in v1, reserved for future
// commands like originate/mute/dtmf).
type wsEventsInMsg struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// wsEventsOutMsg is the wire format for server → client response messages
// (distinct from streamed events which use the Event.MarshalJSON shape).
type wsEventsOutMsg struct {
	Type      string      `json:"type"`
	RequestID string      `json:"request_id,omitempty"`
	Data      interface{} `json:"data,omitempty"`
}

const wsEventsBufSize = 256

func (s *Server) wsEvents(w http.ResponseWriter, r *http.Request) {
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
		s.Log.Error("ws events upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	var dropped atomic.Int64
	ch := make(chan events.Event, wsEventsBufSize)
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
		s.Log.Error("ws events send connected failed", "error", err)
		return
	}

	s.Log.Info("ws events client connected")

	var closed atomic.Bool
	done := make(chan struct{})

	// Send loop: forward bus events as JSON text frames.
	// Before each event, check if any were dropped and notify the client.
	go func() {
		for {
			select {
			case e := <-ch:
				if n := dropped.Swap(0); n > 0 {
					s.Log.Warn("ws events: notifying client of dropped events", "count", n)
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
					s.Log.Warn("ws events marshal failed", "type", e.Type, "error", err)
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

	// Recv loop with typed dispatch.
	s.wsEventsRecvLoop(conn, lw, &closed)

	close(done)
	s.Log.Info("ws events client disconnected")
}

func (s *Server) wsEventsRecvLoop(conn io.ReadWriter, lw *wsLockedWriter, closed *atomic.Bool) {
	controlHandler := wsutil.ControlFrameHandler(conn, ws.StateServerSide)
	rd := &wsutil.Reader{
		Source: conn,
		State:  ws.StateServerSide,
		OnIntermediate: func(hdr ws.Header, r io.Reader) error {
			return controlHandler(hdr, r)
		},
	}

	for {
		hdr, err := rd.NextFrame()
		if err != nil {
			return
		}

		if hdr.OpCode.IsControl() {
			if err := controlHandler(hdr, rd); err != nil {
				return
			}
			continue
		}

		payload, err := io.ReadAll(rd)
		if err != nil {
			return
		}

		if hdr.OpCode != ws.OpText {
			continue
		}

		var msg wsEventsInMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			s.wsEventsSendResponse(lw, "", "error",
				map[string]string{"message": "invalid JSON"})
			continue
		}

		switch msg.Type {
		case "pong":
			continue
		case "stop":
			closed.Store(true)
			return
		default:
			s.wsEventsSendResponse(lw, msg.RequestID, "error",
				map[string]string{"message": "unknown command: " + msg.Type})
		}
	}
}

func (s *Server) wsEventsSendResponse(lw *wsLockedWriter, requestID, typ string, data interface{}) {
	resp := wsEventsOutMsg{
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
