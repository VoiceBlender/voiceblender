package turndetector

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// --- HTTPProvider tests ---

func TestHTTPProvider_TurnComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != httpEvaluatePath {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(httpEvaluateResponse{
			IsTurnComplete: true,
			Probability:    0.87,
			ThresholdUsed:  0.55,
			EvaluationMs:   12,
		})
	}))
	defer srv.Close()

	p := NewHTTP(slog.Default())
	p.SetOptions(Options{ServiceURL: srv.URL, Threshold: 0.55})

	var received atomic.Int32
	var lastEvent Event

	ctx := context.Background()
	if err := p.Start(ctx, func(ev Event) {
		lastEvent = ev
		received.Add(1)
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Write some dummy PCM frames.
	frame := make([]byte, 640)
	for i := 0; i < 10; i++ {
		p.Write(frame)
	}

	p.NotifyPause(ctx)

	// Wait for the async HTTP call.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && received.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if received.Load() == 0 {
		t.Fatal("expected onEvent to be called")
	}
	if !lastEvent.Complete {
		t.Errorf("expected Complete=true, got false")
	}
	if lastEvent.Probability != 0.87 {
		t.Errorf("probability = %v, want 0.87", lastEvent.Probability)
	}
	p.Stop()
}

func TestHTTPProvider_TurnIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(httpEvaluateResponse{
			IsTurnComplete: false,
			Probability:    0.21,
			ThresholdUsed:  0.55,
			EvaluationMs:   8,
		})
	}))
	defer srv.Close()

	p := NewHTTP(slog.Default())
	p.SetOptions(Options{ServiceURL: srv.URL, Threshold: 0.55})

	var received atomic.Int32
	ctx := context.Background()
	p.Start(ctx, func(ev Event) {
		if ev.Complete {
			t.Errorf("expected Complete=false, got true")
		}
		received.Add(1)
	})

	p.Write(make([]byte, 640))
	p.NotifyPause(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && received.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if received.Load() == 0 {
		t.Fatal("expected onEvent for incomplete turn")
	}
	p.Stop()
}

func TestHTTPProvider_EmptyBufferNoRequest(t *testing.T) {
	var requested atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(true)
	}))
	defer srv.Close()

	p := NewHTTP(slog.Default())
	p.SetOptions(Options{ServiceURL: srv.URL})
	p.Start(context.Background(), func(Event) {})

	// Notify pause without writing any audio.
	p.NotifyPause(context.Background())
	time.Sleep(50 * time.Millisecond)

	if requested.Load() {
		t.Error("HTTP request sent for empty buffer; want no request")
	}
	p.Stop()
}

func TestHTTPProvider_RunningState(t *testing.T) {
	p := NewHTTP(slog.Default())
	p.SetOptions(Options{ServiceURL: "http://127.0.0.1:1"}) // won't connect, just tests state

	if p.Running() {
		t.Error("Running() should be false before Start")
	}

	p.Start(context.Background(), func(Event) {})
	if !p.Running() {
		t.Error("Running() should be true after Start")
	}

	p.Stop()
	if p.Running() {
		t.Error("Running() should be false after Stop")
	}
}

func TestHTTPProvider_WriteBufferRollover(t *testing.T) {
	p := NewHTTP(slog.Default())
	p.SetOptions(Options{ServiceURL: "http://127.0.0.1:1", Threshold: 0.55})
	p.Start(context.Background(), func(Event) {})

	// 10s of audio at 16kHz int16 = 320,000 bytes — twice the buffer.
	big := make([]byte, 320000)
	if _, err := p.Write(big); err != nil {
		t.Errorf("Write returned error: %v", err)
	}
	p.Stop()
}

func TestHTTPProvider_MissingServiceURL(t *testing.T) {
	p := NewHTTP(slog.Default())
	// No SetOptions call — ServiceURL is empty.
	err := p.Start(context.Background(), func(Event) {})
	if err == nil {
		t.Error("Start with empty ServiceURL should return error")
	}
}

// --- WSProvider tests ---

