package api

import (
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/wsmedia"
	"github.com/go-chi/chi/v5"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
)

const (
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 10 * time.Second
)

// wsLockedWriter serializes all WebSocket frame writes to a net.Conn (server
// side). Kept here for the VSI commands path (internal/api/agent.go,
// internal/api/ws_events.go), which still hand-rolls WS framing for
// command/result messages. The room-WS handler itself uses wsmedia.Transport.
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

// wsRoom upgrades an HTTP request to a WebSocket and wires it as a raw
// participant of the named room. The wire protocol is identical to the
// /v1/legs/websocket endpoint (it goes through the same wsmedia.Transport
// in WireJSONBase64 mode):
//
//   - Welcome:           {"type":"connected","participant_id":"...",
//                         "sample_rate":N,"format":"pcm_s16le"}
//   - Inbound audio:     {"audio":"<base64-pcm>"} or
//                        {"type":"audio","audio":"<base64-pcm>"}
//   - Outbound audio:    {"audio":"<base64-pcm>"}
//   - Heartbeat:         server →{"type":"ping","event_id":N};
//                        client →{"type":"pong","event_id":N}
//   - Client close:      {"type":"stop"} (alias: {"type":"hangup"})
//   - Bidi text:         {"type":"text","text":"..."}
//
// The participant is a raw mixer slot — it does NOT show up in /v1/legs and
// does NOT receive leg lifecycle events. Use /v1/legs/websocket if you want
// a real leg.
func (s *Server) wsRoom(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")

	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	cfg := wsmedia.Config{
		SampleRate:   rm.Mixer().SampleRate(),
		WireFormat:   wsmedia.WireJSONBase64,
		SampleFormat: wsmedia.SampleS16LE,
		Log:          s.Log,
	}
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tr, _, err := wsmedia.UpgradeServer(w, r, cfg)
	if err != nil {
		s.Log.Error("ws upgrade failed", "error", err)
		return
	}

	participantID := "ws-" + uuid.New().String()[:8]

	// Mixer reads inbound PCM from the transport's paced ingress buffer
	// and writes mixed-minus-self into the egress pipe; the transport's
	// send loop reads from that pipe and ships PCM back to the client.
	listenPR, listenPW := io.Pipe()
	rm.Mixer().AddParticipant(participantID, tr.AudioReader(), listenPW)

	if err := tr.SendStructured(map[string]any{
		"type":           "connected",
		"participant_id": participantID,
		"sample_rate":    cfg.SampleRate,
		"format":         "pcm_s16le",
	}); err != nil {
		s.Log.Error("ws send connected failed", "error", err)
		s.wsCleanup(rm, participantID, tr, listenPW)
		return
	}

	tr.Start(listenPR)

	s.Log.Info("ws participant connected", "room_id", roomID, "participant_id", participantID)
	connectedAt := time.Now()

	<-tr.Done()

	s.Log.Info("session closed",
		"kind", "ws_room",
		"room_id", roomID,
		"participant_id", participantID,
		"reason", classifyWSReason(tr.Err()),
		"duration_ms", time.Since(connectedAt).Milliseconds(),
	)
	s.wsCleanup(rm, participantID, tr, listenPW)
}

func (s *Server) wsCleanup(rm interface{ Mixer() *mixer.Mixer }, participantID string, tr *wsmedia.Transport, listenPW *io.PipeWriter) {
	_ = listenPW.Close()
	_ = tr.Close()
	rm.Mixer().RemoveParticipant(participantID)
}
