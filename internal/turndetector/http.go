package turndetector

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	// httpBufferSamples is 8 seconds of audio at 16kHz, matching smartturn-go's AudioRingBuffer capacity.
	httpBufferSamples = 128000
	// httpEvaluatePath is the SmartTurn HTTP endpoint path.
	httpEvaluatePath = "/v1/evaluate"
	// httpClientTimeout bounds each HTTP evaluation request.
	httpClientTimeout = 3 * time.Second
)

// HTTPProvider sends accumulated leg audio to the SmartTurn HTTP evaluate endpoint
// after VoiceBlender's existing speaking.Detector signals a speech pause.
//
// Concurrency: Write and NotifyPause are called from separate goroutines (the leg's
// audio tap and the speaking detector callback respectively). Both are safe to call
// concurrently. Start/Stop are external lifecycle calls and must not overlap.
type HTTPProvider struct {
	log    *slog.Logger
	client *http.Client
	opts   Options

	mu      sync.Mutex
	buf     []byte  // raw PCM ring approximation (oldest bytes dropped when full)
	running bool
	cancel  context.CancelFunc
	onEvent func(Event)
}

// httpEvaluateResponse is the JSON shape returned by POST /v1/evaluate.
type httpEvaluateResponse struct {
	IsTurnComplete bool    `json:"is_turn_complete"`
	Probability    float64 `json:"probability"`
	ThresholdUsed  float64 `json:"threshold_used"`
	EvaluationMs   int64   `json:"evaluation_ms"`
}

// NewHTTP creates an HTTPProvider. Call Start before writing audio.
func NewHTTP(log *slog.Logger) *HTTPProvider {
	return &HTTPProvider{
		log:    log,
		client: &http.Client{Timeout: httpClientTimeout},
	}
}

// Start initialises the provider with the given options and callback.
func (p *HTTPProvider) Start(ctx context.Context, onEvent func(Event)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return nil
	}
	if p.opts.ServiceURL == "" {
		return fmt.Errorf("turndetector: SMART_TURN_URL is not set")
	}
	_, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.onEvent = onEvent
	p.buf = make([]byte, 0, httpBufferSamples*2) // int16 = 2 bytes per sample
	p.running = true
	return nil
}

// Stop halts the provider and releases resources.
func (p *HTTPProvider) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return
	}
	p.running = false
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.buf = nil
}

// Running reports whether the provider is active.
func (p *HTTPProvider) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Write implements io.Writer. It appends raw PCM bytes to the internal buffer,
// dropping the oldest audio when the 8-second capacity is exceeded.
func (p *HTTPProvider) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return len(b), nil
	}
	cap := httpBufferSamples * 2
	available := cap - len(p.buf)
	if len(b) >= cap {
		// Incoming chunk larger than the whole buffer: keep only the tail.
		p.buf = p.buf[:cap]
		copy(p.buf, b[len(b)-cap:])
	} else if len(b) > available {
		// Drop oldest bytes to make room.
		drop := len(b) - available
		copy(p.buf, p.buf[drop:])
		p.buf = p.buf[:len(p.buf)-drop]
		p.buf = append(p.buf, b...)
	} else {
		p.buf = append(p.buf, b...)
	}
	return len(b), nil
}

// NotifyPause is called by the API layer when the speaking.Detector fires a
// speaking.stopped event. It drains the PCM buffer and POSTs it to SmartTurn.
// Runs the HTTP call in its own goroutine so the speaking callback is not blocked.
func (p *HTTPProvider) NotifyPause(ctx context.Context) {
	p.mu.Lock()
	if !p.running || len(p.buf) == 0 {
		p.mu.Unlock()
		return
	}
	// Grab a snapshot of the buffer and reset it.
	payload := make([]byte, len(p.buf))
	copy(payload, p.buf)
	p.buf = p.buf[:0]
	threshold := p.opts.Threshold
	serviceURL := p.opts.ServiceURL
	onEvent := p.onEvent
	p.mu.Unlock()

	go func() {
		ev, err := p.evaluate(ctx, serviceURL, threshold, payload)
		if err != nil {
			p.log.Warn("SmartTurn HTTP evaluation failed", "err", err)
			return
		}
		if onEvent != nil {
			onEvent(*ev)
		}
	}()
}

// SetOptions replaces the provider options. Must be called before Start.
func (p *HTTPProvider) SetOptions(opts Options) {
	p.mu.Lock()
	p.opts = opts
	p.mu.Unlock()
}

func (p *HTTPProvider) evaluate(ctx context.Context, serviceURL string, threshold float64, payload []byte) (*Event, error) {
	u, err := url.Parse(serviceURL + httpEvaluatePath)
	if err != nil {
		return nil, fmt.Errorf("invalid SmartTurn URL: %w", err)
	}
	q := u.Query()
	q.Set("threshold", strconv.FormatFloat(threshold, 'f', -1, 64))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to build SmartTurn request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SmartTurn HTTP request failed: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SmartTurn returned status %d", resp.StatusCode)
	}

	var r httpEvaluateResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("failed to decode SmartTurn response: %w", err)
	}

	thresh := r.ThresholdUsed
	if thresh == 0 {
		thresh = threshold
	}
	return &Event{
		Complete:      r.IsTurnComplete,
		Probability:   r.Probability,
		ThresholdUsed: thresh,
		ProcessingMs:  r.EvaluationMs,
	}, nil
}

// binaryLittleEndianInt16 converts raw PCM bytes to []int16 samples.
// Kept as a package-level helper for test use.
func binaryLittleEndianInt16(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}
