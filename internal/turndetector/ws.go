package turndetector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	wsStreamPath = "/v1/ws/stream"
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

// WSProvider streams raw 20ms PCM frames to the SmartTurn WebSocket endpoint
// and relays turn_decision events back to the caller via the onEvent callback.
//
// SmartTurn runs its own VAD and ML inference server-side; the provider's only
// job is forwarding audio frames and parsing the decision events.
//
// Concurrency: Write is called from the leg's audio tap goroutine; Start is
// called once during leg setup. Stop may be called from any goroutine.
type WSProvider struct {
	log  *slog.Logger
	opts Options

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	lw      *wsutilx.LockedWriter // guarded by mu; non-nil between Start and close
	buf     []byte                // accumulates 16kHz PCM bytes to flush exact 640-byte (20ms) frames
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
// It launches a read goroutine and returns once the connection is established.
// The caller must consume events via onEvent from a goroutine that is safe
// to block briefly (it is called inline from the read goroutine).
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

	wsURL := buildWSURL(opts)
	p.log.Info("smartturn ws dialing", "url", wsURL)
	conn, _, _, err := ws.Dial(ctx, wsURL)
	if err != nil {
		return fmt.Errorf("smartturn ws dial failed: %w", err)
	}
	p.log.Info("smartturn ws connected")

	ctx, cancel := context.WithCancel(ctx)

	lw := wsutilx.NewLockedWriter(conn)

	p.mu.Lock()
	p.running = true
	p.cancel = cancel
	p.lw = lw
	p.buf = make([]byte, 0, 640*4)
	p.mu.Unlock()

	// Read goroutine: receives JSON turn_decision events from SmartTurn.
	go func() {
		defer func() {
			conn.Close()
			p.mu.Lock()
			p.running = false
			p.lw = nil
			p.buf = nil
			p.mu.Unlock()
			cancel()
		}()

		for {
			// Respect context cancellation.
			if ctx.Err() != nil {
				return
			}
			wsutilx.SetReadDeadline(conn, wsutilx.DefaultReadTimeout.Load())

			hdr, reader, err := wsutil.NextReader(conn, ws.StateClientSide)
			if err != nil {
				if ctx.Err() == nil {
					p.log.Warn("smartturn ws recv error", "err", err)
				}
				return
			}
			if hdr.OpCode.IsControl() {
				if err := wsutil.ControlFrameHandler(conn, ws.StateClientSide)(hdr, reader); err != nil {
					p.log.Warn("smartturn ws control frame error", "err", err)
					return
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
	}()

	// Watchdog: cancel our read goroutine when the parent context is done.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	return nil
}

// Write sends raw PCM audio to SmartTurn as binary WebSocket messages.
// Audio is buffered and delivered in exact 20ms frames (640 bytes @ 16kHz Linear PCM = 320 samples).
func (p *WSProvider) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.running || p.lw == nil {
		return len(b), nil
	}

	p.buf = append(p.buf, b...)
	const frameBytes = 640 // 320 samples @ 16kHz = 20ms

	for len(p.buf) >= frameBytes {
		frame := p.buf[:frameBytes]
		if err := p.lw.WriteBinary(frame); err != nil {
			if p.log != nil {
				p.log.Warn("smartturn ws write error", "err", err, slog.Int("bytes", len(frame)))
			}
			break
		}
		p.buf = p.buf[frameBytes:]
	}
	return len(b), nil
}

// Stop closes the WebSocket connection and terminates streaming.
func (p *WSProvider) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.running = false
	p.lw = nil
	p.buf = nil
	p.mu.Unlock()
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
	base := opts.ServiceURL
	// Replace http:// with ws:// and https:// with wss://.
	switch {
	case len(base) > 8 && base[:8] == "https://":
		base = "wss://" + base[8:]
	case len(base) > 7 && base[:7] == "http://":
		base = "ws://" + base[7:]
	}

	vad := opts.VAD
	if vad == "" {
		vad = "silero"
	}
	url := base + wsStreamPath + "?vad=" + url.QueryEscape(vad)

	if opts.Threshold > 0 {
		url += "&threshold=" + strconv.FormatFloat(opts.Threshold, 'f', -1, 64)
	}
	if opts.PauseDurationMs > 0 {
		url += "&pause_duration=" + strconv.Itoa(opts.PauseDurationMs) + "ms"
	}
	if opts.Adaptive {
		url += "&adaptive=true"
	}

	return url
}

// Ensure WSProvider satisfies the net.Conn-based check via LockedWriter.
var _ net.Conn

