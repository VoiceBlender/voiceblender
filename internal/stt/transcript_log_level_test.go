package stt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// piiCanary is a string that appears nowhere else in the tree, so any log
// record containing it can only have got it from the transcript under test.
const piiCanary = "zz-pii-canary-6f2a-123-45-6789"

// capturedRecord is one slog record, kept structurally. Asserting on a flat
// buffer would be a masking defect here: the raw-frame preview at
// internal/stt/azure.go:236 dumps the whole provider frame at debug, canary
// included, so a buffer-substring test stays green even if the transcript log
// line is deleted outright.
type capturedRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

func (r capturedRecord) hasCanary() bool {
	if strings.Contains(r.Message, piiCanary) {
		return true
	}
	for _, v := range r.Attrs {
		if strings.Contains(v, piiCanary) {
			return true
		}
	}
	return false
}

type capturingHandler struct {
	mu      sync.Mutex
	records *[]capturedRecord
}

func newCapturingLogger() (*slog.Logger, func() []capturedRecord) {
	recs := &[]capturedRecord{}
	h := &capturingHandler{records: recs}
	return slog.New(h), func() []capturedRecord {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := make([]capturedRecord, len(*recs))
		copy(out, *recs)
		return out
	}
}

// Enabled always returns true so records at every level are captured; the test
// asserts on Level itself rather than relying on a handler threshold.
func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{Level: r.Level, Message: r.Message, Attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = fmt.Sprint(a.Value.Any())
		return true
	})
	h.mu.Lock()
	*h.records = append(*h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// runAzureRecvLoop stands up a mock Azure Speech websocket that sends one text
// frame, drives recvLoop against it, and returns everything that was logged.
// Modelled on TestAzure_FullFlowWithMockServer.
func runAzureRecvLoop(t *testing.T, frame string, partial bool) []capturedRecord {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the client's speech.config frame before sending anything.
		// Dialing returns a bufio.Reader that would swallow any frame the
		// server pushed alongside the handshake response, so the server must
		// not speak first. Mirrors TestAzure_FullFlowWithMockServer.
		if _, _, err := wsutil.ReadClientData(conn); err != nil {
			return
		}
		_ = wsutil.WriteServerText(conn, []byte(frame))
		_ = wsutil.WriteServerMessage(conn, ws.OpClose, ws.NewCloseFrameBody(ws.StatusNormalClosure, ""))
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log, captured := newCapturingLogger()
	transcriber := NewAzure("test", log)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, _, err := ws.Dialer{}.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var got []string
	var gotMu sync.Mutex
	cb := func(text string, isFinal bool) {
		gotMu.Lock()
		got = append(got, text)
		gotMu.Unlock()
	}

	lw := wsutilx.NewLockedWriter(conn)
	if err := transcriber.sendConfig(lw, "testreqid"); err != nil {
		t.Fatalf("sendConfig: %v", err)
	}
	transcriber.recvLoop(ctx, conn, lw, cb, partial)

	gotMu.Lock()
	defer gotMu.Unlock()
	if len(got) != 1 || got[0] != piiCanary {
		t.Fatalf("callback did not receive the transcript (got %v) — the branch under test never ran, so the log assertions would be vacuous", got)
	}
	return captured()
}

// assertCanaryOnlyAtDebug proves the transcript was demoted, not deleted:
// nothing at info or above carries it, and the named debug line does.
func assertCanaryOnlyAtDebug(t *testing.T, records []capturedRecord, debugMsg string) {
	t.Helper()

	for _, r := range records {
		if r.Level >= slog.LevelInfo && r.hasCanary() {
			t.Errorf("transcript text leaked at %s: message %q attrs %v", r.Level, r.Message, r.Attrs)
		}
	}

	for _, r := range records {
		// Keyed on Message so the unrelated debug raw-frame preview, which
		// also contains the canary, cannot satisfy this.
		if r.Level == slog.LevelDebug && r.Message == debugMsg && r.Attrs["text"] == piiCanary {
			return
		}
	}
	t.Errorf("no debug record %q carrying the transcript in its \"text\" attr; the line was deleted rather than demoted. Captured: %v", debugMsg, records)
}

func TestAzureSTT_FinalTranscriptTextOnlyAtDebug(t *testing.T) {
	body, err := json.Marshal(azSpeechPhrase{RecognitionStatus: "Success", DisplayText: piiCanary})
	if err != nil {
		t.Fatalf("marshal phrase: %v", err)
	}
	frame := "Path:speech.phrase\r\nContent-Type:application/json\r\n\r\n" + string(body)

	records := runAzureRecvLoop(t, frame, false)
	assertCanaryOnlyAtDebug(t, records, "azure stt final transcript")
}

func TestAzureSTT_InterimTranscriptTextOnlyAtDebug(t *testing.T) {
	body, err := json.Marshal(azSpeechHypothesis{Text: piiCanary})
	if err != nil {
		t.Fatalf("marshal hypothesis: %v", err)
	}
	frame := "Path:speech.hypothesis\r\nContent-Type:application/json\r\n\r\n" + string(body)

	// partial must be true: recvLoop skips the hypothesis branch entirely when
	// it is false, which would make both assertions vacuous.
	records := runAzureRecvLoop(t, frame, true)
	assertCanaryOnlyAtDebug(t, records, "azure stt interim transcript")
}
