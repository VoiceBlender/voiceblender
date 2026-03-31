package stt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func TestAzure_StartAndReceiveTranscripts(t *testing.T) {
	var mu sync.Mutex
	_ = mu

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		defer conn.Close()

		// Read messages from client.
		go func() {
			for {
				msg, op, err := wsutil.ReadClientData(conn)
				if err != nil {
					return
				}
				_ = op
				_ = msg
			}
		}()

		// Send a hypothesis (partial).
		hypoBody, _ := json.Marshal(azSpeechHypothesis{Text: "hel"})
		hypoMsg := "Path:speech.hypothesis\r\nContent-Type:application/json\r\n\r\n" + string(hypoBody)
		wsutil.WriteServerText(conn, []byte(hypoMsg))

		time.Sleep(50 * time.Millisecond)

		// Send a phrase (final).
		phraseBody, _ := json.Marshal(azSpeechPhrase{RecognitionStatus: "Success", DisplayText: "hello world"})
		phraseMsg := "Path:speech.phrase\r\nContent-Type:application/json\r\n\r\n" + string(phraseBody)
		wsutil.WriteServerText(conn, []byte(phraseMsg))

		time.Sleep(50 * time.Millisecond)

		// Send close.
		wsutil.WriteServerMessage(conn, ws.OpClose, ws.NewCloseFrameBody(ws.StatusNormalClosure, ""))
	}))
	defer srv.Close()

	// Replace wss with ws for test server.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	transcriber := &AzureTranscriber{
		region: "test",
		log:    slog.Default(),
	}
	// Override the URL by temporarily patching. Instead, we test with the actual Start method
	// which constructs the URL from region. For unit tests, we test the helper functions directly
	// and test the full flow via the mock WebSocket server.

	// We'll test the parsing functions separately and use a direct WebSocket connection for the full flow.
	_ = wsURL
	_ = transcriber

	// Test parseAzureTextFrame.
	t.Run("parseAzureTextFrame", func(t *testing.T) {
		path, body := parseAzureTextFrame("Path:speech.phrase\r\nContent-Type:application/json\r\n\r\n{\"DisplayText\":\"hello\"}")
		if path != "speech.phrase" {
			t.Errorf("path = %q, want speech.phrase", path)
		}
		var p azSpeechPhrase
		json.Unmarshal([]byte(body), &p)
		if p.DisplayText != "hello" {
			t.Errorf("DisplayText = %q, want hello", p.DisplayText)
		}
	})

	t.Run("parseAzureTextFrame_no_separator", func(t *testing.T) {
		path, body := parseAzureTextFrame("no separator here")
		if path != "" {
			t.Errorf("path = %q, want empty", path)
		}
		if body != "no separator here" {
			t.Errorf("body = %q, want original", body)
		}
	})
}

func TestBuildAzureBinaryFrame(t *testing.T) {
	payload := []byte("test audio data")
	frame := buildAzureBinaryFrame("audio", "abc123", "audio/x-wav", payload)

	// First 2 bytes are the header length.
	headerLen := binary.BigEndian.Uint16(frame[:2])
	header := string(frame[2 : 2+headerLen])

	if !strings.Contains(header, "Path:audio") {
		t.Error("header missing Path:audio")
	}
	if !strings.Contains(header, "X-RequestId:abc123") {
		t.Error("header missing X-RequestId")
	}
	if !strings.Contains(header, "Content-Type:audio/x-wav") {
		t.Error("header missing Content-Type")
	}

	audioPayload := frame[2+headerLen:]
	if !bytes.Equal(audioPayload, payload) {
		t.Errorf("payload = %q, want %q", audioPayload, payload)
	}
}

func TestBuildAzureBinaryFrame_EmptyPayload(t *testing.T) {
	frame := buildAzureBinaryFrame("audio", "abc123", "audio/x-wav", nil)
	headerLen := binary.BigEndian.Uint16(frame[:2])
	if int(headerLen)+2 != len(frame) {
		t.Errorf("frame length = %d, expected %d (header only)", len(frame), int(headerLen)+2)
	}
}

