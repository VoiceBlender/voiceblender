package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/go-chi/chi/v5"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
)

const (
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 10 * time.Second
)

// wsLockedWriter serializes all WebSocket frame writes to a net.Conn (server side).
type wsLockedWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (lw *wsLockedWriter) writeText(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteServerText(lw.conn, data)
}

func (lw *wsLockedWriter) writeControl(op ws.OpCode, payload []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteServerMessage(lw.conn, op, payload)
}

func (s *Server) wsRoom(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")

	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		s.Log.Error("ws upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	participantID := "ws-" + uuid.New().String()[:8]

	// speak buffer: WS client writes PCM → paced streamBuffer → mixer reads
	speakBuf := newStreamBuffer()
	// listen pipe: mixer writes mixed-minus-self → we read and send to WS client
	listenPR, listenPW := createPipe()

	rm.Mixer().AddParticipant(participantID, speakBuf, listenPW)

	lw := &wsLockedWriter{conn: conn}

	// Send connected message.
	connMsg, _ := json.Marshal(map[string]interface{}{
		"type":           "connected",
		"participant_id": participantID,
		"sample_rate":    rm.Mixer().SampleRate(),
		"format":         "pcm_s16le",
	})
	if err := lw.writeText(connMsg); err != nil {
		s.Log.Error("ws send connected failed", "error", err)
		s.wsCleanup(rm, participantID, speakBuf, listenPW)
		return
	}

	s.Log.Info("ws participant connected", "room_id", roomID, "participant_id", participantID)

	var closed atomic.Bool

	// Send loop: read from mixer listen pipe → base64 encode → send JSON.
	go func() {
		buf := make([]byte, rm.Mixer().FrameSizeBytes())
		for {
			n, err := listenPR.Read(buf)
			if err != nil || closed.Load() {
				return
			}
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			msg, _ := json.Marshal(map[string]string{"audio": encoded})
			if err := lw.writeText(msg); err != nil {
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
			}
		}
	}()

	// Recv loop: read from WebSocket → decode JSON → base64 decode → write to speakBuf.
	s.wsRecvLoop(conn, lw, speakBuf, &closed)

	s.Log.Info("ws participant disconnected", "room_id", roomID, "participant_id", participantID)
	s.wsCleanup(rm, participantID, speakBuf, listenPW)
}

type wsAudioMsg struct {
	Audio string `json:"audio,omitempty"`
	Type  string `json:"type,omitempty"`
}

func (s *Server) wsRecvLoop(conn net.Conn, lw *wsLockedWriter, speakBuf *streamBuffer, closed *atomic.Bool) {
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

		var msg wsAudioMsg
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}

		if msg.Type == "stop" {
			return
		}

		if msg.Type == "pong" {
			continue
		}

		if msg.Audio == "" {
			continue
		}

		pcm, err := base64.StdEncoding.DecodeString(msg.Audio)
		if err != nil {
			s.Log.Warn("ws invalid base64 audio", "error", err)
			continue
		}

		speakBuf.Write(pcm)
	}
}

func (s *Server) wsCleanup(rm interface{ Mixer() *mixer.Mixer }, participantID string, speakBuf *streamBuffer, listenPW *pipeWriter) {
	speakBuf.Close()
	listenPW.Close()
	rm.Mixer().RemoveParticipant(participantID)
}
