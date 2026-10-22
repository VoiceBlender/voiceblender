package turndetector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	wsStreamPath = "/v1/ws/stream"

	// wsPipeCap is the number of 640-byte frames the send pipe can buffer before
	// dropping. At 20ms/frame this is 2 seconds of audio. The pipe drops rather
	// than blocks so the RTP receive goroutine never stalls.
	wsPipeCap = 100

	// wsPingInterval is the interval between client-initiated WebSocket pings.
	// Must be less than wsutilx.DefaultReadTimeout (60s) so the server's read
	// deadline is refreshed before it expires on a quiet call.
	wsPingInterval = 30 * time.Second

	// wsMaxReconnectAttempts is the number of dial+session attempts before giving
	// up and leaving detection permanently inactive for this leg.
	wsMaxReconnectAttempts = 5

	// wsHandshakeGrace is the window after a successful upgrade in which the
	// server must send at least one frame before we treat the closure as a
	// startup error (e.g. missing Silero model file).
	wsHandshakeGrace = 500 * time.Millisecond
)

// wsTurnEvent is the JSON shape pushed by the SmartTurn WebSocket endpoint
// on each turn decision.
type wsTurnEvent struct {
	Event         string  `json:"event"`
	Action        string  `json:"action"`
	Probability   float64 `json:"probability,omitempty"`
	ThresholdUsed float64 `json:"threshold_used"`
	EvaluationMs  int64   `json:"evaluation_ms,omitempty"`
}

// wsPipeWriter is a lock-free, non-blocking write end of a frame pipe.
// It is a minimal copy of recording.pipeWriter kept here to avoid a
// cross-package dependency on internal/api.
type wsPipeWriter struct {
	ch      chan []byte
	done    chan struct{}
	closed  atomic.Bool
	offered atomic.Uint64
	dropped atomic.Uint64
}

func (w *wsPipeWriter) Write(p []byte) (int, error) {
	if w.closed.Load() {
		return len(p), nil
	}
	data := make([]byte, len(p))
	copy(data, p)
	w.offered.Add(1)
	select {
	case w.ch <- data:
	case <-w.done:
	default:
		// Drop rather than block: the RTP receive goroutine must never stall.
		w.dropped.Add(1)
	}
	return len(p), nil
}

func (w *wsPipeWriter) close() {
	if w.closed.CompareAndSwap(false, true) {
		close(w.done)
	}
}

func (w *wsPipeWriter) stats() (offered, dropped uint64) {
	return w.offered.Load(), w.dropped.Load()
}

// newWsPipe allocates a matched pipe pair.
func newWsPipe() (ch chan []byte, pw *wsPipeWriter) {
	ch = make(chan []byte, wsPipeCap)
	done := make(chan struct{})
	pw = &wsPipeWriter{ch: ch, done: done}
	return ch, pw
}

// WSProvider streams raw 20ms PCM frames to the SmartTurn WebSocket endpoint
// and relays turn_decision events back to the caller via the onEvent callback.
//
// SmartTurn runs its own VAD and ML inference server-side; the provider's only
// job is forwarding audio frames and parsing the decision events.
//
// Concurrency: Write is called from the leg's audio tap goroutine; Start is
// called once during leg setup. Stop may be called from any goroutine.
//
// DTX / silence suppression note: the frame counter inside SmartTurn advances
// only when binary frames arrive. Codecs with DTX (e.g. Opus) or SIP silence
// suppression do not send packets during silence, so the pause counter never
// advances and no turn decision fires. This is a known constraint of the WS
// transport; zero-filling during packet gaps is not currently implemented.
type WSProvider struct {
	log  *slog.Logger
	opts Options

	mu       sync.Mutex
	running  bool
	cancel   context.CancelFunc
	pw       *wsPipeWriter // non-nil between Start and close; Write enqueues here
	frameBuf []byte        // accumulates PCM bytes to flush exact 640-byte frames
}

// NewWS creates a WSProvider. Call Start before writing audio.
func NewWS(log *slog.Logger) *WSProvider {
	return &WSProvider{log: log}
}

// SetOptions replaces the provider options. Must be called before Start.
func (p *WSProvider) SetOptions(opts Options) {
	p.mu.Lock()
	p.opts = opts
	p.mu.Unlock()
}

