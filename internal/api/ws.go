package api

import (
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/wsmedia"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/go-chi/chi/v5"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
)

const (
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 10 * time.Second
)

// vsiWriteTimeout bounds every server → client write on the VSI WebSocket.
// The value is shared with internal/wsmedia rather than re-invented: both are
// long-lived sockets carrying small JSON frames, so one number is easier to
// reason about than two. It is a wsutilx.DurationVar — the same shape as
// wsutilx.DefaultReadTimeout — so tests can shorten it without racing the
// handler goroutines that read it. Nothing outside tests writes it.
var vsiWriteTimeout wsutilx.DurationVar

func init() { vsiWriteTimeout.Store(wsmedia.DefaultWriteTimeout) }

// wsLockedWriter serializes all WebSocket frame writes to a net.Conn (server
// side). Kept here for the VSI commands path (internal/api/agent.go,
// internal/api/ws_events.go), which still hand-rolls WS framing for
// command/result messages. The room-WS handler itself uses wsmedia.Transport.
//
// The mutex is deliberately held across the write: gobwas emits one frame as a
// header write followed by a payload write, so releasing in between would let
// another goroutine interleave its header into this frame. The hold is bounded
// by the write deadline armed immediately before the write, and a write that
// misses that deadline is fatal to the connection — see fail.
//
// If internal/wsutilx ever grows a shared locked writer it will be the client
// side (masked frames, WriteClientText) and cannot serve this call site;
// converging means adding a server-side constructor beside it and deleting
// this type.
type wsLockedWriter struct {
	mu      sync.Mutex
	conn    net.Conn
	timeout time.Duration

	failOnce sync.Once
	writeErr atomic.Pointer[error]
}

func newWSLockedWriter(conn net.Conn, timeout time.Duration) *wsLockedWriter {
	return &wsLockedWriter{conn: conn, timeout: timeout}
}

func (lw *wsLockedWriter) writeText(data []byte) error {
	lw.mu.Lock()
	setWSWriteDeadline(lw.conn, lw.timeout)
	err := wsutil.WriteServerText(lw.conn, data)
	lw.mu.Unlock()
	if err != nil {
		// Outside the lock: fail closes the conn, which is a call that can
		// block, and nothing else may wait on it while the mutex is held.
		lw.fail(err)
	}
	return err
}

func (lw *wsLockedWriter) writeControl(op ws.OpCode, payload []byte) error {
	lw.mu.Lock()
	setWSWriteDeadline(lw.conn, lw.timeout)
	err := wsutil.WriteServerMessage(lw.conn, op, payload)
	lw.mu.Unlock()
	if err != nil {
		lw.fail(err)
	}
	return err
}

// fail records the first write failure and closes the connection. Closing is
// what unblocks the recv loop's blocking read, so the handler can return and
// run its deferred unsubscribe instead of continuing to serve commands onto a
// stream that may carry a half-written frame. Once semantics matter: after the
// first failure every writer still blocked on this conn wakes with a
// consequential error and calls fail too, and only the root cause is kept.
func (lw *wsLockedWriter) fail(err error) {
	lw.failOnce.Do(func() {
		lw.writeErr.Store(&err)
		_ = lw.conn.Close()
	})
}

// Err returns the first write error recorded by fail, or nil if every write so
// far has succeeded.
func (lw *wsLockedWriter) Err() error {
	if p := lw.writeErr.Load(); p != nil {
		return *p
	}
	return nil
}

// setWSWriteDeadline pushes conn's write deadline forward by timeout. A
// non-positive timeout is a no-op (the caller manages deadlines). Mirrors the
// unexported helper of the same shape in internal/wsmedia, which cannot be
// reused across the package boundary.
func setWSWriteDeadline(conn net.Conn, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
}

// wsRoom upgrades an HTTP request to a WebSocket and wires it as a raw
// participant of the named room. The wire protocol is identical to the
// /v1/legs/websocket endpoint (it goes through the same wsmedia.Transport
// in WireJSONBase64 mode):
//
//   - Welcome:           {"type":"connected","participant_id":"...",
//     "sample_rate":N,"format":"pcm_s16le"}
//   - Inbound audio:     {"audio":"<base64-pcm>"} or
//     {"type":"audio","audio":"<base64-pcm>"}
//   - Outbound audio:    {"audio":"<base64-pcm>"}
//   - Heartbeat:         server →{"type":"ping","event_id":N};
//     client →{"type":"pong","event_id":N}
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
