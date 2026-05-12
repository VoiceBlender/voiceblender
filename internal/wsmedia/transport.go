package wsmedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// Transport carries bidirectional PCM audio, text messages, and structured
// control frames over a single WebSocket connection. Constructed via
// DialClient (outbound) or UpgradeServer (inbound).
type Transport struct {
	cfg     Config
	conn    net.Conn
	side    ws.State
	codec   AudioCodec
	log     *slog.Logger
	peerHdr http.Header

	audioIn  *streamBuffer // recv loop writes here; AudioReader returns this
	audioOut io.Reader     // set in Start; the leg's egress pipe reader

	writeMu sync.Mutex

	onText    atomic.Value // func(string)
	onControl atomic.Value // func(json.RawMessage)

	textDrops  atomic.Int64
	audioDrops atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc

	startOnce sync.Once
	closeOnce sync.Once
	closed    atomic.Bool
	done      chan struct{}
	wg        sync.WaitGroup
	err       atomic.Value // error
}

// AudioReader returns an io.Reader that yields inbound PCM16-LE mono at
// cfg.SampleRate, paced one frame per cfg.FrameMs. Suitable as a leg's
// AudioReader().
func (t *Transport) AudioReader() io.Reader { return t.audioIn }

// PeerHeaders returns the HTTP headers exchanged at the handshake — for
// outbound dials, the server's response headers; for inbound upgrades, the
// client's request headers.
func (t *Transport) PeerHeaders() http.Header { return t.peerHdr }

// Done returns a channel closed once Start's loops have all exited.
func (t *Transport) Done() <-chan struct{} { return t.done }

// Err returns the terminal error, if any. Stable after Done is closed.
func (t *Transport) Err() error {
	if v := t.err.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// AudioDropsBytes reports the cumulative count of inbound audio bytes
// discarded on ingress buffer overflow.
func (t *Transport) AudioDropsBytes() int64 { return t.audioIn.Dropped() }

// TextDropsCount reports the cumulative count of inbound text messages
// discarded because the text-channel callback wasn't draining fast enough.
func (t *Transport) TextDropsCount() int64 { return t.textDrops.Load() }

// SetOnText registers the callback invoked for inbound text messages.
// Setting nil disables delivery (messages still count toward TextDrops).
func (t *Transport) SetOnText(fn func(text string)) {
	if fn == nil {
		t.onText.Store(func(string) {})
		return
	}
	t.onText.Store(fn)
}

// SetOnControl registers the callback invoked for inbound JSON text frames
// whose "type" isn't recognized by Transport itself. Lets agent providers
// plug in vendor-specific decoders.
func (t *Transport) SetOnControl(fn func(raw json.RawMessage)) {
	if fn == nil {
		t.onControl.Store(func(json.RawMessage) {})
		return
	}
	t.onControl.Store(fn)
}

// Start begins the send/recv/ping goroutines. audioOut supplies PCM16-LE
// mono at cfg.SampleRate that will be encoded and shipped to the peer.
// Calling Start more than once is a no-op after the first call.
func (t *Transport) Start(audioOut io.Reader) {
	t.startOnce.Do(func() {
		t.audioOut = audioOut
		t.wg.Add(3)
		go t.recvLoop()
		go t.sendLoop()
		go t.pingLoop()
		go func() {
			t.wg.Wait()
			close(t.done)
		}()
	})
}

// Close shuts the transport down: cancels the context, closes the inbound
// audio buffer, closes the underlying conn, and — if audioOut was set via
// Start and implements io.Closer — closes that too so the send loop
// unblocks from any pending read. Safe to call repeatedly and from any
// goroutine.
func (t *Transport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		t.cancel()
		t.audioIn.Close()
		_ = t.conn.Close()
		if c, ok := t.audioOut.(io.Closer); ok {
			_ = c.Close()
		}
	})
	return nil
}

// WriteText sends a text message to the peer as a JSON control frame.
// Returns ErrTextDisabled if the transport was constructed with text off.
func (t *Transport) WriteText(_ context.Context, text string) error {
	if !t.cfg.TextEnabledValue() {
		return ErrTextDisabled
	}
	frame, err := json.Marshal(controlFrame{Type: "text", Text: text})
	if err != nil {
		return err
	}
	return t.writeText(frame)
}

// SendStructured marshals v to JSON and ships it as a WS text frame.
// Intended for callers (future agent providers) that need to send
// vendor-specific control messages outside the built-in envelope.
func (t *Transport) SendStructured(v any) error {
	frame, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return t.writeText(frame)
}

// ErrTextDisabled is returned by WriteText when the transport was
// configured with TextEnabled=false.
var ErrTextDisabled = errors.New("wsmedia: text channel disabled")

// writeText is the locked + write-deadline-bounded server/client text write.
func (t *Transport) writeText(payload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	setWriteDeadline(t.conn, t.cfg.WriteTimeout)
	if t.side == ws.StateServerSide {
		return wsutil.WriteServerText(t.conn, payload)
	}
	return wsutil.WriteClientText(t.conn, payload)
}

// writeBinary is the locked + write-deadline-bounded binary write.
func (t *Transport) writeBinary(payload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	setWriteDeadline(t.conn, t.cfg.WriteTimeout)
	if t.side == ws.StateServerSide {
		return wsutil.WriteServerBinary(t.conn, payload)
	}
	return wsutil.WriteClientBinary(t.conn, payload)
}