// Start dials the SmartTurn WebSocket endpoint and begins streaming.
// It blocks until the first connection attempt is resolved, returning an error
// if the initial dial fails. Subsequent reconnects after session drops happen
// in the background goroutine without blocking the caller.
func (p *WSProvider) Start(ctx context.Context, onEvent func(Event)) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil
	}
	opts := p.opts
	p.mu.Unlock()

	if opts.ServiceURL == "" {
		return fmt.Errorf("turndetector: SMART_TURN_URL is not set")
	}

	ctx, cancel := context.WithCancel(ctx)

	// ready is signalled exactly once after the first dial attempt.
	// A nil error means connected; non-nil means the first dial failed (Start returns the error).
	ready := make(chan error, 1)

	// Reconnect loop: attempt up to wsMaxReconnectAttempts connections.
	go func() {
		defer cancel()
		backoff := time.Second
		firstAttempt := true
		for attempt := 1; attempt <= wsMaxReconnectAttempts; attempt++ {
			if ctx.Err() != nil {
				if firstAttempt {
					ready <- ctx.Err()
					firstAttempt = false
				}
				return
			}

			wsURL := buildWSURL(opts)
			if attempt > 1 {
				p.log.Info("smartturn ws reconnecting",
					"attempt", attempt,
					"max", wsMaxReconnectAttempts,
					"url", wsURL)
			} else {
				p.log.Info("smartturn ws dialing", "url", wsURL)
			}

			conn, _, _, err := ws.Dial(ctx, wsURL)
			if err != nil {
				if firstAttempt {
					ready <- err
					firstAttempt = false
				}
				if ctx.Err() != nil {
					return
				}
				p.log.Warn("smartturn ws dial failed",
					"err", err, "attempt", attempt)
				p.sleepBackoff(ctx, backoff)
				backoff = min(backoff*2, 16*time.Second)
				continue
			}
			p.log.Info("smartturn ws connected")

			// Set up the pipe and sender goroutine for this connection.
			pipeRead, pw := newWsPipe()
			lw := wsutilx.NewLockedWriter(conn)

			p.mu.Lock()
			p.running = true
			p.cancel = cancel
			p.pw = pw
			p.frameBuf = make([]byte, 0, 640*4)
			p.mu.Unlock()

			// Signal Start that the connection is up.
			if firstAttempt {
				ready <- nil
				firstAttempt = false
			}

			// Sender goroutine: drains the pipe and writes frames to the WS conn.
			// On write error, tears the session down (H3).
			senderDone := make(chan struct{})
			go func() {
				defer close(senderDone)
				for {
					select {
					case frame, ok := <-pipeRead:
						if !ok {
							return
						}
						if err := lw.WriteBinary(frame); err != nil {
							p.log.Warn("smartturn ws write error",
								"err", err, "bytes", len(frame))
							conn.Close()
							return
						}
					case <-ctx.Done():
						return
					}
				}
			}()

			// Ping goroutine: keeps the connection alive during quiet calls (H1).
			pingDone := make(chan struct{})
			go func() {
				defer close(pingDone)
				ticker := time.NewTicker(wsPingInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if err := lw.WriteControl(ws.OpPing, nil); err != nil {
							// Write error is already handled by the sender goroutine;
							// just stop pinging.
							return
						}
					case <-ctx.Done():
						return
					case <-senderDone:
						return
					}
				}
			}()

			// Read loop for this session.
			sessionOK := p.runReadLoop(ctx, conn, onEvent)

			// Clean up this session's goroutines.
			pw.close()
			conn.Close()
			<-senderDone
			<-pingDone

			p.mu.Lock()
			p.running = false
			p.pw = nil
			p.frameBuf = nil
			p.mu.Unlock()

			if ctx.Err() != nil || sessionOK {
				return
			}

			if opts.OnError != nil {
				opts.OnError("ws_session_dropped", "websocket session ended; reconnecting", false)
			}

			// Retry with backoff.
			p.log.Warn("smartturn ws session ended; will retry",
				"attempt", attempt, "max", wsMaxReconnectAttempts)
			p.sleepBackoff(ctx, backoff)
			backoff = min(backoff*2, 16*time.Second)
		}

		p.log.Error("smartturn ws: exhausted reconnect attempts; turn detection is inactive",
			"max_attempts", wsMaxReconnectAttempts)
		if opts.OnError != nil {
			opts.OnError("ws_reconnect_exhausted", "exhausted reconnect attempts; turn detection is inactive", true)
		}
		// Signal Start in the unlikely case all attempts fail before ready fires.
		if firstAttempt {
			ready <- fmt.Errorf("smartturn ws: exhausted %d reconnect attempts", wsMaxReconnectAttempts)
		}
	}()

	// Block until the first connection attempt resolves.
	return <-ready
}