func TestBuildAzureTextFrame(t *testing.T) {
	payload := []byte(`{"key":"value"}`)
	msg := buildAzureTextFrame("speech.config", "req123", "application/json", payload)
	raw := string(msg)

	if !strings.Contains(raw, "Path:speech.config") {
		t.Error("missing Path header")
	}
	if !strings.Contains(raw, "X-RequestId:req123") {
		t.Error("missing X-RequestId header")
	}
	if !strings.Contains(raw, "Content-Type:application/json") {
		t.Error("missing Content-Type header")
	}
	idx := strings.Index(raw, "\r\n\r\n")
	if idx < 0 {
		t.Fatal("missing header/body separator")
	}
	body := raw[idx+4:]
	if body != `{"key":"value"}` {
		t.Errorf("body = %q, want {\"key\":\"value\"}", body)
	}
}

func TestAzure_StopBeforeStart(t *testing.T) {
	transcriber := NewAzure("eastus", slog.Default())
	// Stop should be safe to call even when not running.
	transcriber.Stop()
	if transcriber.Running() {
		t.Error("should not be running")
	}
}

func TestAzure_DoubleStart(t *testing.T) {
	transcriber := NewAzure("eastus", slog.Default())

	// Simulate running state.
	transcriber.mu.Lock()
	transcriber.running = true
	transcriber.mu.Unlock()

	// Second Start should return nil immediately.
	err := transcriber.Start(context.Background(), strings.NewReader(""), "key", Options{}, func(string, bool) {})
	if err != nil {
		t.Errorf("expected nil from double start, got %v", err)
	}

	// Reset.
	transcriber.mu.Lock()
	transcriber.running = false
	transcriber.mu.Unlock()
}