// writeMessage routes to writeText or writeBinary based on op.
func (t *Transport) writeMessage(op ws.OpCode, payload []byte) error {
	switch op {
	case ws.OpText:
		return t.writeText(payload)
	case ws.OpBinary:
		return t.writeBinary(payload)
	default:
		return fmt.Errorf("wsmedia: unsupported write opcode %x", op)
	}
}

// setWriteDeadline pushes the conn write deadline forward by timeout. A
// non-positive timeout is a no-op (caller manages deadlines).
func setWriteDeadline(conn net.Conn, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
}

// recvLoop reads WS frames until error or close.
func (t *Transport) recvLoop() {
	defer t.wg.Done()
	defer t.Close()

	stopWatch := wsutilx.WatchCancel(t.ctx, t.conn)
	defer stopWatch()

	controlHandler := wsutil.ControlFrameHandler(t.conn, t.side)
	rd := &wsutil.Reader{
		Source: t.conn,
		State:  t.side,
		OnIntermediate: func(hdr ws.Header, r io.Reader) error {
			return controlHandler(hdr, r)
		},
	}

	for {
		if t.closed.Load() {
			return
		}
		wsutilx.SetReadDeadline(t.conn, t.cfg.ReadTimeout)
		hdr, err := rd.NextFrame()
		if err != nil {
			t.setErr(err)
			return
		}
		if hdr.OpCode.IsControl() {
			if err := controlHandler(hdr, rd); err != nil {
				t.setErr(err)
				return
			}
			continue
		}

		payload, err := io.ReadAll(rd)
		if err != nil {
			t.setErr(err)
			return
		}

		switch hdr.OpCode {
		case ws.OpBinary:
			t.handleBinaryFrame(payload)
		case ws.OpText:
			t.handleTextFrame(payload)
		}
	}
}

func (t *Transport) handleBinaryFrame(payload []byte) {
	if t.cfg.WireFormat != WireBinary {
		// Unexpected binary in a JSON-mode session — drop and log once.
		t.log.Debug("wsmedia: dropping unexpected binary frame in JSON mode", "bytes", len(payload))
		return
	}
	pcm, err := t.codec.Decode(ws.OpBinary, payload)
	if err != nil {
		t.log.Warn("wsmedia: decode binary frame", "error", err)
		return
	}
	if len(pcm) == 0 {
		return
	}
	t.writePCM(pcm)
}

func (t *Transport) handleTextFrame(payload []byte) {
	cf, err := parseControlFrame(payload)
	if err != nil {
		t.log.Warn("wsmedia: parse text frame", "error", err)
		return
	}
	switch cf.Type {
	case "audio":
		if t.cfg.WireFormat != WireJSONBase64 {
			t.log.Debug("wsmedia: dropping audio JSON frame in binary mode")
			return
		}
		pcm, err := decodeBase64Audio(cf.Audio)
		if err != nil {
			t.log.Warn("wsmedia: decode base64 audio", "error", err)
			return
		}
		t.writePCM(pcm)
	case "text":
		if !t.cfg.TextEnabledValue() {
			return
		}
		fn, _ := t.onText.Load().(func(string))
		if fn == nil {
			t.textDrops.Add(1)
			return
		}
		fn(cf.Text)
	case "ping":
		reply, _ := json.Marshal(controlFrame{Type: "pong", EventID: cf.EventID})
		if err := t.writeText(reply); err != nil {
			t.log.Debug("wsmedia: pong write failed", "error", err)
		}
	case "pong":
		// Heartbeat acknowledgement; nothing to do.
	case "hangup":
		t.Close()
	default:
		fn, _ := t.onControl.Load().(func(json.RawMessage))
		if fn != nil {
			fn(cf.Raw)
		}
	}
}

func (t *Transport) writePCM(pcm []int16) {
	if len(pcm) == 0 {
		return
	}
	raw := pcmToBytes(pcm)
	if _, err := t.audioIn.Write(raw); err != nil {
		t.log.Warn("wsmedia: audio ingress write", "error", err)
	}
}

func (t *Transport) sendLoop() {
	defer t.wg.Done()
	defer t.Close()

	frameSamples := t.cfg.FrameSamples()
	frameBytes := frameSamples * 2
	buf := make([]byte, frameBytes)

	for {
		if t.closed.Load() {
			return
		}
		_, err := io.ReadFull(t.audioOut, buf)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			t.setErr(err)
			return
		}
		pcm := bytesToPCM(buf)
		payload, op, err := t.codec.Encode(pcm)
		if err != nil {
			t.log.Warn("wsmedia: codec encode", "error", err)
			continue
		}
		if err := t.writeMessage(op, payload); err != nil {
			t.setErr(err)
			return
		}
	}
}

func (t *Transport) pingLoop() {
	defer t.wg.Done()

	if t.cfg.PingInterval <= 0 {
		return
	}
	ticker := time.NewTicker(t.cfg.PingInterval)
	defer ticker.Stop()

	var eventID int64
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			if t.closed.Load() {
				return
			}
			eventID++
			payload, _ := json.Marshal(controlFrame{Type: "ping", EventID: eventID})
			if err := t.writeText(payload); err != nil {
				// Ping failure is sufficient evidence the peer is gone;
				// the recv-side timeout will also fire shortly. Surface
				// the error so Err() carries something descriptive.
				t.setErr(err)
				return
			}
		}
	}
}

func (t *Transport) setErr(err error) {
	if err == nil {
		return
	}
	// First error wins.
	if t.err.Load() == nil {
		t.err.Store(err)
	}
}
