package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/providercfg"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// speakRecorder logs the order of writes (by first byte) and discards.
type speakRecorder struct {
	mu  sync.Mutex
	ops []string
}

func (r *speakRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.ops = append(r.ops, fmt.Sprintf("audio:%d", p[0]))
	r.mu.Unlock()
	return len(p), nil
}

func (r *speakRecorder) DiscardBuffered() {
	r.mu.Lock()
	r.ops = append(r.ops, "discard")
	r.mu.Unlock()
}

// silenceReader feeds the send loop one silent frame per ptime.
type silenceReader struct{}

func (silenceReader) Read(p []byte) (int, error) {
	time.Sleep(20 * time.Millisecond)
	clear(p)
	return len(p), nil
}

func elAudio(eventID int64, fill byte) string {
	pcm := []byte{fill, 0, fill, 0}
	return fmt.Sprintf(`{"type":"audio","audio_event":{"audio_base_64":%q,"event_id":%d}}`,
		base64.StdEncoding.EncodeToString(pcm), eventID)
}

func TestElevenLabsInterruptionDiscardsBufferedSpeech(t *testing.T) {
	msgs := []string{
		elAudio(1, 1),
		elAudio(2, 2),
		`{"type":"interruption","interruption_event":{"event_id":2}}`,
		elAudio(2, 9), // late chunk of the interrupted response
		elAudio(3, 3),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := wsutil.ReadClientData(conn); err != nil {
			return
		}
		for _, m := range msgs {
			if err := wsutil.WriteServerText(conn, []byte(m)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	s := NewElevenLabs(providercfg.ElevenLabs{BaseURL: srv.URL}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := &speakRecorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Start(ctx, silenceReader{}, rec, "key", Options{AgentID: "a"}, Callbacks{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	want := []string{"audio:1", "audio:2", "discard", "audio:3"}
	if !reflect.DeepEqual(rec.ops, want) {
		t.Fatalf("ops = %v, want %v", rec.ops, want)
	}
}