func TestWSProvider_TurnDecisions(t *testing.T) {
	var framesReceived atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wsStreamPath {
			http.NotFound(w, r)
			return
		}
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()

		// Send complete decision
		msg, _ := json.Marshal(map[string]interface{}{
			"event":          "turn_decision",
			"action":         "TURN_COMPLETE",
			"probability":    0.91,
			"threshold_used": 0.60,
			"evaluation_ms":  15,
		})
		_ = wsutil.WriteServerText(conn, msg)

		// Read frames
		for {
			data, _, err := wsutil.ReadClientData(conn)
			if err != nil {
				return
			}
			if len(data) == 640 {
				framesReceived.Add(1)
			}
		}
	}))
	defer srv.Close()

	p := NewWS(slog.Default())
	p.SetOptions(Options{ServiceURL: srv.URL, Threshold: 0.60, VAD: "silero"})

	var received atomic.Int32
	var lastEvent Event

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx, func(ev Event) {
		lastEvent = ev
		received.Add(1)
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !p.Running() {
		t.Error("expected Running() = true")
	}

	// Write three 20ms frames
	for i := 0; i < 3; i++ {
		p.Write(make([]byte, 640))
	}

	// Wait for the decision event
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && received.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if received.Load() == 0 {
		t.Fatal("expected turn decision event from WebSocket")
	}
	if !lastEvent.Complete {
		t.Errorf("expected Complete=true, got %v", lastEvent.Complete)
	}
	if lastEvent.Probability != 0.91 {
		t.Errorf("expected Probability=0.91, got %v", lastEvent.Probability)
	}

	p.Stop()
	if p.Running() {
		t.Error("expected Running() = false after Stop")
	}
}

func TestWSProvider_20msFramingEdgeCases(t *testing.T) {
	var mu sync.Mutex
	var frameSizes []int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			data, op, err := wsutil.ReadClientData(conn)
			if err != nil {
				return
			}
			if op == ws.OpBinary {
				mu.Lock()
				frameSizes = append(frameSizes, len(data))
				mu.Unlock()
			}
		}
	}))
	defer srv.Close()

	p := NewWS(slog.Default())
	p.SetOptions(Options{ServiceURL: srv.URL})

	ctx := context.Background()
	if err := p.Start(ctx, func(Event) {}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Edge Case 1: Empty writes should do nothing
	p.Write(nil)
	p.Write([]byte{})

	// Edge Case 2: Sub-frame fragments (100 bytes each)
	// 7 writes of 100 bytes = 700 bytes -> should flush 1 exact 640-byte frame, 60 bytes remain buffered
	for i := 0; i < 7; i++ {
		p.Write(make([]byte, 100))
	}

	// Edge Case 3: Oversized multi-frame write (e.g. 1920 bytes from 48kHz Opus resampler)
	// 60 remaining + 1920 bytes = 1980 bytes -> should flush 3 frames of 640 (1920), 60 bytes remain
	p.Write(make([]byte, 1920))

	// Edge Case 4: Complete the remaining bytes
	// 60 remaining + 580 bytes = 640 bytes -> should flush 1 frame of 640
	p.Write(make([]byte, 580))

	time.Sleep(100 * time.Millisecond)
	p.Stop()

	mu.Lock()
	defer mu.Unlock()

	// Total bytes written = 700 + 1920 + 580 = 3200 bytes = exactly 5 frames of 640 bytes
	if len(frameSizes) != 5 {
		t.Fatalf("expected 5 frames sent, got %d (sizes: %v)", len(frameSizes), frameSizes)
	}
	for i, size := range frameSizes {
		if size != 640 {
			t.Errorf("frame %d size = %d, want exact 640 bytes", i, size)
		}
	}
}

func TestWSProvider_MissingServiceURL(t *testing.T) {
	p := NewWS(slog.Default())
	err := p.Start(context.Background(), func(Event) {})
	if err == nil {
		t.Error("Start with empty ServiceURL should return error")
	}
}

// --- buildWSURL tests ---

func TestBuildWSURL(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "default_fallback_to_silero",
			opts: Options{ServiceURL: "http://smartturn:8080", Threshold: 0.55, PauseDurationMs: 400},
			want: "ws://smartturn:8080/v1/ws/stream?vad=silero&threshold=0.55&pause_duration=400ms",
		},
		{
			name: "explicit_rms_vad",
			opts: Options{ServiceURL: "http://smartturn:8080", VAD: "rms", Threshold: 0.60},
			want: "ws://smartturn:8080/v1/ws/stream?vad=rms&threshold=0.6",
		},
		{
			name: "https_base_converted",
			opts: Options{ServiceURL: "https://smartturn.internal", Threshold: 0.70},
			want: "wss://smartturn.internal/v1/ws/stream?vad=silero&threshold=0.7",
		},
		{
			name: "adaptive_enabled",
			opts: Options{ServiceURL: "http://localhost:8080", Adaptive: true},
			want: "ws://localhost:8080/v1/ws/stream?vad=silero&adaptive=true",
		},
		{
			name: "already_ws_scheme",
			opts: Options{ServiceURL: "ws://localhost:8080", Threshold: 0.60},
			want: "ws://localhost:8080/v1/ws/stream?vad=silero&threshold=0.6",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildWSURL(tc.opts)
			if got != tc.want {
				t.Errorf("buildWSURL =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}