// runReadLoop runs the WebSocket read loop for one session. Returns true if the
// loop exited cleanly (context cancelled), false if the connection dropped and
// the caller should retry.
func (p *WSProvider) runReadLoop(ctx context.Context, conn net.Conn, onEvent func(Event)) bool {
	// Detect immediate post-handshake close (H4): if the server closes the
	// connection within wsHandshakeGrace before sending any frame, it likely
	// failed to initialise (e.g. missing Silero model file).
	frameReceived := false
	sessionStart := time.Now()

	for {
		if ctx.Err() != nil {
			return true
		}
		wsutilx.SetReadDeadline(conn, wsutilx.DefaultReadTimeout.Load())

		hdr, reader, err := wsutil.NextReader(conn, ws.StateClientSide)
		if err != nil {
			if ctx.Err() != nil {
				return true
			}
			if !frameReceived && time.Since(sessionStart) < wsHandshakeGrace {
				p.log.Error("smartturn ws: connection closed immediately after upgrade; "+
					"check that the configured VAD model (e.g. silero_vad.onnx) is present on the service",
					"err", err)
				return false
			}
			p.log.Warn("smartturn ws recv error", "err", err)
			return false
		}
		frameReceived = true

		if hdr.OpCode.IsControl() {
			if err := wsutil.ControlFrameHandler(conn, ws.StateClientSide)(hdr, reader); err != nil {
				p.log.Warn("smartturn ws control frame error", "err", err)
				return false
			}
			continue
		}
		if hdr.OpCode != ws.OpText {
			continue
		}

		var ev wsTurnEvent
		if err := json.NewDecoder(reader).Decode(&ev); err != nil {
			p.log.Warn("smartturn ws json decode error", "err", err)
			continue
		}
		if ev.Event != "turn_decision" {
			continue
		}

		onEvent(Event{
			Complete:      ev.Action == "TURN_COMPLETE",
			Probability:   ev.Probability,
			ThresholdUsed: ev.ThresholdUsed,
			ProcessingMs:  ev.EvaluationMs,
		})
	}
}

// sleepBackoff sleeps for d, returning early if ctx is cancelled.
func (p *WSProvider) sleepBackoff(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// Write sends raw PCM audio to SmartTurn as binary WebSocket messages.
// Audio is buffered and delivered in exact 20ms frames (640 bytes @ 16kHz Linear PCM).
// Write never blocks; frames are dropped if the send pipe is full (H2).
func (p *WSProvider) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running || p.pw == nil {
		return len(b), nil
	}

	p.frameBuf = append(p.frameBuf, b...)
	const frameBytes = 640 // 320 samples @ 16kHz = 20ms

	for len(p.frameBuf) >= frameBytes {
		frame := make([]byte, frameBytes)
		copy(frame, p.frameBuf[:frameBytes])
		p.pw.Write(frame) // non-blocking: drops if pipe is full
		p.frameBuf = p.frameBuf[frameBytes:]
	}
	return len(b), nil
}

// Stop closes the WebSocket connection and terminates streaming.
func (p *WSProvider) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	pw := p.pw
	p.running = false
	p.pw = nil
	p.frameBuf = nil
	p.mu.Unlock()

	if pw != nil {
		offered, dropped := pw.stats()
		if dropped > 0 && p.log != nil {
			p.log.Warn("smartturn ws: frames dropped (sender could not keep up)",
				"offered", offered, "dropped", dropped)
		}
		pw.close()
	}
	if cancel != nil {
		cancel()
	}
}

// Running reports whether the WebSocket session is active.
func (p *WSProvider) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// buildWSURL constructs the SmartTurn WebSocket stream URL from Options.
func buildWSURL(opts Options) string {
	base := strings.TrimRight(opts.ServiceURL, "/")
	// Replace http:// with ws:// and https:// with wss://.
	switch {
	case len(base) > 8 && base[:8] == "https://":
		base = "wss://" + base[8:]
	case len(base) > 7 && base[:7] == "http://":
		base = "ws://" + base[7:]
	}
	base = strings.TrimSuffix(base, wsStreamPath)

	vad := opts.VAD
	if vad == "" {
		vad = "silero"
	}
	wsURL := base + wsStreamPath + "?vad=" + url.QueryEscape(vad)

	if opts.Threshold > 0 {
		wsURL += "&threshold=" + strconv.FormatFloat(opts.Threshold, 'f', -1, 64)
	}
	if opts.PauseDurationMs > 0 {
		wsURL += "&pause_duration=" + strconv.Itoa(opts.PauseDurationMs) + "ms"
	}
	if opts.Adaptive {
		wsURL += "&adaptive=true"
	}

	return wsURL
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