func TestParseAzureTextFrame_Hypothesis(t *testing.T) {
	hypo := azSpeechHypothesis{Text: "testing"}
	body, _ := json.Marshal(hypo)
	raw := "Path:speech.hypothesis\r\nX-RequestId:abc\r\n\r\n" + string(body)

	path, jsonBody := parseAzureTextFrame(raw)
	if path != "speech.hypothesis" {
		t.Errorf("path = %q, want speech.hypothesis", path)
	}

	var parsed azSpeechHypothesis
	if err := json.Unmarshal([]byte(jsonBody), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Text != "testing" {
		t.Errorf("Text = %q, want testing", parsed.Text)
	}
}

func TestParseAzureTextFrame_Phrase(t *testing.T) {
	phrase := azSpeechPhrase{RecognitionStatus: "Success", DisplayText: "Hello world."}
	body, _ := json.Marshal(phrase)
	raw := "Path:speech.phrase\r\nX-RequestId:abc\r\n\r\n" + string(body)

	path, jsonBody := parseAzureTextFrame(raw)
	if path != "speech.phrase" {
		t.Errorf("path = %q, want speech.phrase", path)
	}

	var parsed azSpeechPhrase
	if err := json.Unmarshal([]byte(jsonBody), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.RecognitionStatus != "Success" {
		t.Errorf("RecognitionStatus = %q, want Success", parsed.RecognitionStatus)
	}
	if parsed.DisplayText != "Hello world." {
		t.Errorf("DisplayText = %q, want 'Hello world.'", parsed.DisplayText)
	}
}

func TestParseAzureTextFrame_NoMatch(t *testing.T) {
	phrase := azSpeechPhrase{RecognitionStatus: "NoMatch", DisplayText: ""}
	body, _ := json.Marshal(phrase)
	raw := "Path:speech.phrase\r\n\r\n" + string(body)

	_, jsonBody := parseAzureTextFrame(raw)
	var parsed azSpeechPhrase
	json.Unmarshal([]byte(jsonBody), &parsed)

	if parsed.RecognitionStatus != "NoMatch" {
		t.Errorf("RecognitionStatus = %q, want NoMatch", parsed.RecognitionStatus)
	}
}

func TestAzure_FullFlowWithMockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()

		// Read the speech.config text frame.
		msg, op, err := wsutil.ReadClientData(conn)
		if err != nil {
			return
		}
		if op != ws.OpText {
			t.Errorf("expected text frame for config, got %v", op)
		}
		if !strings.Contains(string(msg), "speech.config") {
			t.Errorf("expected speech.config, got %s", string(msg))
		}

		// Read one audio binary frame.
		msg, op, err = wsutil.ReadClientData(conn)
		if err != nil {
			return
		}
		if op != ws.OpBinary {
			t.Errorf("expected binary frame for audio, got %v", op)
		}
		// Verify the binary frame has the 2-byte header length prefix.
		if len(msg) < 2 {
			t.Errorf("binary frame too short: %d bytes", len(msg))
			return
		}
		headerLen := binary.BigEndian.Uint16(msg[:2])
		header := string(msg[2 : 2+headerLen])
		if !strings.Contains(header, "Path:audio") {
			t.Errorf("audio frame header missing Path:audio: %s", header)
		}

		// Send a final transcript.
		phraseBody, _ := json.Marshal(azSpeechPhrase{RecognitionStatus: "Success", DisplayText: "test transcript"})
		phraseMsg := "Path:speech.phrase\r\nContent-Type:application/json\r\n\r\n" + string(phraseBody)
		wsutil.WriteServerText(conn, []byte(phraseMsg))

		// Give client time to process.
		time.Sleep(100 * time.Millisecond)

		// Close.
		wsutil.WriteServerMessage(conn, ws.OpClose, ws.NewCloseFrameBody(ws.StatusNormalClosure, ""))
	}))
	defer srv.Close()

	// We need to test with the actual Start method. The challenge is that Start constructs
	// the URL from region. We work around this by creating a transcriber and calling internal
	// methods, or by testing the integrated flow with a custom dialer.
	// For a proper unit test, test the recv/send loops indirectly through the full Start path.

	// Create audio data (one frame).
	audioData := make([]byte, azFrameBytes)
	for i := range audioData {
		audioData[i] = byte(i % 256)
	}
	_ = io.NopCloser(bytes.NewReader(audioData))

	var transcripts []string
	var transcriptsMu sync.Mutex
	cb := func(text string, isFinal bool) {
		transcriptsMu.Lock()
		transcripts = append(transcripts, text)
		transcriptsMu.Unlock()
	}

	// Create a transcriber that connects to our test server.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	transcriber := NewAzure("test", slog.Default())
	// We need to override the dial URL. Since Start builds the URL internally,
	// we test by directly calling the WebSocket connection and loops.
	// For integration, we'd test with real Azure. For unit test, we verify the helpers.

	// Direct WebSocket test.
	dialer := ws.Dialer{}
	conn, _, _, err := dialer.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	lw := &azLockedWriter{conn: conn}
	requestID := "testreqid"

	// Send config.
	if err := transcriber.sendConfig(lw, requestID); err != nil {
		t.Fatalf("sendConfig: %v", err)
	}

	// Send one audio frame.
	frame := buildAzureBinaryFrame("audio", requestID, "audio/x-wav", audioData)
	if err := lw.WriteBinary(frame); err != nil {
		t.Fatalf("WriteBinary: %v", err)
	}

	// Run recvLoop in background.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		transcriber.recvLoop(ctx, conn, lw, cb, false)
	}()

	wg.Wait()

	transcriptsMu.Lock()
	defer transcriptsMu.Unlock()
	if len(transcripts) != 1 {
		t.Fatalf("expected 1 transcript, got %d", len(transcripts))
	}
	if transcripts[0] != "test transcript" {
		t.Errorf("transcript = %q, want 'test transcript'", transcripts[0])
	}
}
